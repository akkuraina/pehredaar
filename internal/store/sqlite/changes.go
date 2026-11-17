package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/akkuraina/pehredaar/internal/store"
)

func scanSnapshot(sc scanner) (store.Snapshot, error) {
	var (
		snap      store.Snapshot
		fetchedAt sql.NullInt64
	)
	if err := sc.Scan(&snap.ID, &snap.SourceID, &snap.ContentHash, &snap.Text, &fetchedAt); err != nil {
		return store.Snapshot{}, err
	}
	snap.FetchedAt = fromMillis(fetchedAt)
	return snap, nil
}

func (s *Store) LatestSnapshot(ctx context.Context, sourceID int64) (store.Snapshot, error) {
	snap, err := scanSnapshot(s.db.QueryRowContext(ctx, `
		SELECT id, source_id, content_hash, text, fetched_at
		FROM snapshots WHERE source_id = ?
		ORDER BY id DESC LIMIT 1`, sourceID))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Snapshot{}, fmt.Errorf("latest snapshot of source %d: %w", sourceID, store.ErrNotFound)
	}
	if err != nil {
		return store.Snapshot{}, fmt.Errorf("sqlite: latest snapshot of source %d: %w", sourceID, err)
	}
	return snap, nil
}

func (s *Store) Snapshot(ctx context.Context, id int64) (store.Snapshot, error) {
	snap, err := scanSnapshot(s.db.QueryRowContext(ctx, `
		SELECT id, source_id, content_hash, text, fetched_at
		FROM snapshots WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Snapshot{}, fmt.Errorf("snapshot %d: %w", id, store.ErrNotFound)
	}
	return snap, err
}

func (s *Store) Snapshots(ctx context.Context, sourceID int64, limit int) ([]store.Snapshot, error) {
	// Selecting '' for text keeps scanSnapshot reusable without reading
	// large documents we won't show.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source_id, content_hash, '', fetched_at
		FROM snapshots WHERE source_id = ?
		ORDER BY id DESC LIMIT ?`, sourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: snapshots of source %d: %w", sourceID, err)
	}
	defer rows.Close()

	var out []store.Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

func (s *Store) SaveSnapshot(ctx context.Context, snap store.Snapshot, change *store.Change) (store.Snapshot, *store.Change, error) {
	if change != nil && change.SourceID != 0 && change.SourceID != snap.SourceID {
		return store.Snapshot{}, nil, fmt.Errorf("sqlite: change source %d does not match snapshot source %d",
			change.SourceID, snap.SourceID)
	}
	if snap.FetchedAt.IsZero() {
		snap.FetchedAt = s.now()
	}

	// Copy rather than mutate the caller's change: the function's result is
	// the only output, which keeps the data flow easy to follow.
	var saved *store.Change
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO snapshots (source_id, content_hash, text, fetched_at)
			VALUES (?, ?, ?, ?) RETURNING id`,
			snap.SourceID, snap.ContentHash, snap.Text, toMillis(snap.FetchedAt),
		).Scan(&snap.ID); err != nil {
			return fmt.Errorf("insert snapshot: %w", err)
		}
		if change == nil {
			return nil
		}

		c := *change
		c.SourceID = snap.SourceID
		c.NewSnapshotID = snap.ID
		if c.Status == "" {
			c.Status = store.StatusPending
		}
		c.CreatedAt = s.now()
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO changes (source_id, old_snapshot_id, new_snapshot_id, kind,
			                     diff, importance, status, attempts, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			c.SourceID, nullID(c.OldSnapshotID), c.NewSnapshotID, c.Kind,
			c.Diff, c.Importance, c.Status, c.Attempts, toMillis(c.CreatedAt),
		).Scan(&c.ID); err != nil {
			return fmt.Errorf("insert change: %w", err)
		}
		saved = &c
		return nil
	})
	if err != nil {
		return store.Snapshot{}, nil, fmt.Errorf("sqlite: save snapshot for source %d: %w", snap.SourceID, err)
	}
	return snap, saved, nil
}

const changeColumns = `id, source_id, old_snapshot_id, new_snapshot_id, kind, diff,
	importance, status, attempts, created_at`

// changeRow holds the scan targets for changeColumns. Splitting "where to
// scan into" (dest) from "build the value" (value) lets one row hold a
// change and its source together (see FeedChanges).
type changeRow struct {
	c         store.Change
	oldSnap   sql.NullInt64
	createdAt sql.NullInt64
}

func (r *changeRow) dest() []any {
	return []any{&r.c.ID, &r.c.SourceID, &r.oldSnap, &r.c.NewSnapshotID, &r.c.Kind, &r.c.Diff,
		&r.c.Importance, &r.c.Status, &r.c.Attempts, &r.createdAt}
}

func (r *changeRow) value() store.Change {
	r.c.OldSnapshotID = r.oldSnap.Int64
	r.c.CreatedAt = fromMillis(r.createdAt)
	return r.c
}

func scanChange(sc scanner) (store.Change, error) {
	var r changeRow
	if err := sc.Scan(r.dest()...); err != nil {
		return store.Change{}, err
	}
	return r.value(), nil
}

func scanFeedRow(sc scanner, e *store.FeedEntry) error {
	var (
		cr changeRow
		sr sourceRow
	)
	dest := append(cr.dest(), sr.dest()...)
	dest = append(dest, &e.Summary)
	if err := sc.Scan(dest...); err != nil {
		return err
	}
	e.Change, e.Source = cr.value(), sr.value()
	return nil
}

// prefixed qualifies a column list with a table alias:
// prefixed("s", "id, url") is "s.id, s.url".
func prefixed(alias, columns string) string {
	cols := strings.Split(columns, ",")
	for i, c := range cols {
		cols[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}

// queryChanges runs a SELECT of changeColumns and scans every row.
func (s *Store) queryChanges(ctx context.Context, query string, args ...any) ([]store.Change, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+changeColumns+` FROM changes `+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Change
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UnfinishedChanges(ctx context.Context, limit int) ([]store.Change, error) {
	out, err := s.queryChanges(ctx,
		`WHERE status IN ('pending', 'summarized') ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: unfinished changes: %w", err)
	}
	return out, nil
}

func (s *Store) Change(ctx context.Context, id int64) (store.Change, error) {
	c, err := scanChange(s.db.QueryRowContext(ctx,
		`SELECT `+changeColumns+` FROM changes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Change{}, fmt.Errorf("change %d: %w", id, store.ErrNotFound)
	}
	return c, err
}

func (s *Store) RecentChanges(ctx context.Context, sourceID int64, limit int) ([]store.Change, error) {
	// "? = 0 OR source_id = ?" keeps one query for both cases; SQLite still
	// uses the index when a source is given.
	out, err := s.queryChanges(ctx,
		`WHERE ? = 0 OR source_id = ? ORDER BY id DESC LIMIT ?`, sourceID, sourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: recent changes: %w", err)
	}
	return out, nil
}

func (s *Store) UpdateChange(ctx context.Context, c store.Change) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE changes SET importance = ?, status = ?, attempts = ? WHERE id = ?`,
		c.Importance, c.Status, c.Attempts, c.ID)
	if err != nil {
		return fmt.Errorf("sqlite: update change %d: %w", c.ID, err)
	}
	return rowsAffectedOrNotFound(res, fmt.Sprintf("change %d", c.ID))
}

func (s *Store) SaveSummary(ctx context.Context, changeID int64, lang store.Language, text string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO change_summaries (change_id, language, text, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (change_id, language) DO UPDATE SET text = excluded.text`,
		changeID, lang, text, toMillis(s.now()))
	if err != nil {
		return fmt.Errorf("sqlite: save %s summary for change %d: %w", lang, changeID, err)
	}
	return nil
}

func (s *Store) Summary(ctx context.Context, changeID int64, lang store.Language) (string, error) {
	var text string
	err := s.db.QueryRowContext(ctx,
		`SELECT text FROM change_summaries WHERE change_id = ? AND language = ?`,
		changeID, lang).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%s summary of change %d: %w", lang, changeID, store.ErrNotFound)
	}
	return text, err
}

func (s *Store) MarkDelivered(ctx context.Context, changeID, contactID int64) (bool, error) {
	// The primary key on (change_id, contact_id) makes this race-free:
	// if two goroutines try at once, exactly one insert wins.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO deliveries (change_id, contact_id, sent_at)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`,
		changeID, contactID, toMillis(s.now()))
	if err != nil {
		return false, fmt.Errorf("sqlite: mark change %d delivered to contact %d: %w", changeID, contactID, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) FeedChanges(ctx context.Context, q store.FeedQuery) ([]store.FeedEntry, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	lang := q.Lang
	if lang == "" {
		lang = store.LangEnglish
	}
	where := []string{
		`c.status NOT IN ('suppressed', 'failed')`,
		`c.importance != 'cosmetic'`,
		`c.id > ?`,
	}
	args := []any{lang, q.AfterID}
	if q.BeforeID > 0 {
		where = append(where, `c.id < ?`)
		args = append(args, q.BeforeID)
	}
	if len(q.Categories) > 0 {
		where = append(where, `src.category IN (?`+strings.Repeat(", ?", len(q.Categories)-1)+`)`)
		for _, c := range q.Categories {
			args = append(args, c)
		}
	}
	args = append(args, q.Limit)

	// The summary in the wanted language, else English, else none. Two
	// LEFT JOINs and COALESCE do that in one query.
	cols := prefixed("c", changeColumns)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+cols+`, `+prefixed("src", sourceColumns)+`,
		       COALESCE(want.text, en.text, '')
		FROM changes c
		JOIN sources src ON src.id = c.source_id
		LEFT JOIN change_summaries want ON want.change_id = c.id AND want.language = ?
		LEFT JOIN change_summaries en   ON en.change_id = c.id AND en.language = 'en'
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY c.id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: feed changes: %w", err)
	}
	defer rows.Close()

	var out []store.FeedEntry
	for rows.Next() {
		var e store.FeedEntry
		if err := scanFeedRow(rows, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Stats(ctx context.Context) (store.Stats, error) {
	var (
		st        store.Stats
		lastCheck sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM sources WHERE enabled = 1),
			(SELECT COUNT(*) FROM sources WHERE enabled = 1 AND parent_id IS NULL),
			(SELECT COUNT(*) FROM changes WHERE importance != 'cosmetic' AND status NOT IN ('suppressed', 'failed')),
			(SELECT MAX(last_checked_at) FROM sources)`).
		Scan(&st.Sources, &st.Feeds, &st.ChangesCaught, &lastCheck)
	if err != nil {
		return store.Stats{}, fmt.Errorf("sqlite: stats: %w", err)
	}
	st.LastCheckAt = fromMillis(lastCheck)
	return st, nil
}
