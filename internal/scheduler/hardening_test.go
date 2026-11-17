package scheduler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/akkuraina/pehredaar/internal/fetcher"
	"github.com/akkuraina/pehredaar/internal/normalize"
	"github.com/akkuraina/pehredaar/internal/store"
)

// panicFetcher panics on every fetch, standing in for any bug deep in the
// pipeline (a parser choking on a strange PDF, say).
type panicFetcher struct{}

func (panicFetcher) Fetch(context.Context, fetcher.Request) (*fetcher.Response, error) {
	panic("boom")
}

func TestPanicBecomesFailedCheck(t *testing.T) {
	e := setup(t, false)
	e.sched.fetch = panicFetcher{}

	ev, src := e.checkNow(t)
	if ev.Outcome != OutcomeFailed || !strings.Contains(ev.Detail, "internal error: boom") {
		t.Errorf("event = %+v, want a failed check mentioning the panic", ev)
	}
	if src.ConsecutiveFailures != 1 || src.NextCheckAt.IsZero() {
		t.Errorf("source = %+v, want one failure and a retry scheduled", src)
	}
}

// hangingFetcher blocks until its context ends, like a server that accepts
// the connection and never answers.
type hangingFetcher struct{}

func (hangingFetcher) Fetch(ctx context.Context, _ fetcher.Request) (*fetcher.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCheckTimeout(t *testing.T) {
	e := setup(t, false)
	e.sched.fetch = hangingFetcher{}
	e.sched.timeout = 50 * time.Millisecond

	ev, src := e.checkNow(t)
	if ev.Outcome != OutcomeFailed || !strings.Contains(ev.Detail, "longer than 50ms") {
		t.Errorf("event = %+v, want a timed-out failure", ev)
	}
	if src.ConsecutiveFailures != 1 {
		t.Errorf("failures = %d, want 1 (a timeout is the source's failure, not a shutdown)", src.ConsecutiveFailures)
	}
}

func notFound() fakeResult {
	return fakeResult{err: &fetcher.Error{Kind: fetcher.Permanent, URL: feedURL, StatusCode: http.StatusNotFound}}
}

func TestRepeated404DisablesSource(t *testing.T) {
	e := setup(t, false)
	for i := 1; i <= goneAfter; i++ {
		e.fetch.push(feedURL, notFound())
		ev, src := e.checkNow(t)
		if i < goneAfter {
			if !src.Enabled {
				t.Fatalf("disabled after %d 404s, want %d", i, goneAfter)
			}
			continue
		}
		if src.Enabled || src.DisabledReason != store.DisabledGone {
			t.Errorf("after %d 404s: enabled=%v reason=%q", i, src.Enabled, src.DisabledReason)
		}
		if !strings.Contains(ev.Detail, "disabled: gone") {
			t.Errorf("detail = %q", ev.Detail)
		}
	}
}

func TestDisableReason(t *testing.T) {
	feedSrc := store.Source{Kind: store.KindFeed, ConsecutiveFailures: goneAfter - 1}
	pdf := store.Source{Kind: store.KindPDF}
	err404 := &fetcher.Error{Kind: fetcher.Permanent, StatusCode: http.StatusNotFound}
	tests := []struct {
		name string
		src  store.Source
		err  error
		want store.DisabledReason
	}{
		{"scanned pdf", pdf, normalize.ErrNoText, store.DisabledNeedsOCR},
		{"wrapped no-text", pdf, errors.Join(errors.New("x"), normalize.ErrNoText), store.DisabledNeedsOCR},
		{"404 at the limit", feedSrc, err404, store.DisabledGone},
		{"410 at the limit", feedSrc, &fetcher.Error{Kind: fetcher.Permanent, StatusCode: http.StatusGone}, store.DisabledGone},
		{"404 below the limit", pdf, err404, ""},
		// Oversized bodies are Permanent too, but the URL isn't gone.
		{"oversized body", feedSrc, &fetcher.Error{Kind: fetcher.Permanent, StatusCode: 200}, ""},
		{"403 blocked", feedSrc, &fetcher.Error{Kind: fetcher.Blocked, StatusCode: http.StatusForbidden}, ""},
		{"5xx", feedSrc, &fetcher.Error{Kind: fetcher.Temporary, StatusCode: 502}, ""},
		{"error page", feedSrc, normalize.ErrNotContent, ""},
	}
	for _, tt := range tests {
		if got := disableReason(tt.src, tt.err); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

// discoverTwo checks the feed once so it discovers two PDFs, and returns them.
func discoverTwo(t *testing.T, e env) []store.Source {
	t.Helper()
	e.fetch.push(feedURL, ok(feed(item("b", 2, "Notice B"), item("a", 1, "Notice A")), ""))
	e.checkNow(t)
	all, err := e.st.Sources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d sources, want feed + 2 PDFs", len(all))
	}
	return all[1:]
}

func TestRediscoveryKeepsDisabledPDFDisabled(t *testing.T) {
	e := setup(t, true)
	pdfs := discoverTwo(t, e)
	if err := e.st.DisableSource(t.Context(), pdfs[0].ID, store.DisabledNeedsOCR); err != nil {
		t.Fatal(err)
	}
	// The feed changes, so discovery upserts both PDFs again.
	e.fetch.push(feedURL, ok(feed(item("c", 3, "Notice C"), item("b", 2, "Notice B"), item("a", 1, "Notice A")), ""))
	e.checkNow(t)

	got, _ := e.st.Source(t.Context(), pdfs[0].ID)
	if got.Enabled || got.DisabledReason != store.DisabledNeedsOCR {
		t.Errorf("rediscovered PDF: enabled=%v reason=%q, want it to stay disabled", got.Enabled, got.DisabledReason)
	}
}

func TestSpreadOverdueKeepsFeedsDueAndSpreadsPDFs(t *testing.T) {
	e := setup(t, true)
	pdfs := discoverTwo(t, e)

	// Pretend the process was off for a day: everything is overdue.
	now := time.Now().Add(24 * time.Hour)
	e.sched.now = func() time.Time { return now }
	if err := e.sched.spreadOverdue(t.Context()); err != nil {
		t.Fatal(err)
	}

	feedRow, _ := e.st.Source(t.Context(), e.src.ID)
	if feedRow.NextCheckAt.After(now) {
		t.Errorf("curated feed pushed to %v; it should stay due", feedRow.NextCheckAt)
	}
	for _, p := range pdfs {
		got, _ := e.st.Source(t.Context(), p.ID)
		if got.NextCheckAt.Before(now.Add(-time.Millisecond)) || got.NextCheckAt.After(now.Add(spreadWindow)) {
			t.Errorf("PDF next check %v, want within [now, now+%v]", got.NextCheckAt, spreadWindow)
		}
	}
}

func TestRetireOld(t *testing.T) {
	e := setup(t, true)
	pdfs := discoverTwo(t, e)

	e.sched.retireOld(t.Context()) // just discovered: nothing to retire
	if got, _ := e.st.Source(t.Context(), pdfs[0].ID); !got.Enabled {
		t.Fatal("a fresh PDF was retired")
	}

	later := time.Now().Add(e.sched.retire + time.Hour)
	e.sched.now = func() time.Time { return later }
	e.sched.retireOld(t.Context())
	for _, p := range pdfs {
		if got, _ := e.st.Source(t.Context(), p.ID); got.Enabled || got.DisabledReason != store.DisabledRetired {
			t.Errorf("PDF %d: enabled=%v reason=%q, want retired", p.ID, got.Enabled, got.DisabledReason)
		}
	}
	if got, _ := e.st.Source(t.Context(), e.src.ID); !got.Enabled {
		t.Error("the curated feed was retired")
	}
}

// crashStore fails RecordCheck once, simulating the process dying right
// after a snapshot (and its change) was committed but before the source's
// validators and schedule were saved.
type crashStore struct {
	store.Store
	crashed bool
}

func (c *crashStore) RecordCheck(ctx context.Context, id int64, r store.CheckResult) error {
	if !c.crashed {
		c.crashed = true
		return errors.New("simulated crash")
	}
	return c.Store.RecordCheck(ctx, id, r)
}

// After a crash between saving a change and recording the check, the next
// check refetches without the new ETag (it was never stored), sees the same
// content as the saved snapshot, and records nothing new: no lost change,
// no duplicate change, so no duplicate alert.
func TestCrashAfterSnapshotNoDuplicateChange(t *testing.T) {
	e := setup(t, false)
	v1 := feed(item("a", 1, "Notice A"))
	v2 := feed(item("b", 2, "Notice B"), item("a", 1, "Notice A"))

	e.fetch.push(feedURL, ok(v1, `W/"v1"`))
	e.checkNow(t)

	cs := &crashStore{Store: e.st}
	e.sched.store = cs
	e.fetch.push(feedURL, ok(v2, `W/"v2"`))
	if ev, _ := e.checkNow(t); ev.Outcome != OutcomeChanged {
		t.Fatalf("outcome = %s, want changed", ev.Outcome)
	}
	src, _ := e.st.Source(t.Context(), e.src.ID)
	if src.ETag != `W/"v1"` {
		t.Fatalf("ETag = %q; the crash should have left the old one", src.ETag)
	}

	// "Restart": the same content comes back.
	e.fetch.push(feedURL, ok(v2, `W/"v2"`))
	ev, src := e.checkNow(t)
	if ev.Outcome != OutcomeUnchanged {
		t.Errorf("outcome after restart = %s, want unchanged", ev.Outcome)
	}
	if last := e.fetch.reqs[len(e.fetch.reqs)-1]; last.ETag != `W/"v1"` {
		t.Errorf("restart sent ETag %q, want the last recorded one", last.ETag)
	}
	if src.ETag != `W/"v2"` {
		t.Errorf("ETag after restart = %q, want the new one", src.ETag)
	}
	changes, err := e.st.RecentChanges(t.Context(), e.src.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Errorf("got %d changes, want exactly 1", len(changes))
	}
}
