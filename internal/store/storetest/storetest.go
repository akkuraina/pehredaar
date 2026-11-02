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
		{"Categories", testCategories},
		{"Snapshots", testSnapshots},
		{"SaveSnapshotIsAtomic", testSaveSnapshotAtomic},
		{"ReadQueries", testReadQueries},
		{"ChangeLifecycle", testChangeLifecycle},
		{"Summaries", testSummaries},
		{"Subscribers", testSubscribers},
		{"Subscriptions", testSubscriptions},
		{"SubscribersFor", testSubscribersFor},
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

func mustSubscriber(t *testing.T, s store.Store, telegramID int64) store.Subscriber {
	t.Helper()
	sub, err := s.UpsertSubscriber(ctx(t), telegramID, telegramID*10)
	if err != nil {
		t.Fatalf("UpsertSubscriber(%d): %v", telegramID, err)
	}
	return sub
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

func telegramIDs(subs []store.Subscriber) []int64 {
	ids := make([]int64, len(subs))
	for i, s := range subs {
		ids[i] = s.TelegramID
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
	if sub.Language != store.LangEnglish || !sub.Active || sub.ChatID != 420 {
		t.Errorf("new subscriber = %+v", sub)
	}

	if err := s.SetLanguage(ctx(t), 42, store.LangHindi); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive(ctx(t), 42, false); err != nil {
		t.Fatal(err)
	}
	// Coming back (e.g. /start after unblocking) reactivates and keeps language.
	again, err := s.UpsertSubscriber(ctx(t), 42, 4200)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != sub.ID || !again.Active || again.ChatID != 4200 || again.Language != store.LangHindi {
		t.Errorf("re-upserted subscriber = %+v", again)
	}

	if err := s.SetLanguage(ctx(t), 42, "fr"); err == nil {
		t.Error("SetLanguage accepted an unknown language")
	}
	if err := s.SetLanguage(ctx(t), 7, store.LangHindi); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetLanguage(unknown user) err = %v, want ErrNotFound", err)
	}
}

func testSubscriptions(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/subs", "ssc:results")
	mustSubscriber(t, s, 1)

	cat := store.Target{Category: "ssc:results"}
	one := store.Target{SourceID: src.ID}
	for _, target := range []store.Target{cat, cat, one} { // cat twice: idempotent
		if err := s.Subscribe(ctx(t), 1, target); err != nil {
			t.Fatalf("Subscribe(%+v): %v", target, err)
		}
	}
	subs, err := s.Subscriptions(ctx(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 || subs[0].Target != cat || subs[1].Target != one {
		t.Errorf("subscriptions = %+v", subs)
	}

	if err := s.Subscribe(ctx(t), 1, store.Target{SourceID: src.ID, Category: "x"}); err == nil {
		t.Error("Subscribe accepted a target with both fields set")
	}
	if err := s.Subscribe(ctx(t), 1, store.Target{}); err == nil {
		t.Error("Subscribe accepted an empty target")
	}
	if err := s.Subscribe(ctx(t), 99, cat); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Subscribe(unknown user) err = %v, want ErrNotFound", err)
	}

	removed, err := s.Unsubscribe(ctx(t), 1, cat)
	if err != nil || !removed {
		t.Errorf("Unsubscribe = %v, %v; want true, nil", removed, err)
	}
	removed, err = s.Unsubscribe(ctx(t), 1, cat)
	if err != nil || removed {
		t.Errorf("second Unsubscribe = %v, %v; want false, nil", removed, err)
	}
	subs, _ = s.Subscriptions(ctx(t), 1)
	if len(subs) != 1 || subs[0].Target != one {
		t.Errorf("after unsubscribe = %+v", subs)
	}
}

func testSubscribersFor(t *testing.T, s store.Store) {
	feed := mustSource(t, s, "https://example.test/feed", "ssc:notice-board")
	pdf, err := s.UpsertSource(ctx(t), store.Source{
		URL: "https://example.test/notice.pdf", Kind: store.KindPDF, Category: "ssc:notice-board",
		Language: store.LangEnglish, ParentID: feed.ID, CheckInterval: time.Hour, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	other := mustSource(t, s, "https://example.test/other", "ssc:results")

	for _, id := range []int64{1, 2, 3, 4, 5} {
		mustSubscriber(t, s, id)
	}
	subscribe := func(id int64, target store.Target) {
		t.Helper()
		if err := s.Subscribe(ctx(t), id, target); err != nil {
			t.Fatal(err)
		}
	}
	subscribe(1, store.Target{Category: "ssc:notice-board"})
	subscribe(1, store.Target{SourceID: feed.ID}) // overlaps; must not duplicate
	subscribe(2, store.Target{SourceID: feed.ID}) // parent of the PDF
	subscribe(3, store.Target{Category: "ssc:results"})
	subscribe(4, store.Target{Category: "ssc:notice-board"})
	subscribe(5, store.Target{SourceID: pdf.ID})
	if err := s.SetActive(ctx(t), 4, false); err != nil { // blocked the bot
		t.Fatal(err)
	}

	cases := []struct {
		src  store.Source
		want []int64
	}{
		{feed, []int64{1, 2}},
		{pdf, []int64{1, 2, 5}},
		{other, []int64{3}},
	}
	for _, c := range cases {
		subs, err := s.SubscribersFor(ctx(t), c.src)
		if err != nil {
			t.Fatal(err)
		}
		if got := telegramIDs(subs); !equalInts(got, c.want) {
			t.Errorf("SubscribersFor(%s) = %v, want %v", c.src.URL, got, c.want)
		}
	}
}

func testMarkDelivered(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/deliv", "c")
	ch := mustChange(t, s, src)
	sub := mustSubscriber(t, s, 1)

	first, err := s.MarkDelivered(ctx(t), ch.ID, sub.ID)
	if err != nil || !first {
		t.Fatalf("first MarkDelivered = %v, %v", first, err)
	}
	again, err := s.MarkDelivered(ctx(t), ch.ID, sub.ID)
	if err != nil || again {
		t.Errorf("second MarkDelivered = %v, %v; want false", again, err)
	}
}

func testMarkDeliveredConcurrent(t *testing.T, s store.Store) {
	src := mustSource(t, s, "https://example.test/race", "c")
	ch := mustChange(t, s, src)
	sub := mustSubscriber(t, s, 1)

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
