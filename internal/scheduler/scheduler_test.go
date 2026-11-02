package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/fetcher"
	"github.com/akkuraina/pehredaar/internal/normalize"
	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/store/sqlite"
)

const feedURL = "https://ssc.example.test/api/feed?limit=10"

// fakeFetcher returns queued results per URL; tests never touch the network.
type fakeFetcher struct {
	mu    sync.Mutex
	queue map[string][]fakeResult
	reqs  []fetcher.Request
}

type fakeResult struct {
	resp *fetcher.Response
	err  error
}

func (f *fakeFetcher) push(url string, r fakeResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queue == nil {
		f.queue = make(map[string][]fakeResult)
	}
	f.queue[url] = append(f.queue[url], r)
}

func (f *fakeFetcher) Fetch(ctx context.Context, req fetcher.Request) (*fetcher.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	q := f.queue[req.URL]
	if len(q) == 0 {
		return nil, &fetcher.Error{Kind: fetcher.Temporary, URL: req.URL, Err: errors.New("no fake response queued")}
	}
	f.queue[req.URL] = q[1:]
	return q[0].resp, q[0].err
}

func ok(body string, etag string) fakeResult {
	return fakeResult{resp: &fetcher.Response{StatusCode: 200, Body: []byte(body), ETag: etag, FetchedAt: time.Now()}}
}

func notModified() fakeResult {
	return fakeResult{resp: &fetcher.Response{StatusCode: 304, NotModified: true}}
}

// item renders one SSC feed item; n orders items (higher is newer).
func item(id string, n int, headline string) string {
	return fmt.Sprintf(`{"id":%q,"headline":%q,"createdAt":"2026-10-%02dT10:00:00.000Z",`+
		`"attachments":[{"path":"uploads/%s.pdf","fileName":"%s.pdf","size":100}]}`, id, headline, n, id, id)
}

func feed(items ...string) string {
	return `{"statusCode":"200","data":[` + strings.Join(items, ",") + `]}`
}

type env struct {
	sched *Scheduler
	st    store.Store
	fetch *fakeFetcher
	src   store.Source
}

func setup(t *testing.T, discover bool) env {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfgSrc := config.Source{
		Source: store.Source{
			URL: feedURL, Kind: store.KindFeed, Category: "ssc:test", Language: store.LangEnglish,
			Label: "Test feed", CheckInterval: 15 * time.Minute, Enabled: true,
		},
		Windowed:            true,
		DiscoverAttachments: discover,
	}
	sources := config.Sources{List: []config.Source{cfgSrc}, DiscoveredInterval: 6 * time.Hour}
	if _, err := config.Sync(t.Context(), st, sources); err != nil {
		t.Fatal(err)
	}
	src, err := st.Sources(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	f := &fakeFetcher{}
	s := New(Config{
		Store: st, Fetcher: f, Normalizer: normalize.New(), Sources: sources,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	s.jitter = func(d time.Duration) time.Duration { return d } // deterministic schedules
	return env{sched: s, st: st, fetch: f, src: src[0]}
}

// checkNow runs one check and reloads the source row.
func (e env) checkNow(t *testing.T) (Event, store.Source) {
	t.Helper()
	ev, err := e.sched.CheckNow(t.Context(), e.src.ID)
	if err != nil {
		t.Fatal(err)
	}
	src, err := e.st.Source(t.Context(), e.src.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ev, src
}

func TestPipelineLifecycle(t *testing.T) {
	e := setup(t, false)
	v1 := feed(item("b", 2, "Notice B"), item("a", 1, "Notice A"))

	// 1. First sight: baseline, no change.
	e.fetch.push(feedURL, ok(v1, `W/"v1"`))
	ev, src := e.checkNow(t)
	if ev.Outcome != OutcomeBaseline {
		t.Fatalf("first check: %+v", ev)
	}
	if src.ETag != `W/"v1"` || src.ConsecutiveFailures != 0 {
		t.Errorf("after baseline: etag=%q failures=%d", src.ETag, src.ConsecutiveFailures)
	}
	if got := src.NextCheckAt.Sub(src.LastCheckedAt); got != 15*time.Minute {
		t.Errorf("next check in %v, want 15m", got)
	}

	// 2. 304: the stored validators were sent, nothing saved.
	e.fetch.push(feedURL, notModified())
	if ev, _ := e.checkNow(t); ev.Outcome != OutcomeNotModified {
		t.Errorf("second check: %+v", ev)
	}
	if last := e.fetch.reqs[len(e.fetch.reqs)-1]; last.ETag != `W/"v1"` {
		t.Errorf("conditional GET sent etag %q", last.ETag)
	}

	// 3. 200 with the same content in a different order: unchanged.
	e.fetch.push(feedURL, ok(feed(item("a", 1, "Notice A"), item("b", 2, "Notice B")), `W/"v1b"`))
	if ev, _ := e.checkNow(t); ev.Outcome != OutcomeUnchanged {
		t.Errorf("third check: %+v", ev)
	}

	// 4. A new notice: one pending change.
	e.fetch.push(feedURL, ok(feed(item("c", 3, "Notice C"), item("b", 2, "Notice B"), item("a", 1, "Notice A")), `W/"v2"`))
	ev, _ = e.checkNow(t)
	if ev.Outcome != OutcomeChanged || ev.ChangeID == 0 {
		t.Fatalf("fourth check: %+v", ev)
	}
	ch, err := e.st.Change(t.Context(), ev.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Kind != store.ChangeAdded || ch.Status != store.StatusPending || !strings.Contains(ch.Diff, "Notice C") {
		t.Errorf("change = %+v", ch)
	}

	snaps, err := e.st.Snapshots(t.Context(), e.src.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Errorf("got %d snapshots, want 2 (baseline + change)", len(snaps))
	}
	if evs := e.sched.Events(); len(evs) != 4 || evs[0].Outcome != OutcomeChanged {
		t.Errorf("events = %+v", evs)
	}
}

func TestPaginationScrollIsNoise(t *testing.T) {
	e := setup(t, false)
	e.fetch.push(feedURL, ok(feed(item("b", 2, "B"), item("a", 1, "A")), ""))
	e.checkNow(t)

	// The oldest item leaves the bottom of a windowed page and nothing else
	// changes: the canonical text differs, but the differ calls it scrolling.
	// The snapshot is still saved, with no change row.
	e.fetch.push(feedURL, ok(feed(item("b", 2, "B")), ""))
	ev, _ := e.checkNow(t)
	if ev.Outcome != OutcomeNoise {
		t.Errorf("bottom item leaving a windowed page: %+v", ev)
	}
	if chs, _ := e.st.RecentChanges(t.Context(), 0, 10); len(chs) != 0 {
		t.Errorf("noise produced %d changes", len(chs))
	}
}

func TestFailuresBackOffAndKeepValidators(t *testing.T) {
	e := setup(t, false)
	e.fetch.push(feedURL, ok(feed(item("a", 1, "A")), `W/"v1"`))
	e.checkNow(t)

	e.fetch.push(feedURL, fakeResult{err: &fetcher.Error{Kind: fetcher.Temporary, URL: feedURL, StatusCode: 502}})
	ev, src := e.checkNow(t)
	if ev.Outcome != OutcomeFailed || src.ConsecutiveFailures != 1 || src.ETag != `W/"v1"` {
		t.Errorf("after 1st failure: %+v, failures=%d etag=%q", ev, src.ConsecutiveFailures, src.ETag)
	}
	if got := src.NextCheckAt.Sub(src.LastCheckedAt); got != 15*time.Minute {
		t.Errorf("1st backoff %v, want 15m", got)
	}

	// An API error envelope is a failure too, and is never stored.
	e.fetch.push(feedURL, ok(`{"statusCode":"203","error":"Query params are missing"}`, ""))
	ev, src = e.checkNow(t)
	if ev.Outcome != OutcomeFailed || !strings.Contains(ev.Detail, "not usable content") {
		t.Errorf("api error: %+v", ev)
	}
	if got := src.NextCheckAt.Sub(src.LastCheckedAt); got != 30*time.Minute {
		t.Errorf("2nd backoff %v, want 30m", got)
	}

	e.fetch.push(feedURL, fakeResult{err: &fetcher.Error{Kind: fetcher.Permanent, URL: feedURL, StatusCode: 404}})
	_, src = e.checkNow(t)
	if got := src.NextCheckAt.Sub(src.LastCheckedAt); got != maxBackoff {
		t.Errorf("404 backoff %v, want %v", got, maxBackoff)
	}

	// Recovery resets the count; same content means no change.
	e.fetch.push(feedURL, ok(feed(item("a", 1, "A")), `W/"v1"`))
	ev, src = e.checkNow(t)
	if ev.Outcome != OutcomeUnchanged || src.ConsecutiveFailures != 0 || src.LastError != "" {
		t.Errorf("recovery: %+v failures=%d", ev, src.ConsecutiveFailures)
	}
	if snaps, _ := e.st.Snapshots(t.Context(), e.src.ID, 10); len(snaps) != 1 {
		t.Errorf("outage created %d snapshots, want 1", len(snaps))
	}
}

func TestDiscoverAttachments(t *testing.T) {
	e := setup(t, true)
	e.fetch.push(feedURL, ok(feed(item("b", 2, "Notice B"), item("a", 1, "Notice A")), ""))
	ev, _ := e.checkNow(t)
	if !strings.Contains(ev.Detail, "watching 2 attachments") {
		t.Errorf("detail = %q", ev.Detail)
	}
	all, err := e.st.Sources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d sources, want feed + 2 PDFs", len(all))
	}
	pdf := all[1]
	if pdf.Kind != store.KindPDF || pdf.ParentID != e.src.ID || pdf.Category != "ssc:test" ||
		pdf.CheckInterval != 6*time.Hour || pdf.URL != "https://ssc.example.test/api/attachment/uploads/b.pdf" ||
		pdf.Label != "Notice B" {
		t.Errorf("discovered = %+v", pdf)
	}
}

func TestCheckNowBusy(t *testing.T) {
	e := setup(t, false)
	e.sched.claim(e.src.ID)
	if _, err := e.sched.CheckNow(t.Context(), e.src.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrBusy", err)
	}
}

func TestRunChecksDueSourcesAndStops(t *testing.T) {
	e := setup(t, false)
	e.sched.poll = 20 * time.Millisecond
	e.fetch.push(feedURL, ok(feed(item("a", 1, "A")), ""))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- e.sched.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(e.sched.Events()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	evs := e.sched.Events()
	if len(evs) != 1 || evs[0].Outcome != OutcomeBaseline {
		t.Errorf("events = %+v, want exactly one baseline (source not due again for 15m)", evs)
	}
}
