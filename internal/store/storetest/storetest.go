// Package storetest is a conformance suite for store.Store implementations.
// Every implementation (SQLite now, Postgres later) runs the same tests, which
// is how we know the interface really is swappable.
//
// Go note: a non-_test.go file importing "testing" is fine for packages that
// exist only to support tests; the standard library does the same in
// net/http/httptest and testing/fstest.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/akkuraina/pehredaar/internal/store"
)

// Opener returns a fresh, empty store. It should register its own cleanup
// with t.Cleanup.
type Opener func(t *testing.T) store.Store

// Run runs the full conformance suite against stores produced by open.
func Run(t *testing.T, open Opener) {
	tests := []struct {
		name string
		fn   func(*testing.T, store.Store)
	}{
		{"UpsertSourceInsertsAndUpdates", testUpsertSource},
		{"UpsertSourceValidates", testUpsertSourceValidates},
		{"SourceNotFound", testSourceNotFound},
		{"DueSources", testDueSources},
		{"RecordCheck", testRecordCheck},
		{"DisableCuratedExcept", testDisableCuratedExcept},
		{"DisableSourceIsStickyForDiscovered", testDisableSourceSticky},
		{"RetireDiscovered", testRetireDiscovered},
		{"Reschedule", testReschedule},
		{"Categories", testCategories},
		{"Snapshots", testSnapshots},
		{"SaveSnapshotIsAtomic", testSaveSnapshotAtomic},
		{"ReadQueries", testReadQueries},
		{"ChangeLifecycle", testChangeLifecycle},
		{"Summaries", testSummaries},
		{"Subscribers", testSubscribers},
		{"Subscriptions", testSubscriptions},
		{"EmailContacts", testEmailContacts},
		{"LinkTelegramMerges", testLinkTelegramMerges},
		{"Interests", testInterests},
		{"InterestsKeepCreatedAt", testInterestsKeepCreatedAt},
		{"Launches", testLaunches},
		{"DeleteSubscriber", testDeleteSubscriber},
		{"Tokens", testTokens},
		{"Setting", testSetting},
		{"FeedChanges", testFeedChanges},
		{"RecipientsFor", testRecipientsFor},
		{"MarkDelivered", testMarkDelivered},
		{"MarkDeliveredConcurrent", testMarkDeliveredConcurrent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.fn(t, open(t))
		})
	}
}

// --- helpers ---------------------------------------------------------------

func ctx(t *testing.T) context.Context {
	// t.Context (Go 1.24+) is cancelled when the test finishes.
	return t.Context()
}

func mustSource(t *testing.T, s store.Store, url, category string) store.Source {
	t.Helper() // makes failures point at the caller's line, not this one
	src, err := s.UpsertSource(ctx(t), store.Source{
		URL:           url,
		Kind:          store.KindFeed,
		Category:      category,
		Language:      store.LangEnglish,
		Label:         "label for " + url,
		CheckInterval: 15 * time.Minute,
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("UpsertSource(%s): %v", url, err)
	}
	return src
}

// mustSubscriber registers a Telegram user (chat id = 10x the user id).
func mustSubscriber(t *testing.T, s store.Store, telegramID int64) store.Subscriber {
	t.Helper()
	sub, err := s.TelegramSubscriber(ctx(t), telegramID, telegramID*10)
	if err != nil {
		t.Fatalf("TelegramSubscriber(%d): %v", telegramID, err)
	}
	return sub
}

func telegramContact(t *testing.T, s store.Store, telegramID int64) store.Contact {
	t.Helper()
	c, err := s.ContactByAddress(ctx(t), store.ChannelTelegram, fmt.Sprint(telegramID))
	if err != nil {
		t.Fatalf("telegram contact %d: %v", telegramID, err)
	}
	return c
}

func mustChange(t *testing.T, s store.Store, src store.Source) *store.Change {
	t.Helper()
	_, ch, err := s.SaveSnapshot(ctx(t),
		store.Snapshot{SourceID: src.ID, ContentHash: "h", Text: "t"},
		&store.Change{Kind: store.ChangeAdded, Diff: "+ new notice"})
	if err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	return ch
}

func contactIDs(rs []store.Recipient) []int64 {
	ids := make([]int64, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
	}
	return ids
}

func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- sources ---------------------------------------------------------------

func testUpsertSource(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/a", "ssc:notice-board")
	if src.ID == 0 || !src.Enabled || src.CheckInterval != 15*time.Minute || src.CreatedAt.IsZero() {
		t.Fatalf("unexpected inserted source: %+v", src)
	}

	// Give it polling state, then re-upsert with new curated fields.
	if err := s.RecordCheck(ctx(t), src.ID, store.CheckResult{
		CheckedAt: time.Now(), NextCheckAt: time.Now().Add(time.Hour), ETag: `W/"1"`,
	}); err != nil {
		t.Fatal(err)
	}
	src.Category = "ssc:calendar"
	src.CheckInterval = time.Hour
	got, err := s.UpsertSource(ctx(t), src)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != src.ID {
		t.Errorf("upsert created a new row: id %d, want %d", got.ID, src.ID)
	}
	if got.Category != "ssc:calendar" || got.CheckInterval != time.Hour {
		t.Errorf("curated fields not updated: %+v", got)
	}
	if got.ETag != `W/"1"` || got.NextCheckAt.IsZero() {
		t.Errorf("polling state was overwritten by upsert: %+v", got)
	}
}

func testUpsertSourceValidates(t *testing.T, s store.Store) {
	good := store.Source{URL: "https://example.test/x", Kind: store.KindPDF, Category: "c",
		Language: store.LangHindi, CheckInterval: time.Minute}
	bad := map[string]func(*store.Source){
		"empty url":    func(s *store.Source) { s.URL = "" },
		"bad kind":     func(s *store.Source) { s.Kind = "rss" },
		"bad language": func(s *store.Source) { s.Language = "fr" },
		"no category":  func(s *store.Source) { s.Category = "" },
		"no interval":  func(s *store.Source) { s.CheckInterval = 0 },
	}
	for name, mutate := range bad {
		src := good
		mutate(&src)
		if _, err := s.UpsertSource(ctx(t), src); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := s.UpsertSource(ctx(t), good); err != nil {
		t.Errorf("valid source rejected: %v", err)
	}
}

func testSourceNotFound(t *testing.T, s store.Store) {
	if _, err := s.Source(ctx(t), 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Source(999) error = %v, want ErrNotFound", err)
	}
	if err := s.RecordCheck(ctx(t), 999, store.CheckResult{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RecordCheck(999) error = %v, want ErrNotFound", err)
	}
}

func testDueSources(t *testing.T, s store.Store) {
	now := time.Now()
	fresh := mustSource(t, s, "https://example.test/fresh", "c") // never checked: due
	overdue := mustSource(t, s, "https://example.test/overdue", "c")
	later := mustSource(t, s, "https://example.test/later", "c")
	disabled := mustSource(t, s, "https://example.test/disabled", "c")

	record := func(id int64, next time.Time) {
		t.Helper()
		if err := s.RecordCheck(ctx(t), id, store.CheckResult{CheckedAt: now, NextCheckAt: next}); err != nil {
			t.Fatal(err)
		}
	}
	record(overdue.ID, now.Add(-time.Hour))
	record(later.ID, now.Add(time.Hour))
	disabled.Enabled = false
	if _, err := s.UpsertSource(ctx(t), disabled); err != nil {
		t.Fatal(err)
	}

	due, err := s.DueSources(ctx(t), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, d := range due {
		got = append(got, d.ID)
	}
	// Never-checked first (treated as most overdue), then by next_check_at.
	if want := []int64{fresh.ID, overdue.ID}; !equalInts(got, want) {
		t.Errorf("due = %v, want %v", got, want)
	}

	if due, _ := s.DueSources(ctx(t), now, 1); len(due) != 1 {
		t.Errorf("limit 1 returned %d sources", len(due))
	}
}

func testRecordCheck(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/rc", "c")
	now := time.Now().Truncate(time.Millisecond)

	if err := s.RecordCheck(ctx(t), src.ID, store.CheckResult{
		CheckedAt: now, NextCheckAt: now.Add(time.Minute), ETag: "e1", LastModified: "lm1",
	}); err != nil {
		t.Fatal(err)
	}
	// Two failures: validators kept, counter climbs.
	for range 2 {
		if err := s.RecordCheck(ctx(t), src.ID, store.CheckResult{
			CheckedAt: now, NextCheckAt: now.Add(time.Hour), Err: "timeout",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Source(ctx(t), src.ID)
	if got.ConsecutiveFailures != 2 || got.LastError != "timeout" || got.ETag != "e1" || got.LastModified != "lm1" {
		t.Errorf("after failures: %+v", got)
	}
	if !got.LastCheckedAt.Equal(now) {
		t.Errorf("LastCheckedAt = %v, want %v", got.LastCheckedAt, now)
	}

	// Success resets the failure state.
	if err := s.RecordCheck(ctx(t), src.ID, store.CheckResult{
		CheckedAt: now, NextCheckAt: now.Add(time.Minute), ETag: "e2",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Source(ctx(t), src.ID)
	if got.ConsecutiveFailures != 0 || got.LastError != "" || got.ETag != "e2" {
		t.Errorf("after success: %+v", got)
	}
}

func testDisableCuratedExcept(t *testing.T, s store.Store) {
	keep := mustSource(t, s, "https://example.test/keep", "c")
	drop := mustSource(t, s, "https://example.test/drop", "c")
	child, err := s.UpsertSource(ctx(t), store.Source{
		URL: "https://example.test/drop/notice.pdf", Kind: store.KindPDF, Category: "c",
		Language: store.LangEnglish, ParentID: drop.ID, CheckInterval: time.Hour, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	n, err := s.DisableCuratedExcept(ctx(t), []string{keep.URL})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("disabled %d sources, want 1", n)
	}
	for _, c := range []struct {
		id   int64
		want bool
	}{{keep.ID, true}, {drop.ID, false}, {child.ID, true}} {
		got, _ := s.Source(ctx(t), c.id)
		if got.Enabled != c.want {
			t.Errorf("source %s enabled = %v, want %v", got.URL, got.Enabled, c.want)
		}
	}
	if got, _ := s.Source(ctx(t), drop.ID); got.DisabledReason != store.DisabledRemoved {
		t.Errorf("dropped source reason = %q, want %q", got.DisabledReason, store.DisabledRemoved)
	}
}

func mustPDF(t *testing.T, s store.Store, url string, parent int64) store.Source {
	t.Helper()
	src, err := s.UpsertSource(ctx(t), store.Source{
		URL: url, Kind: store.KindPDF, Category: "c", Language: store.LangEnglish,
		ParentID: parent, CheckInterval: time.Hour, Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertSource(%s): %v", url, err)
	}
	return src
}

func testDisableSourceSticky(t *testing.T, s store.Store) {
	feed := mustSource(t, s, "https://example.test/feed", "c")
	pdf := mustPDF(t, s, "https://example.test/a.pdf", feed.ID)

	if err := s.DisableSource(ctx(t), pdf.ID, store.DisabledGone); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableSource(ctx(t), feed.ID, store.DisabledGone); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableSource(ctx(t), 9999, store.DisabledGone); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DisableSource(missing) err = %v, want ErrNotFound", err)
	}

	// The feed lists the PDF again: rediscovery must not re-enable it.
	again := mustPDF(t, s, pdf.URL, feed.ID)
	if again.Enabled || again.DisabledReason != store.DisabledGone {
		t.Errorf("rediscovered PDF: enabled=%v reason=%q, want disabled/gone", again.Enabled, again.DisabledReason)
	}

	// Re-syncing a curated source from the yaml retries it.
	back := mustSource(t, s, feed.URL, "c")
	if !back.Enabled || back.DisabledReason != "" {
		t.Errorf("re-synced curated source: enabled=%v reason=%q, want enabled/empty", back.Enabled, back.DisabledReason)
	}
}

func testRetireDiscovered(t *testing.T, s store.Store) {
	feed := mustSource(t, s, "https://example.test/feed", "c")
	pdf := mustPDF(t, s, "https://example.test/old.pdf", feed.ID)

	// Nothing was created before an hour ago.
	if n, err := s.RetireDiscovered(ctx(t), time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("RetireDiscovered(past) = %d, %v; want 0", n, err)
	}
	n, err := s.RetireDiscovered(ctx(t), time.Now().Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("RetireDiscovered(future) = %d, %v; want 1 (curated feeds are never retired)", n, err)
	}
	got, _ := s.Source(ctx(t), pdf.ID)
	if got.Enabled || got.DisabledReason != store.DisabledRetired {
		t.Errorf("retired PDF: enabled=%v reason=%q", got.Enabled, got.DisabledReason)
	}
	if got, _ := s.Source(ctx(t), feed.ID); !got.Enabled {
		t.Error("curated feed was retired")
	}
}

func testReschedule(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/r", "c")
	next := time.Now().Add(42 * time.Minute).Truncate(time.Millisecond)
	if err := s.Reschedule(ctx(t), src.ID, next); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Source(ctx(t), src.ID)
	if !got.NextCheckAt.Equal(next) {
		t.Errorf("NextCheckAt = %v, want %v", got.NextCheckAt, next)
	}
	if err := s.Reschedule(ctx(t), 9999, next); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Reschedule(missing) err = %v, want ErrNotFound", err)
	}
}

func testCategories(t *testing.T, s store.Store) {
	mustSource(t, s, "https://example.test/1", "ssc:results")
	mustSource(t, s, "https://example.test/2", "ssc:calendar")
	mustSource(t, s, "https://example.test/3", "ssc:results")
	got, err := s.Categories(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "ssc:calendar" || got[1] != "ssc:results" {
		t.Errorf("Categories = %v", got)
	}
}

// --- snapshots and changes ---------------------------------------------------

func testSnapshots(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/snap", "c")
	if _, err := s.LatestSnapshot(ctx(t), src.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LatestSnapshot on empty source: err = %v, want ErrNotFound", err)
	}

	first, ch, err := s.SaveSnapshot(ctx(t), store.Snapshot{SourceID: src.ID, ContentHash: "h1", Text: "one"}, nil)
	if err != nil || ch != nil {
		t.Fatalf("baseline save: change=%v err=%v", ch, err)
	}
	// Same timestamp on purpose: "latest" must still be the second one.
	second, ch, err := s.SaveSnapshot(ctx(t),
		store.Snapshot{SourceID: src.ID, ContentHash: "h2", Text: "two", FetchedAt: first.FetchedAt},
		&store.Change{OldSnapshotID: first.ID, Kind: store.ChangeEdited, Diff: "-one\n+two"})
	if err != nil {
		t.Fatal(err)
	}
	if ch.ID == 0 || ch.NewSnapshotID != second.ID || ch.SourceID != src.ID || ch.Status != store.StatusPending {
		t.Errorf("saved change = %+v", ch)
	}

	latest, err := s.LatestSnapshot(ctx(t), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != second.ID || latest.Text != "two" || latest.ContentHash != "h2" {
		t.Errorf("latest = %+v, want snapshot %d", latest, second.ID)
	}
}

// testReadQueries covers the listing methods the dashboard uses.
func testReadQueries(t *testing.T, s store.Store) {
	a := mustSource(t, s, "https://example.test/read-a", "c")
	b := mustSource(t, s, "https://example.test/read-b", "c")
	if _, err := s.UpsertSource(ctx(t), store.Source{
		URL: "https://example.test/read-off", Kind: store.KindPDF, Category: "c",
		Language: store.LangHindi, CheckInterval: time.Hour, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := s.Sources(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != a.ID || all[1].ID != b.ID || all[2].Enabled {
		t.Errorf("Sources = %+v, want a, b, then the disabled one", all)
	}

	base, _, err := s.SaveSnapshot(ctx(t), store.Snapshot{SourceID: a.ID, ContentHash: "h1", Text: "one"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, chA, err := s.SaveSnapshot(ctx(t), store.Snapshot{SourceID: a.ID, ContentHash: "h2", Text: "two"},
		&store.Change{OldSnapshotID: base.ID, Kind: store.ChangeEdited, Diff: "- one + two"})
	if err != nil {
		t.Fatal(err)
	}
	chB := mustChange(t, s, b)

	snap, err := s.Snapshot(ctx(t), next.ID)
	if err != nil || snap.Text != "two" || snap.SourceID != a.ID {
		t.Errorf("Snapshot = %+v, %v", snap, err)
	}
	if _, err := s.Snapshot(ctx(t), 99999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Snapshot(missing): err = %v, want ErrNotFound", err)
	}
	snaps, err := s.Snapshots(ctx(t), a.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 || snaps[0].ID != next.ID || snaps[0].Text != "" || snaps[0].ContentHash != "h2" {
		t.Errorf("Snapshots = %+v, want newest first without text", snaps)
	}

	got, err := s.Change(ctx(t), chA.ID)
	if err != nil || got.Diff != chA.Diff || got.OldSnapshotID != base.ID {
		t.Errorf("Change = %+v, %v", got, err)
	}
	if _, err := s.Change(ctx(t), 99999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Change(missing): err = %v, want ErrNotFound", err)
	}

	recent, err := s.RecentChanges(ctx(t), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 || recent[0].ID != chB.ID || recent[1].ID != chA.ID {
		t.Errorf("RecentChanges(all) = %+v, want b's then a's", recent)
	}
	onlyA, err := s.RecentChanges(ctx(t), a.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyA) != 1 || onlyA[0].ID != chA.ID {
		t.Errorf("RecentChanges(a) = %+v", onlyA)
	}
	if one, _ := s.RecentChanges(ctx(t), 0, 1); len(one) != 1 {
		t.Errorf("limit not applied: %d changes", len(one))
	}
}

func testSaveSnapshotAtomic(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/atomic", "c")
	// An invalid change kind violates a CHECK constraint, which must roll
	// back the snapshot inserted earlier in the same transaction.
	_, _, err := s.SaveSnapshot(ctx(t),
		store.Snapshot{SourceID: src.ID, ContentHash: "h", Text: "t"},
		&store.Change{Kind: "exploded"})
	if err == nil {
		t.Fatal("expected an error for invalid change kind")
	}
	if _, err := s.LatestSnapshot(ctx(t), src.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("snapshot survived a failed transaction (err = %v)", err)
	}
}

func testChangeLifecycle(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/life", "c")
	a := mustChange(t, s, src)
	b := mustChange(t, s, src)

	pending, err := s.UnfinishedChanges(ctx(t), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != a.ID || pending[1].ID != b.ID {
		t.Fatalf("unfinished = %+v", pending)
	}

	a.Importance, a.Status, a.Attempts = store.ImportanceDate, store.StatusSummarized, 1
	b.Importance, b.Status = store.ImportanceCosmetic, store.StatusSuppressed
	for _, c := range []*store.Change{a, b} {
		if err := s.UpdateChange(ctx(t), *c); err != nil {
			t.Fatal(err)
		}
	}

	pending, _ = s.UnfinishedChanges(ctx(t), 10)
	if len(pending) != 1 || pending[0].ID != a.ID ||
		pending[0].Importance != store.ImportanceDate || pending[0].Attempts != 1 {
		t.Errorf("after update, unfinished = %+v", pending)
	}

	if err := s.UpdateChange(ctx(t), store.Change{ID: 999, Status: store.StatusFailed}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateChange(999) err = %v, want ErrNotFound", err)
	}
}

func testSummaries(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/sum", "c")
	ch := mustChange(t, s, src)

	if _, err := s.Summary(ctx(t), ch.ID, store.LangHindi); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing summary err = %v, want ErrNotFound", err)
	}
	if err := s.SaveSummary(ctx(t), ch.ID, store.LangHindi, "पहला"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSummary(ctx(t), ch.ID, store.LangHindi, "अंतिम तिथि बदली"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Summary(ctx(t), ch.ID, store.LangHindi)
	if err != nil || got != "अंतिम तिथि बदली" {
		t.Errorf("Summary = %q, %v", got, err)
	}
	if _, err := s.Summary(ctx(t), ch.ID, store.LangEnglish); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("English summary should not exist, err = %v", err)
	}
}

// --- subscribers -------------------------------------------------------------

func testSubscribers(t *testing.T, s store.Store) {
	sub := mustSubscriber(t, s, 42)
	c := telegramContact(t, s, 42)
	if sub.Language != store.LangEnglish || c.SubscriberID != sub.ID || !c.Active ||
		c.ChatID != 420 || !c.Verified() || c.Channel != store.ChannelTelegram {
		t.Errorf("new subscriber = %+v, contact = %+v", sub, c)
	}

	if err := s.SetLanguage(ctx(t), sub.ID, store.LangHindi); err != nil {
		t.Fatal(err)
	}
	if err := s.SetContactActive(ctx(t), c.ID, false); err != nil {
		t.Fatal(err)
	}
	// Coming back (e.g. /start after unblocking) reactivates and keeps language.
	again, err := s.TelegramSubscriber(ctx(t), 42, 4200)
	if err != nil {
		t.Fatal(err)
	}
	c = telegramContact(t, s, 42)
	if again.ID != sub.ID || !c.Active || c.ChatID != 4200 || again.Language != store.LangHindi {
		t.Errorf("returning subscriber = %+v, contact = %+v", again, c)
	}

	if err := s.SetLanguage(ctx(t), sub.ID, "fr"); err == nil {
		t.Error("SetLanguage accepted an unknown language")
	}
	if err := s.SetLanguage(ctx(t), 9999, store.LangHindi); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetLanguage(unknown) err = %v, want ErrNotFound", err)
	}
	if _, err := s.Subscriber(ctx(t), 9999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Subscriber(unknown) err = %v, want ErrNotFound", err)
	}
}

func testEmailContacts(t *testing.T, s store.Store) {
	web, err := s.CreateSubscriber(ctx(t), store.LangHindi)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.AddEmail(ctx(t), web.ID, "  Asha@Example.TEST ")
	if err != nil {
		t.Fatal(err)
	}
	if c.Address != "asha@example.test" || c.Verified() || !c.Active || c.SubscriberID != web.ID {
		t.Errorf("new email contact = %+v", c)
	}
	// The same address again, even from someone else, returns the original.
	other, _ := s.CreateSubscriber(ctx(t), store.LangEnglish)
	dup, err := s.AddEmail(ctx(t), other.ID, "asha@example.test")
	if err != nil || dup.ID != c.ID || dup.SubscriberID != web.ID {
		t.Errorf("duplicate AddEmail = %+v, %v; want the existing contact", dup, err)
	}
	if _, err := s.AddEmail(ctx(t), web.ID, "not-an-address"); err == nil {
		t.Error("AddEmail accepted a non-address")
	}

	if err := s.VerifyContact(ctx(t), c.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ContactByAddress(ctx(t), store.ChannelEmail, "ASHA@example.test")
	if !got.Verified() {
		t.Errorf("after verify = %+v", got)
	}
	all, err := s.Contacts(ctx(t), web.ID)
	if err != nil || len(all) != 1 {
		t.Errorf("Contacts = %+v, %v", all, err)
	}
	if _, err := s.ContactByAddress(ctx(t), store.ChannelEmail, "nobody@example.test"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ContactByAddress(unknown) err = %v", err)
	}
}

func testLinkTelegramMerges(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/link", "ssc:results")
	ch := mustChange(t, s, src)

	// Someone used the bot first...
	bot := mustSubscriber(t, s, 7)
	tg := telegramContact(t, s, 7)
	if err := s.Subscribe(ctx(t), bot.ID, store.Target{Category: "ssc:results"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInterests(ctx(t), bot.ID, []string{"upsc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDelivered(ctx(t), ch.ID, tg.ID); err != nil {
		t.Fatal(err)
	}
	// ...then signed up on the web and linked Telegram.
	web, _ := s.CreateSubscriber(ctx(t), store.LangEnglish)
	if err := s.Subscribe(ctx(t), web.ID, store.Target{Category: "ssc:calendar"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInterests(ctx(t), web.ID, []string{"ibps", "upsc"}); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkTelegram(ctx(t), web.ID, 7, 77); err != nil {
		t.Fatal(err)
	}

	tg = telegramContact(t, s, 7)
	if tg.SubscriberID != web.ID || tg.ChatID != 77 {
		t.Errorf("telegram contact after link = %+v", tg)
	}
	if _, err := s.Subscriber(ctx(t), bot.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("merged subscriber still exists: err = %v", err)
	}
	subs, _ := s.Subscriptions(ctx(t), web.ID)
	if len(subs) != 2 {
		t.Errorf("subscriptions after merge = %+v, want calendar + results", subs)
	}
	if got, _ := s.Interests(ctx(t), web.ID); len(got) != 2 || got[0] != "ibps" || got[1] != "upsc" {
		t.Errorf("interests after merge = %v", got)
	}
	// The delivery moved with the contact: no repeat alert.
	if again, _ := s.MarkDelivered(ctx(t), ch.ID, tg.ID); again {
		t.Error("delivery record lost in the merge")
	}

	// Linking a brand-new Telegram account just adds it.
	if err := s.LinkTelegram(ctx(t), web.ID, 8, 88); err != nil {
		t.Fatal(err)
	}
	if c := telegramContact(t, s, 8); c.SubscriberID != web.ID || !c.Verified() {
		t.Errorf("new linked contact = %+v", c)
	}
	// Linking again is harmless.
	if err := s.LinkTelegram(ctx(t), web.ID, 8, 89); err != nil {
		t.Fatal(err)
	}
}

func testInterests(t *testing.T, s store.Store) {
	sub, _ := s.CreateSubscriber(ctx(t), store.LangEnglish)
	if err := s.SetInterests(ctx(t), sub.ID, []string{"upsc", "ssc", "upsc"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInterests(ctx(t), sub.ID, []string{"ssc", "cat"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Interests(ctx(t), sub.ID)
	if err != nil || len(got) != 2 || got[0] != "cat" || got[1] != "ssc" {
		t.Errorf("Interests = %v, %v; want [cat ssc] (replaced, sorted)", got, err)
	}
}

func testTokens(t *testing.T, s store.Store) {
	sub, _ := s.CreateSubscriber(ctx(t), store.LangEnglish)
	now := time.Now()
	tok := store.Token{Hash: "h1", Purpose: store.TokenLogin, SubscriberID: sub.ID, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateToken(ctx(t), tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeToken(ctx(t), "h1", store.TokenVerifyEmail, now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("wrong purpose: err = %v, want ErrNotFound", err)
	}
	if _, err := s.ConsumeToken(ctx(t), "h1", store.TokenLogin, now.Add(2*time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired: err = %v, want ErrNotFound", err)
	}
	got, err := s.ConsumeToken(ctx(t), "h1", store.TokenLogin, now)
	if err != nil || got.SubscriberID != sub.ID {
		t.Fatalf("ConsumeToken = %+v, %v", got, err)
	}
	if _, err := s.ConsumeToken(ctx(t), "h1", store.TokenLogin, now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second use: err = %v, want ErrNotFound", err)
	}
	if err := s.CreateToken(ctx(t), store.Token{Hash: "h2"}); err == nil {
		t.Error("CreateToken accepted an incomplete token")
	}
}

func testSetting(t *testing.T, s store.Store) {
	v, err := s.Setting(ctx(t), "secret", "first")
	if err != nil || v != "first" {
		t.Fatalf("Setting = %q, %v", v, err)
	}
	if v, _ := s.Setting(ctx(t), "secret", "second"); v != "first" {
		t.Errorf("Setting overwrote the stored value: %q", v)
	}
}

func testFeedChanges(t *testing.T, s store.Store) {
	a := mustSource(t, s, "https://example.test/fa", "ssc:results")
	b := mustSource(t, s, "https://example.test/fb", "ssc:calendar")
	c1 := mustChange(t, s, a)
	c2 := mustChange(t, s, b)
	c3 := mustChange(t, s, a)
	cosmetic := mustChange(t, s, a)
	cosmetic.Importance, cosmetic.Status = store.ImportanceCosmetic, store.StatusSuppressed
	if err := s.UpdateChange(ctx(t), *cosmetic); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSummary(ctx(t), c1.ID, store.LangEnglish, "english"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSummary(ctx(t), c3.ID, store.LangHindi, "hindi"); err != nil {
		t.Fatal(err)
	}

	ids := func(es []store.FeedEntry) []int64 {
		out := make([]int64, len(es))
		for i, e := range es {
			out[i] = e.Change.ID
		}
		return out
	}
	all, err := s.FeedChanges(ctx(t), store.FeedQuery{Lang: store.LangHindi})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{c3.ID, c2.ID, c1.ID}; !equalInts(ids(all), want) {
		t.Errorf("all = %v, want %v (newest first, cosmetic hidden)", ids(all), want)
	}
	if all[0].Summary != "hindi" || all[2].Summary != "english" || all[1].Summary != "" {
		t.Errorf("summaries = %q %q %q; want hindi, none, English fallback",
			all[0].Summary, all[1].Summary, all[2].Summary)
	}
	if all[1].Source.ID != b.ID || all[1].Source.Category != "ssc:calendar" {
		t.Errorf("source not joined: %+v", all[1].Source)
	}

	onlyA, _ := s.FeedChanges(ctx(t), store.FeedQuery{Categories: []string{"ssc:results"}})
	if want := []int64{c3.ID, c1.ID}; !equalInts(ids(onlyA), want) {
		t.Errorf("category filter = %v, want %v", ids(onlyA), want)
	}
	after, _ := s.FeedChanges(ctx(t), store.FeedQuery{AfterID: c2.ID})
	if want := []int64{c3.ID}; !equalInts(ids(after), want) {
		t.Errorf("AfterID = %v, want %v", ids(after), want)
	}
	page, _ := s.FeedChanges(ctx(t), store.FeedQuery{BeforeID: c3.ID, Limit: 1})
	if want := []int64{c2.ID}; !equalInts(ids(page), want) {
		t.Errorf("BeforeID+Limit = %v, want %v", ids(page), want)
	}

	st, err := s.Stats(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if st.Feeds != 2 || st.Sources != 2 || st.ChangesCaught != 3 {
		t.Errorf("stats = %+v", st)
	}
}

func testSubscriptions(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/subs", "ssc:results")
	sub := mustSubscriber(t, s, 1)

	cat := store.Target{Category: "ssc:results"}
	one := store.Target{SourceID: src.ID}
	for _, target := range []store.Target{cat, cat, one} { // cat twice: idempotent
		if err := s.Subscribe(ctx(t), sub.ID, target); err != nil {
			t.Fatalf("Subscribe(%+v): %v", target, err)
		}
	}
	subs, err := s.Subscriptions(ctx(t), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 || subs[0].Target != cat || subs[1].Target != one {
		t.Errorf("subscriptions = %+v", subs)
	}

	if err := s.Subscribe(ctx(t), sub.ID, store.Target{SourceID: src.ID, Category: "x"}); err == nil {
		t.Error("Subscribe accepted a target with both fields set")
	}
	if err := s.Subscribe(ctx(t), sub.ID, store.Target{}); err == nil {
		t.Error("Subscribe accepted an empty target")
	}
	if err := s.Subscribe(ctx(t), 9999, cat); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Subscribe(unknown user) err = %v, want ErrNotFound", err)
	}

	removed, err := s.Unsubscribe(ctx(t), sub.ID, cat)
	if err != nil || !removed {
		t.Errorf("Unsubscribe = %v, %v; want true, nil", removed, err)
	}
	removed, err = s.Unsubscribe(ctx(t), sub.ID, cat)
	if err != nil || removed {
		t.Errorf("second Unsubscribe = %v, %v; want false, nil", removed, err)
	}
	subs, _ = s.Subscriptions(ctx(t), sub.ID)
	if len(subs) != 1 || subs[0].Target != one {
		t.Errorf("after unsubscribe = %+v", subs)
	}
}

func testRecipientsFor(t *testing.T, s store.Store) {
	feed := mustSource(t, s, "https://example.test/feed", "ssc:notice-board")
	pdf, err := s.UpsertSource(ctx(t), store.Source{
		URL: "https://example.test/notice.pdf", Kind: store.KindPDF, Category: "ssc:notice-board",
		Language: store.LangEnglish, ParentID: feed.ID, CheckInterval: time.Hour, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	other := mustSource(t, s, "https://example.test/other", "ssc:results")

	subs := map[int64]store.Subscriber{}
	tg := map[int64]int64{} // telegram id -> contact id
	for _, id := range []int64{1, 2, 3, 4, 5} {
		subs[id] = mustSubscriber(t, s, id)
		tg[id] = telegramContact(t, s, id).ID
	}
	subscribe := func(id int64, target store.Target) {
		t.Helper()
		if err := s.Subscribe(ctx(t), subs[id].ID, target); err != nil {
			t.Fatal(err)
		}
	}
	subscribe(1, store.Target{Category: "ssc:notice-board"})
	subscribe(1, store.Target{SourceID: feed.ID}) // overlaps; must not duplicate
	subscribe(2, store.Target{SourceID: feed.ID}) // parent of the PDF
	subscribe(3, store.Target{Category: "ssc:results"})
	subscribe(4, store.Target{Category: "ssc:notice-board"})
	subscribe(5, store.Target{SourceID: pdf.ID})
	if err := s.SetContactActive(ctx(t), tg[4], false); err != nil { // blocked the bot
		t.Fatal(err)
	}
	if err := s.SetLanguage(ctx(t), subs[2].ID, store.LangHindi); err != nil {
		t.Fatal(err)
	}
	// Subscriber 1 also has email: unconfirmed first, so no alerts there.
	email, err := s.AddEmail(ctx(t), subs[1].ID, "one@example.test")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		src  store.Source
		want []int64
	}{
		{feed, []int64{tg[1], tg[2]}},
		{pdf, []int64{tg[1], tg[2], tg[5]}},
		{other, []int64{tg[3]}},
	}
	for _, c := range cases {
		rs, err := s.RecipientsFor(ctx(t), c.src)
		if err != nil {
			t.Fatal(err)
		}
		if got := contactIDs(rs); !equalInts(got, c.want) {
			t.Errorf("RecipientsFor(%s) = %v, want %v", c.src.URL, got, c.want)
		}
	}

	// Once confirmed, the email address is a second recipient for person 1.
	if err := s.VerifyContact(ctx(t), email.ID); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.RecipientsFor(ctx(t), feed)
	if want := []int64{tg[1], tg[2], email.ID}; !equalInts(contactIDs(rs), want) {
		t.Errorf("with confirmed email = %v, want %v", contactIDs(rs), want)
	}
	for _, r := range rs {
		if r.ID == tg[2] && r.Language != store.LangHindi {
			t.Errorf("recipient language = %q, want the subscriber's (hi)", r.Language)
		}
		if r.ID == email.ID && (r.Channel != store.ChannelEmail || r.Address != "one@example.test") {
			t.Errorf("email recipient = %+v", r)
		}
	}
}

func testMarkDelivered(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/deliv", "c")
	ch := mustChange(t, s, src)
	mustSubscriber(t, s, 1)
	c := telegramContact(t, s, 1)

	first, err := s.MarkDelivered(ctx(t), ch.ID, c.ID)
	if err != nil || !first {
		t.Fatalf("first MarkDelivered = %v, %v", first, err)
	}
	again, err := s.MarkDelivered(ctx(t), ch.ID, c.ID)
	if err != nil || again {
		t.Errorf("second MarkDelivered = %v, %v; want false", again, err)
	}
}

func testMarkDeliveredConcurrent(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/race", "c")
	ch := mustChange(t, s, src)
	mustSubscriber(t, s, 1)
	sub := telegramContact(t, s, 1)

	// Many goroutines race to deliver the same alert; exactly one may win.
	const n = 20
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.MarkDelivered(ctx(t), ch.ID, sub.ID)
			if err != nil {
				t.Error(err) // t.Error is goroutine-safe; t.Fatal is not
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d goroutines won the delivery, want exactly 1", wins)
	}
}

func testDeleteSubscriber(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/del", "c")
	ch := mustChange(t, s, src)
	sub := mustSubscriber(t, s, 3)
	c := telegramContact(t, s, 3)
	email, _ := s.AddEmail(ctx(t), sub.ID, "del@example.test")
	s.Subscribe(ctx(t), sub.ID, store.Target{Category: "c"})
	s.SetInterests(ctx(t), sub.ID, []string{"upsc"})
	s.MarkDelivered(ctx(t), ch.ID, c.ID)

	if err := s.DeleteSubscriber(ctx(t), sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Subscriber(ctx(t), sub.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("subscriber still there: %v", err)
	}
	for _, id := range []int64{c.ID, email.ID} {
		if _, err := s.Contact(ctx(t), id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("contact %d still there: %v", id, err)
		}
	}
	if rs, _ := s.RecipientsFor(ctx(t), src); len(rs) != 0 {
		t.Errorf("recipients after delete = %+v", rs)
	}
	if err := s.DeleteSubscriber(ctx(t), sub.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete err = %v", err)
	}
	// The address is free again.
	again, _ := s.CreateSubscriber(ctx(t), store.LangEnglish)
	if c2, err := s.AddEmail(ctx(t), again.ID, "del@example.test"); err != nil || c2.SubscriberID != again.ID {
		t.Errorf("re-adding a deleted address = %+v, %v", c2, err)
	}
}

// A kept interest keeps its age; launch notices depend on it.
func testInterestsKeepCreatedAt(t *testing.T, s store.Store) {
	sub := mustSubscriber(t, s, 501)
	if err := s.SetInterests(ctx(t), sub.ID, []string{"jee"}); err != nil {
		t.Fatal(err)
	}
	liveAt := time.Now().Add(time.Hour) // jee went live after they chose it
	if _, err := s.ExamLive(ctx(t), "jee", liveAt); err != nil {
		t.Fatal(err)
	}
	// Re-saving the survey (adding neet) must not make "jee" look new.
	if err := s.SetInterests(ctx(t), sub.ID, []string{"neet", "jee"}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.LaunchCandidates(ctx(t), "jee", liveAt, "jee:")
	if err != nil || len(ids) != 1 {
		t.Errorf("candidates after re-save = %v, %v; want the subscriber", ids, err)
	}
}

func testLaunches(t *testing.T, s store.Store) {
	now := time.Now()
	// SSC is seeded as live from the start.
	if at, err := s.ExamLive(ctx(t), "ssc", now); err != nil || !at.Before(now.Add(-24*time.Hour)) {
		t.Errorf("ssc live at %v, %v; want the seeded zero time", at, err)
	}
	// The first call records the time; later calls return it unchanged.
	first, err := s.ExamLive(ctx(t), "jee", now)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := s.ExamLive(ctx(t), "jee", now.Add(time.Hour)); !again.Equal(first) {
		t.Errorf("ExamLive moved from %v to %v", first, again)
	}

	asked := mustSubscriber(t, s, 601) // chose jee, verified Telegram: told
	watching := mustSubscriber(t, s, 602)
	unverified, _ := s.CreateSubscriber(ctx(t), store.LangEnglish) // email never confirmed
	if _, err := s.AddEmail(ctx(t), unverified.ID, "u@example.test"); err != nil {
		t.Fatal(err)
	}
	blocked := mustSubscriber(t, s, 603)
	if err := s.SetContactActive(ctx(t), telegramContact(t, s, 603).ID, false); err != nil {
		t.Fatal(err)
	}
	other := mustSubscriber(t, s, 604) // chose neet only
	for _, sub := range []store.Subscriber{asked, watching, unverified, blocked} {
		if err := s.SetInterests(ctx(t), sub.ID, []string{"jee", "ssc"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetInterests(ctx(t), other.ID, []string{"neet"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Subscribe(ctx(t), watching.ID, store.Target{Category: "jee:nta-notices"}); err != nil {
		t.Fatal(err)
	}

	later := now.Add(time.Minute) // interests above were chosen before this
	ids, err := s.LaunchCandidates(ctx(t), "jee", later, "jee:")
	if err != nil || !equalInts(ids, []int64{asked.ID}) {
		t.Errorf("candidates = %v, %v; want only %d", ids, err, asked.ID)
	}
	// Interests chosen after the launch were handled by the survey.
	if ids, _ := s.LaunchCandidates(ctx(t), "jee", now.Add(-time.Hour), "jee:"); len(ids) != 0 {
		t.Errorf("candidates for an earlier launch = %v, want none", ids)
	}
	// A literal prefix: "jee_" must not match "jee:" or "jeex".
	if ids, _ := s.LaunchCandidates(ctx(t), "jee", later, "jee_"); len(ids) != 2 {
		t.Errorf("candidates with an unrelated prefix = %v, want asked and watching", ids)
	}

	if ok, err := s.MarkLaunchNotice(ctx(t), asked.ID, "jee"); !ok || err != nil {
		t.Fatalf("first mark = %v, %v", ok, err)
	}
	if ok, _ := s.MarkLaunchNotice(ctx(t), asked.ID, "jee"); ok {
		t.Error("second mark returned true")
	}
	if ids, _ := s.LaunchCandidates(ctx(t), "jee", later, "jee:"); len(ids) != 0 {
		t.Errorf("candidates after marking = %v, want none", ids)
	}
	// Deleting the person removes their notices too (no FK error).
	if err := s.DeleteSubscriber(ctx(t), asked.ID); err != nil {
		t.Fatal(err)
	}
}
