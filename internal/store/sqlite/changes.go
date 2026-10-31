package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/akkuraina/pehredaar/internal/store"
)

func (s *Store) LatestSnapshot(ctx context.Context, sourceID int64) (store.Snapshot, error) {
	var (
		snap      store.Snapshot
		fetchedAt sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, source_id, content_hash, text, fetched_at
		FROM snapshots WHERE source_id = ?
		ORDER BY id DESC LIMIT 1`, sourceID,
	).Scan(&snap.ID, &snap.SourceID, &snap.ContentHash, &snap.Text, &fetchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Snapshot{}, fmt.Errorf("latest snapshot of source %d: %w", sourceID, store.ErrNotFound)
	}
	if err != nil {
		return store.Snapshot{}, fmt.Errorf("sqlite: latest snapshot of source %d: %w", sourceID, err)
	}
	snap.FetchedAt = fromMillis(fetchedAt)
	return snap, nil
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

func (s *Store) UnfinishedChanges(ctx context.Context, limit int) ([]store.Change, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source_id, old_snapshot_id, new_snapshot_id, kind, diff,
		       importance, status, attempts, created_at
		FROM changes
		WHERE status IN ('pending', 'summarized')
		ORDER BY id
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: unfinished changes: %w", err)
	}
	defer rows.Close()

	var out []store.Change
	for rows.Next() {
		var (
			c         store.Change
			oldSnap   sql.NullInt64
			createdAt sql.NullInt64
		)
		if err := rows.Scan(&c.ID, &c.SourceID, &oldSnap, &c.NewSnapshotID, &c.Kind, &c.Diff,
			&c.Importance, &c.Status, &c.Attempts, &createdAt); err != nil {
			return nil, err
		}
		c.OldSnapshotID = oldSnap.Int64
		c.CreatedAt = fromMillis(createdAt)
		out = append(out, c)
	}
	return out, rows.Err()
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

func (s *Store) MarkDelivered(ctx context.Context, changeID, subscriberID int64) (bool, error) {
	// The primary key on (change_id, subscriber_id) makes this race-free:
	// if two goroutines try at once, exactly one insert wins.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO deliveries (change_id, subscriber_id, sent_at)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`,
		changeID, subscriberID, toMillis(s.now()))
	if err != nil {
		return false, fmt.Errorf("sqlite: mark change %d delivered to %d: %w", changeID, subscriberID, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
