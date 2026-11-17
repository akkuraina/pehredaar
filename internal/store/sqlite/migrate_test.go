package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/akkuraina/pehredaar/internal/store"
)

// TestContactsMigrationKeepsData builds a database as it was before
// migration 3 (subscribers were Telegram accounts), then opens it with the
// current code and checks that people, subscriptions and delivery records
// all survived the table rebuild.
func TestContactsMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := raw.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`)
	for i, name := range []string{"0001_init.sql", "0002_disabled_reason.sql"} {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
		exec(`INSERT INTO schema_migrations VALUES (?, 0)`, i+1)
	}
	exec(`INSERT INTO sources (id, url, kind, category, language, check_interval_sec, created_at)
	      VALUES (1, 'https://example.test/f', 'feed', 'ssc:results', 'en', 900, 0)`)
	exec(`INSERT INTO snapshots (id, source_id, content_hash, text, fetched_at) VALUES (1, 1, 'h', 't', 0)`)
	exec(`INSERT INTO changes (id, source_id, new_snapshot_id, kind, diff, created_at)
	      VALUES (1, 1, 1, 'added', '+ x', 0)`)
	exec(`INSERT INTO subscribers (id, telegram_id, chat_id, language, active, created_at)
	      VALUES (5, 111, 222, 'hi', 0, 1000)`)
	exec(`INSERT INTO subscriptions (subscriber_id, category, created_at) VALUES (5, 'ssc:results', 0)`)
	exec(`INSERT INTO deliveries (change_id, subscriber_id, sent_at) VALUES (1, 5, 0)`)
	raw.Close()

	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	sub, err := s.Subscriber(ctx, 5)
	if err != nil || sub.Language != store.LangHindi {
		t.Fatalf("subscriber after migration = %+v, %v", sub, err)
	}
	c, err := s.ContactByAddress(ctx, store.ChannelTelegram, "111")
	if err != nil {
		t.Fatal(err)
	}
	if c.SubscriberID != 5 || c.ChatID != 222 || c.Active || !c.Verified() {
		t.Errorf("contact = %+v; want subscriber 5, chat 222, inactive (blocked), verified", c)
	}
	if subs, _ := s.Subscriptions(ctx, 5); len(subs) != 1 {
		t.Errorf("subscriptions = %+v, want the one we had", subs)
	}
	if again, err := s.MarkDelivered(ctx, 1, c.ID); err != nil || again {
		t.Errorf("MarkDelivered = %v, %v; the old delivery should have moved to the contact", again, err)
	}
	// Foreign keys must be back on for normal use: a contact for a
	// missing subscriber is rejected.
	if _, err := s.db.Exec(`INSERT INTO contacts (subscriber_id, channel, address, created_at)
	                         VALUES (999, 'email', 'x@y.test', 0)`); err == nil {
		t.Error("foreign keys are off after the migration")
	}
}
