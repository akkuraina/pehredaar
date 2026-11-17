package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akkuraina/pehredaar/internal/store"
)

// --- subscribers -----------------------------------------------------------

const subscriberColumns = `id, language, created_at`

func scanSubscriber(sc scanner) (store.Subscriber, error) {
	var (
		sub       store.Subscriber
		createdAt sql.NullInt64
	)
	if err := sc.Scan(&sub.ID, &sub.Language, &createdAt); err != nil {
		return store.Subscriber{}, err
	}
	sub.CreatedAt = fromMillis(createdAt)
	return sub, nil
}

func validLanguage(lang store.Language) error {
	if lang != store.LangEnglish && lang != store.LangHindi {
		return fmt.Errorf("sqlite: unknown language %q", lang)
	}
	return nil
}

func (s *Store) CreateSubscriber(ctx context.Context, lang store.Language) (store.Subscriber, error) {
	if err := validLanguage(lang); err != nil {
		return store.Subscriber{}, err
	}
	sub, err := scanSubscriber(s.db.QueryRowContext(ctx,
		`INSERT INTO subscribers (language, created_at) VALUES (?, ?) RETURNING `+subscriberColumns,
		lang, toMillis(s.now())))
	if err != nil {
		return store.Subscriber{}, fmt.Errorf("sqlite: create subscriber: %w", err)
	}
	return sub, nil
}

func (s *Store) Subscriber(ctx context.Context, id int64) (store.Subscriber, error) {
	sub, err := scanSubscriber(s.db.QueryRowContext(ctx,
		`SELECT `+subscriberColumns+` FROM subscribers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Subscriber{}, fmt.Errorf("subscriber %d: %w", id, store.ErrNotFound)
	}
	return sub, err
}

func (s *Store) SetLanguage(ctx context.Context, subscriberID int64, lang store.Language) error {
	if err := validLanguage(lang); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE subscribers SET language = ? WHERE id = ?`, lang, subscriberID)
	if err != nil {
		return fmt.Errorf("sqlite: set language for %d: %w", subscriberID, err)
	}
	return rowsAffectedOrNotFound(res, fmt.Sprintf("subscriber %d", subscriberID))
}

func (s *Store) DeleteSubscriber(ctx context.Context, id int64) error {
	// ON DELETE CASCADE removes contacts (and with them deliveries),
	// subscriptions, interests and tokens.
	res, err := s.db.ExecContext(ctx, `DELETE FROM subscribers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete subscriber %d: %w", id, err)
	}
	return rowsAffectedOrNotFound(res, fmt.Sprintf("subscriber %d", id))
}

// --- contacts --------------------------------------------------------------

const contactColumns = `id, subscriber_id, channel, address, chat_id, verified_at, active, created_at`

func scanContact(sc scanner, extra ...any) (store.Contact, error) {
	var (
		c                   store.Contact
		chatID              sql.NullInt64
		verified, createdAt sql.NullInt64
	)
	dest := append([]any{&c.ID, &c.SubscriberID, &c.Channel, &c.Address, &chatID,
		&verified, &c.Active, &createdAt}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return store.Contact{}, err
	}
	c.ChatID = chatID.Int64
	c.VerifiedAt = fromMillis(verified)
	c.CreatedAt = fromMillis(createdAt)
	return c, nil
}

// NormalizeEmail is how email addresses are stored and compared. Gmail and
// most providers treat the local part case-insensitively in practice.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (s *Store) Contact(ctx context.Context, id int64) (store.Contact, error) {
	c, err := scanContact(s.db.QueryRowContext(ctx,
		`SELECT `+contactColumns+` FROM contacts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Contact{}, fmt.Errorf("contact %d: %w", id, store.ErrNotFound)
	}
	return c, err
}

func (s *Store) ContactByAddress(ctx context.Context, ch store.Channel, address string) (store.Contact, error) {
	if ch == store.ChannelEmail {
		address = NormalizeEmail(address)
	}
	c, err := scanContact(s.db.QueryRowContext(ctx,
		`SELECT `+contactColumns+` FROM contacts WHERE channel = ? AND address = ?`, ch, address))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Contact{}, fmt.Errorf("%s contact: %w", ch, store.ErrNotFound)
	}
	return c, err
}

func (s *Store) Contacts(ctx context.Context, subscriberID int64) ([]store.Contact, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+contactColumns+` FROM contacts WHERE subscriber_id = ? ORDER BY id`, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: contacts of %d: %w", subscriberID, err)
	}
	defer rows.Close()
	var out []store.Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) TelegramSubscriber(ctx context.Context, telegramID, chatID int64) (store.Subscriber, error) {
	addr := strconv.FormatInt(telegramID, 10)
	var subID int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			UPDATE contacts SET chat_id = ?, active = 1
			WHERE channel = 'telegram' AND address = ?
			RETURNING subscriber_id`, chatID, addr).Scan(&subID)
		if !errors.Is(err, sql.ErrNoRows) {
			return err // found (nil) or a real error
		}
		now := toMillis(s.now())
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO subscribers (created_at) VALUES (?) RETURNING id`, now).Scan(&subID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO contacts (subscriber_id, channel, address, chat_id, verified_at, created_at)
			VALUES (?, 'telegram', ?, ?, ?, ?)`, subID, addr, chatID, now, now)
		return err
	})
	if err != nil {
		return store.Subscriber{}, fmt.Errorf("sqlite: telegram subscriber %d: %w", telegramID, err)
	}
	return s.Subscriber(ctx, subID)
}

func (s *Store) LinkTelegram(ctx context.Context, subscriberID, telegramID, chatID int64) error {
	addr := strconv.FormatInt(telegramID, 10)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := toMillis(s.now())
		var owner int64
		err := tx.QueryRowContext(ctx,
			`SELECT subscriber_id FROM contacts WHERE channel = 'telegram' AND address = ?`, addr).Scan(&owner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `
				INSERT INTO contacts (subscriber_id, channel, address, chat_id, verified_at, created_at)
				VALUES (?, 'telegram', ?, ?, ?, ?)`, subscriberID, addr, chatID, now, now)
			return err
		case err != nil:
			return err
		case owner != subscriberID:
			if err := mergeSubscriber(ctx, tx, owner, subscriberID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE contacts SET chat_id = ?, active = 1
			WHERE channel = 'telegram' AND address = ?`, chatID, addr)
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: link telegram %d to subscriber %d: %w", telegramID, subscriberID, err)
	}
	return nil
}

// mergeSubscriber moves everything of subscriber from into subscriber into
// and deletes from. Subscriptions and interests are unioned; deliveries
// belong to contacts, so they move with them and no alert is repeated.
func mergeSubscriber(ctx context.Context, tx *sql.Tx, from, into int64) error {
	stmts := []string{
		`UPDATE contacts SET subscriber_id = ?2 WHERE subscriber_id = ?1`,
		`INSERT INTO subscriptions (subscriber_id, source_id, category, created_at)
		 SELECT ?2, source_id, category, created_at FROM subscriptions WHERE subscriber_id = ?1
		 ON CONFLICT DO NOTHING`,
		`INSERT INTO interests (subscriber_id, exam, created_at)
		 SELECT ?2, exam, created_at FROM interests WHERE subscriber_id = ?1
		 ON CONFLICT DO NOTHING`,
		// Cascades to the old subscriptions, interests and tokens.
		`DELETE FROM subscribers WHERE id = ?1`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q, from, into); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) AddEmail(ctx context.Context, subscriberID int64, email string) (store.Contact, error) {
	addr := NormalizeEmail(email)
	if !strings.Contains(addr, "@") {
		return store.Contact{}, fmt.Errorf("sqlite: %q is not an email address", email)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO contacts (subscriber_id, channel, address, created_at)
		VALUES (?, 'email', ?, ?)
		ON CONFLICT (channel, address) DO NOTHING`, subscriberID, addr, toMillis(s.now()))
	if err != nil {
		return store.Contact{}, fmt.Errorf("sqlite: add email: %w", err)
	}
	return s.ContactByAddress(ctx, store.ChannelEmail, addr)
}

func (s *Store) VerifyContact(ctx context.Context, contactID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE contacts SET verified_at = COALESCE(verified_at, ?), active = 1 WHERE id = ?`,
		toMillis(s.now()), contactID)
	if err != nil {
		return fmt.Errorf("sqlite: verify contact %d: %w", contactID, err)
	}
	return rowsAffectedOrNotFound(res, fmt.Sprintf("contact %d", contactID))
}

func (s *Store) SetContactActive(ctx context.Context, contactID int64, active bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE contacts SET active = ? WHERE id = ?`, active, contactID)
	if err != nil {
		return fmt.Errorf("sqlite: set contact %d active: %w", contactID, err)
	}
	return rowsAffectedOrNotFound(res, fmt.Sprintf("contact %d", contactID))
}

// --- subscriptions and interests -------------------------------------------

// targetArgs converts a Target into the (source_id, category) column values,
// with NULL for whichever side is unset.
func targetArgs(t store.Target) (sql.NullInt64, sql.NullString) {
	return nullID(t.SourceID), sql.NullString{String: t.Category, Valid: t.Category != ""}
}

func (s *Store) Subscribe(ctx context.Context, subscriberID int64, t store.Target) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if _, err := s.Subscriber(ctx, subscriberID); err != nil {
		return err
	}
	sourceID, category := targetArgs(t)
	// DO NOTHING without a conflict target covers both partial unique
	// indexes, which is what makes subscribing idempotent.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO subscriptions (subscriber_id, source_id, category, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		subscriberID, sourceID, category, toMillis(s.now()))
	if err != nil {
		return fmt.Errorf("sqlite: subscribe %d to %+v: %w", subscriberID, t, err)
	}
	return nil
}

func (s *Store) Unsubscribe(ctx context.Context, subscriberID int64, t store.Target) (bool, error) {
	if err := t.Validate(); err != nil {
		return false, err
	}
	sourceID, category := targetArgs(t)
	// `IS` (rather than `=`) compares NULL to NULL as true in SQLite, so one
	// statement handles both kinds of target.
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM subscriptions
		WHERE subscriber_id = ? AND source_id IS ? AND category IS ?`,
		subscriberID, sourceID, category)
	if err != nil {
		return false, fmt.Errorf("sqlite: unsubscribe %d from %+v: %w", subscriberID, t, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) Subscriptions(ctx context.Context, subscriberID int64) ([]store.Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, subscriber_id, source_id, category, created_at
		FROM subscriptions WHERE subscriber_id = ? ORDER BY id`, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: subscriptions of %d: %w", subscriberID, err)
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

func (s *Store) SetInterests(ctx context.Context, subscriberID int64, exams []string) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM interests WHERE subscriber_id = ?`, subscriberID); err != nil {
			return err
		}
		now := toMillis(s.now())
		for _, e := range exams {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO interests (subscriber_id, exam, created_at) VALUES (?, ?, ?)
				ON CONFLICT DO NOTHING`, subscriberID, e, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sqlite: set interests of %d: %w", subscriberID, err)
	}
	return nil
}

func (s *Store) Interests(ctx context.Context, subscriberID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT exam FROM interests WHERE subscriber_id = ? ORDER BY exam`, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: interests of %d: %w", subscriberID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) RecipientsFor(ctx context.Context, src store.Source) ([]store.Recipient, error) {
	// DISTINCT because one person may match via both category and source.
	// A ParentID of 0 matches nothing, since real IDs start at 1.
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT `+prefixed("c", contactColumns)+`, s.language
		FROM contacts c
		JOIN subscribers s     ON s.id = c.subscriber_id
		JOIN subscriptions sub ON sub.subscriber_id = s.id
		WHERE c.active = 1 AND c.verified_at IS NOT NULL
		  AND (sub.category = ? OR sub.source_id IN (?, ?))
		ORDER BY c.id`,
		src.Category, src.ID, src.ParentID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: recipients for source %d: %w", src.ID, err)
	}
	defer rows.Close()

	var out []store.Recipient
	for rows.Next() {
		var r store.Recipient
		if r.Contact, err = scanContact(rows, &r.Language); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- tokens and settings ---------------------------------------------------

func (s *Store) CreateToken(ctx context.Context, t store.Token) error {
	if t.Hash == "" || t.SubscriberID == 0 || t.ExpiresAt.IsZero() {
		return fmt.Errorf("sqlite: incomplete token %+v", t)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tokens (hash, purpose, subscriber_id, contact_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.Hash, t.Purpose, t.SubscriberID, nullID(t.ContactID), toMillis(t.ExpiresAt), toMillis(s.now()))
	if err != nil {
		return fmt.Errorf("sqlite: create token: %w", err)
	}
	// Opportunistic cleanup, so the table doesn't grow forever. A token
	// expired a day ago is of no use to anyone, even for debugging.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM tokens WHERE expires_at < ?`,
		toMillis(s.now().Add(-24*time.Hour)))
	return nil
}

func (s *Store) ConsumeToken(ctx context.Context, hash string, purpose store.TokenPurpose, now time.Time) (store.Token, error) {
	// One UPDATE both checks and spends the token, so two clicks racing on
	// the same link can't both succeed.
	t := store.Token{Hash: hash, Purpose: purpose}
	var (
		contactID sql.NullInt64
		expires   sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		UPDATE tokens SET used_at = ?
		WHERE hash = ? AND purpose = ? AND used_at IS NULL AND expires_at > ?
		RETURNING subscriber_id, contact_id, expires_at`,
		toMillis(now), hash, purpose, toMillis(now)).Scan(&t.SubscriberID, &contactID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Token{}, fmt.Errorf("token: %w", store.ErrNotFound)
	}
	if err != nil {
		return store.Token{}, fmt.Errorf("sqlite: consume token: %w", err)
	}
	t.ContactID = contactID.Int64
	t.ExpiresAt = fromMillis(expires)
	return t, nil
}

func (s *Store) Setting(ctx context.Context, key, init string) (string, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT DO NOTHING`, key, init); err != nil {
		return "", fmt.Errorf("sqlite: setting %s: %w", key, err)
	}
	var v string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return "", fmt.Errorf("sqlite: setting %s: %w", key, err)
	}
	return v, nil
}
