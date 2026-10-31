package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akkuraina/pehredaar/internal/store"
)

const sourceColumns = `id, url, kind, category, language, label, parent_id,
	check_interval_sec, enabled, etag, last_modified, last_checked_at,
	next_check_at, consecutive_failures, last_error, created_at`

func scanSource(sc scanner) (store.Source, error) {
	var (
		s                                 store.Source
		parentID                          sql.NullInt64
		intervalSec                       int64
		lastChecked, nextCheck, createdAt sql.NullInt64
	)
	err := sc.Scan(&s.ID, &s.URL, &s.Kind, &s.Category, &s.Language, &s.Label, &parentID,
		&intervalSec, &s.Enabled, &s.ETag, &s.LastModified, &lastChecked,
		&nextCheck, &s.ConsecutiveFailures, &s.LastError, &createdAt)
	if err != nil {
		return store.Source{}, err
	}
	s.ParentID = parentID.Int64
	s.CheckInterval = time.Duration(intervalSec) * time.Second
	s.LastCheckedAt = fromMillis(lastChecked)
	s.NextCheckAt = fromMillis(nextCheck)
	s.CreatedAt = fromMillis(createdAt)
	return s, nil
}

func validateSource(s store.Source) error {
	switch {
	case s.URL == "":
		return errors.New("sqlite: source URL is empty")
	case s.Kind != store.KindFeed && s.Kind != store.KindPDF && s.Kind != store.KindHTML:
		return fmt.Errorf("sqlite: source %s: unknown kind %q", s.URL, s.Kind)
	case s.Language != store.LangEnglish && s.Language != store.LangHindi:
		return fmt.Errorf("sqlite: source %s: unknown language %q", s.URL, s.Language)
	case s.Category == "":
		return fmt.Errorf("sqlite: source %s: category is empty", s.URL)
	case s.CheckInterval < time.Second:
		return fmt.Errorf("sqlite: source %s: check interval %v is under 1s", s.URL, s.CheckInterval)
	}
	return nil
}

func (s *Store) UpsertSource(ctx context.Context, src store.Source) (store.Source, error) {
	if err := validateSource(src); err != nil {
		return store.Source{}, err
	}
	// ON CONFLICT ... DO UPDATE ("upsert") touches only the curated columns,
	// so re-syncing sources.yaml never resets ETags, schedules or failure
	// counts. `excluded` refers to the row we tried to insert.
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO sources (url, kind, category, language, label, parent_id,
		                     check_interval_sec, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (url) DO UPDATE SET
			kind               = excluded.kind,
			category           = excluded.category,
			language           = excluded.language,
			label              = excluded.label,
			parent_id          = excluded.parent_id,
			check_interval_sec = excluded.check_interval_sec,
			enabled            = excluded.enabled
		RETURNING id`,
		src.URL, src.Kind, src.Category, src.Language, src.Label, nullID(src.ParentID),
		int64(src.CheckInterval/time.Second), src.Enabled, toMillis(s.now()),
	).Scan(&id)
	if err != nil {
		return store.Source{}, fmt.Errorf("sqlite: upsert source %s: %w", src.URL, err)
	}
	return s.Source(ctx, id)
}

func (s *Store) Source(ctx context.Context, id int64) (store.Source, error) {
	src, err := scanSource(s.db.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Source{}, fmt.Errorf("source %d: %w", id, store.ErrNotFound)
	}
	return src, err
}

func (s *Store) DueSources(ctx context.Context, now time.Time, limit int) ([]store.Source, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+sourceColumns+` FROM sources
		WHERE enabled = 1 AND (next_check_at IS NULL OR next_check_at <= ?)
		ORDER BY COALESCE(next_check_at, 0), id
		LIMIT ?`, now.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: due sources: %w", err)
	}
	defer rows.Close()

	var out []store.Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	// rows.Err reports errors that ended iteration early (e.g. a dropped
	// connection); forgetting it is a classic database/sql bug.
	return out, rows.Err()
}

func (s *Store) RecordCheck(ctx context.Context, sourceID int64, r store.CheckResult) error {
	var (
		res sql.Result
		err error
	)
	if r.Err != "" {
		res, err = s.db.ExecContext(ctx, `
			UPDATE sources SET
				last_checked_at      = ?,
				next_check_at        = ?,
				consecutive_failures = consecutive_failures + 1,
				last_error           = ?
			WHERE id = ?`,
			toMillis(r.CheckedAt), toMillis(r.NextCheckAt), r.Err, sourceID)
	} else {
		res, err = s.db.ExecContext(ctx, `
			UPDATE sources SET
				last_checked_at      = ?,
				next_check_at        = ?,
				etag                 = ?,
				last_modified        = ?,
				consecutive_failures = 0,
				last_error           = ''
			WHERE id = ?`,
			toMillis(r.CheckedAt), toMillis(r.NextCheckAt), r.ETag, r.LastModified, sourceID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: record check for source %d: %w", sourceID, err)
	}
	return rowsAffectedOrNotFound(res, fmt.Sprintf("source %d", sourceID))
}

func (s *Store) DisableCuratedExcept(ctx context.Context, keepURLs []string) (int64, error) {
	query := `UPDATE sources SET enabled = 0 WHERE parent_id IS NULL AND enabled = 1`
	args := make([]any, len(keepURLs))
	if len(keepURLs) > 0 {
		// database/sql has no way to bind a slice, so we generate one "?"
		// per URL. Still parameterized, so still safe from injection.
		query += ` AND url NOT IN (?` + strings.Repeat(", ?", len(keepURLs)-1) + `)`
		for i, u := range keepURLs {
			args[i] = u
		}
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("sqlite: disable curated sources: %w", err)
	}
	return res.RowsAffected()
}

func (s *Store) Categories(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT category FROM sources WHERE enabled = 1 ORDER BY category`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: categories: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
