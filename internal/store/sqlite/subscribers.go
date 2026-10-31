package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/akkuraina/pehredaar/internal/store"
)

const subscriberColumns = `id, telegram_id, chat_id, language, active, created_at`

func scanSubscriber(sc scanner) (store.Subscriber, error) {
	var (
		sub       store.Subscriber
		createdAt sql.NullInt64
	)
	if err := sc.Scan(&sub.ID, &sub.TelegramID, &sub.ChatID, &sub.Language, &sub.Active, &createdAt); err != nil {
		return store.Subscriber{}, err
	}
	sub.CreatedAt = fromMillis(createdAt)
	return sub, nil
}

func (s *Store) UpsertSubscriber(ctx context.Context, telegramID, chatID int64) (store.Subscriber, error) {
	sub, err := scanSubscriber(s.db.QueryRowContext(ctx, `
		INSERT INTO subscribers (telegram_id, chat_id, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT (telegram_id) DO UPDATE SET
			chat_id = excluded.chat_id,
			active  = 1
		RETURNING `+subscriberColumns,
		telegramID, chatID, toMillis(s.now())))
	if err != nil {
		return store.Subscriber{}, fmt.Errorf("sqlite: upsert subscriber %d: %w", telegramID, err)
	}
	return sub, nil
}

func (s *Store) SetLanguage(ctx context.Context, telegramID int64, lang store.Language) error {
	if lang != store.LangEnglish && lang != store.LangHindi {
		return fmt.Errorf("sqlite: unknown language %q", lang)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE subscribers SET language = ? WHERE telegram_id = ?`, lang, telegramID)
	if err != nil {
		return fmt.Errorf("sqlite: set language for %d: %w", telegramID, err)
	}
	return rowsAffectedOrNotFound(res, fmt.Sprintf("subscriber %d", telegramID))
}

func (s *Store) SetActive(ctx context.Context, telegramID int64, active bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE subscribers SET active = ? WHERE telegram_id = ?`, active, telegramID)
	if err != nil {
		return fmt.Errorf("sqlite: set active for %d: %w", telegramID, err)
	}
	return rowsAffectedOrNotFound(res, fmt.Sprintf("subscriber %d", telegramID))
}

// subscriberID looks up the internal ID for a Telegram user.
func (s *Store) subscriberID(ctx context.Context, telegramID int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM subscribers WHERE telegram_id = ?`, telegramID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("subscriber %d: %w", telegramID, store.ErrNotFound)
	}
	return id, err
}

// targetArgs converts a Target into the (source_id, category) column values,
// with NULL for whichever side is unset.
func targetArgs(t store.Target) (sql.NullInt64, sql.NullString) {
	return nullID(t.SourceID), sql.NullString{String: t.Category, Valid: t.Category != ""}
}

func (s *Store) Subscribe(ctx context.Context, telegramID int64, t store.Target) error {
	if err := t.Validate(); err != nil {
		return err
	}
	subID, err := s.subscriberID(ctx, telegramID)
	if err != nil {
		return err
	}
	sourceID, category := targetArgs(t)
	// DO NOTHING without a conflict target covers both partial unique
	// indexes, which is what makes subscribing idempotent.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO subscriptions (subscriber_id, source_id, category, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		subID, sourceID, category, toMillis(s.now()))
	if err != nil {
		return fmt.Errorf("sqlite: subscribe %d to %+v: %w", telegramID, t, err)
	}
	return nil
}

func (s *Store) Unsubscribe(ctx context.Context, telegramID int64, t store.Target) (bool, error) {
	if err := t.Validate(); err != nil {
		return false, err
	}
	sourceID, category := targetArgs(t)
	// `IS` (rather than `=`) compares NULL to NULL as true in SQLite, so one
	// statement handles both kinds of target.
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM subscriptions
		WHERE subscriber_id = (SELECT id FROM subscribers WHERE telegram_id = ?)
		  AND source_id IS ? AND category IS ?`,
		telegramID, sourceID, category)
	if err != nil {
		return false, fmt.Errorf("sqlite: unsubscribe %d from %+v: %w", telegramID, t, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) Subscriptions(ctx context.Context, telegramID int64) ([]store.Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sub.id, sub.subscriber_id, sub.source_id, sub.category, sub.created_at
		FROM subscriptions sub
		JOIN subscribers s ON s.id = sub.subscriber_id
		WHERE s.telegram_id = ?
		ORDER BY sub.id`, telegramID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: subscriptions of %d: %w", telegramID, err)
	}
	defer rows.Close()

	var out []store.Subscription
	for rows.Next() {
		var (
			sub       store.Subscription
			sourceID  sql.NullInt64
			category  sql.NullString
			createdAt sql.NullInt64
		)
		if err := rows.Scan(&sub.ID, &sub.SubscriberID, &sourceID, &category, &createdAt); err != nil {
			return nil, err
		}
		sub.Target = store.Target{SourceID: sourceID.Int64, Category: category.String}
		sub.CreatedAt = fromMillis(createdAt)
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *Store) SubscribersFor(ctx context.Context, src store.Source) ([]store.Subscriber, error) {
	// DISTINCT because one person may match via both category and source.
	// A ParentID of 0 matches nothing, since real IDs start at 1.
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT s.id, s.telegram_id, s.chat_id, s.language, s.active, s.created_at
		FROM subscribers s
		JOIN subscriptions sub ON sub.subscriber_id = s.id
		WHERE s.active = 1
		  AND (sub.category = ? OR sub.source_id IN (?, ?))
		ORDER BY s.id`,
		src.Category, src.ID, src.ParentID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: subscribers for source %d: %w", src.ID, err)
	}
	defer rows.Close()

	var out []store.Subscriber
	for rows.Next() {
		sub, err := scanSubscriber(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}
