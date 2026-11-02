// Package scheduler runs the check pipeline: it finds sources that are due,
// fetches them, normalizes and diffs the result, and stores snapshots and
// changes. Summarizing and notifying are a separate stage driven by the
// changes table (the outbox pattern, see docs/ARCHITECTURE.md §7), so
// nothing here talks to the LLM or Telegram.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/differ"
	"github.com/akkuraina/pehredaar/internal/fetcher"
	"github.com/akkuraina/pehredaar/internal/normalize"
	"github.com/akkuraina/pehredaar/internal/store"
)

// ErrBusy is returned by CheckNow when the source is already being checked.
var ErrBusy = errors.New("scheduler: source is already being checked")

// Fetcher is the part of *fetcher.Fetcher the scheduler uses. Declaring it
// here (the consumer) lets tests substitute a fake without a network.
type Fetcher interface {
	Fetch(ctx context.Context, req fetcher.Request) (*fetcher.Response, error)
}

// Config holds the scheduler's dependencies and settings.
type Config struct {
	Store      store.Store
	Fetcher    Fetcher
	Normalizer *normalize.Normalizer
	Sources    config.Sources
	Workers    int           // concurrent checks; default 4
	Poll       time.Duration // how often to look for due sources; default 10s
	Logger     *slog.Logger
}

// Scheduler polls sources. Create it with New; it is safe for concurrent
// use (Run and CheckNow may be called at the same time).
type Scheduler struct {
	store      store.Store
	fetch      Fetcher
	norm       *normalize.Normalizer
	curated    map[string]config.Source // yaml-only settings by URL
	discovered time.Duration
	workers    int
	poll       time.Duration
	log        *slog.Logger

	// Replaceable in tests.
	now    func() time.Time
	jitter func(time.Duration) time.Duration

	mu       sync.Mutex
	inFlight map[int64]bool // source IDs being checked right now
	events   []Event        // ring of recent checks, newest last
}

const (
	maxEvents  = 300
	maxBackoff = 6 * time.Hour
)

// New returns a Scheduler with defaults filled in.
func New(cfg Config) *Scheduler {
	s := &Scheduler{
		store:      cfg.Store,
		fetch:      cfg.Fetcher,
		norm:       cfg.Normalizer,
		curated:    cfg.Sources.ByURL(),
		discovered: cfg.Sources.DiscoveredInterval,
		workers:    cfg.Workers,
		poll:       cfg.Poll,
		log:        cfg.Logger,
		now:        time.Now,
		jitter:     tenPercent,
		inFlight:   make(map[int64]bool),
	}
	if s.workers <= 0 {
		s.workers = 4
	}
	if s.poll <= 0 {
		s.poll = 10 * time.Second
	}
	if s.discovered <= 0 {
		s.discovered = 6 * time.Hour
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// tenPercent spreads d by ±10%, so sources added together (like the 12
// feeds at first start) drift apart instead of all firing at once forever.
func tenPercent(d time.Duration) time.Duration {
	spread := int64(d) / 5
	if spread <= 0 {
		return d
	}
	return d - time.Duration(spread/2) + time.Duration(rand.Int64N(spread))
}

// Run checks due sources until ctx is cancelled, then waits for in-flight
// checks to stop and returns nil.
//
// Shape: one dispatcher (this goroutine) feeds an unbuffered channel read by
// a fixed pool of workers. The pool bounds concurrency; the fetcher's
// per-host rate limit bounds how hard we hit any one site.
func (s *Scheduler) Run(ctx context.Context) error {
	jobs := make(chan store.Source)
	var wg sync.WaitGroup
	for range s.workers {
		wg.Go(func() {
			for src := range jobs {
				s.check(ctx, src)
				s.release(src.ID)
			}
		})
	}
	// Closing jobs ends the workers' range loops; Wait lets in-flight checks
	// notice the cancelled ctx and return before Run does.
	defer func() {
		close(jobs)
		wg.Wait()
	}()

	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		if err := s.dispatch(ctx, jobs); err != nil && ctx.Err() == nil {
			s.log.Error("finding due sources", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Scheduler) dispatch(ctx context.Context, jobs chan<- store.Source) error {
	due, err := s.store.DueSources(ctx, s.now(), 100)
	if err != nil {
		return err
	}
	for _, src := range due {
		if !s.claim(src.ID) {
			continue // still running from an earlier round or a CheckNow
		}
		select {
		case jobs <- src:
		case <-ctx.Done():
			s.release(src.ID)
			return nil
		}
	}
	return nil
}

func (s *Scheduler) claim(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[id] {
		return false
	}
	s.inFlight[id] = true
	return true
}

func (s *Scheduler) release(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, id)
}

// CheckNow checks one source immediately, whether or not it is due, and
// returns what happened. Used by the dashboard's "check now" button.
func (s *Scheduler) CheckNow(ctx context.Context, id int64) (Event, error) {
	src, err := s.store.Source(ctx, id)
	if err != nil {
		return Event{}, err
	}
	if !s.claim(id) {
		return Event{}, ErrBusy
	}
	defer s.release(id)
	return s.check(ctx, src), nil
}

// check runs the pipeline for one source and records the outcome on the
// source row (validators, schedule, failure count) and in the event log.
func (s *Scheduler) check(ctx context.Context, src store.Source) Event {
	start := s.now()
	ev, v, err := s.pipeline(ctx, src)
	ev.Time, ev.SourceID, ev.Label = start, src.ID, displayLabel(src)
	ev.Duration = s.now().Sub(start)

	if ctx.Err() != nil {
		// Shutting down: not the source's fault, so don't count a failure.
		ev.Outcome, ev.Detail = OutcomeCancelled, ""
		return ev
	}

	res := store.CheckResult{CheckedAt: start}
	if err != nil {
		ev.Outcome, ev.Detail = OutcomeFailed, err.Error()
		res.Err = err.Error()
		res.NextCheckAt = start.Add(s.backoff(src, err))
	} else {
		res.ETag, res.LastModified = v.etag, v.lastModified
		res.NextCheckAt = start.Add(s.jitter(src.CheckInterval))
	}
	if err := s.store.RecordCheck(ctx, src.ID, res); err != nil {
		s.log.Error("recording check", "source", src.ID, "err", err)
	}
	s.record(ev)
	return ev
}

type validators struct{ etag, lastModified string }

// pipeline is fetch → normalize → compare → save. It returns the outcome
// and, on success, the HTTP validators to send next time. Validators are
// only stored after the snapshot is safely saved; otherwise a crash between
// the two could make the next check a 304 for content we never stored.
func (s *Scheduler) pipeline(ctx context.Context, src store.Source) (Event, validators, error) {
	var ev Event
	resp, err := s.fetch.Fetch(ctx, fetcher.Request{
		URL: src.URL, ETag: src.ETag, LastModified: src.LastModified,
	})
	if err != nil {
		return ev, validators{}, err
	}
	v := validators{resp.ETag, resp.LastModified}
	if resp.NotModified {
		ev.Outcome = OutcomeNotModified
		return ev, v, nil
	}

	cfg := s.curated[src.URL] // zero value (no rules) for discovered PDFs
	doc, err := s.norm.Normalize(ctx, src, resp.Body, cfg.Rules)
	if err != nil {
		return ev, validators{}, err
	}

	prev, err := s.store.LatestSnapshot(ctx, src.ID)
	snap := store.Snapshot{SourceID: src.ID, ContentHash: doc.Hash, Text: doc.Text, FetchedAt: resp.FetchedAt}
	switch {
	case errors.Is(err, store.ErrNotFound):
		// First sight is a baseline, never an alert (§6).
		if _, _, err := s.store.SaveSnapshot(ctx, snap, nil); err != nil {
			return ev, validators{}, err
		}
		ev.Outcome = OutcomeBaseline
		ev.Detail = fmt.Sprintf("%d lines", strings.Count(doc.Text, "\n")+1)
	case err != nil:
		return ev, validators{}, err
	case prev.ContentHash == doc.Hash:
		ev.Outcome = OutcomeUnchanged
		return ev, v, nil
	default:
		r, err := differ.Diff(src.Kind, prev.Text, doc.Text, differ.Options{Windowed: cfg.Windowed})
		if err != nil {
			return ev, validators{}, err
		}
		var change *store.Change
		if !r.Empty() {
			change = &store.Change{OldSnapshotID: prev.ID, Kind: r.Kind, Diff: r.Text}
		}
		// The new snapshot is saved even for pure noise, so the next diff
		// starts from what the site shows now.
		_, saved, err := s.store.SaveSnapshot(ctx, snap, change)
		if err != nil {
			return ev, validators{}, err
		}
		if saved == nil {
			ev.Outcome = OutcomeNoise
			ev.Detail = fmt.Sprintf("%d noise blocks ignored", r.Ignored)
		} else {
			ev.Outcome, ev.ChangeID = OutcomeChanged, saved.ID
			ev.Detail = firstLine(r.Text)
		}
	}

	if src.Kind == store.KindFeed && cfg.DiscoverAttachments {
		n, err := s.discover(ctx, src, doc)
		if err != nil {
			return ev, validators{}, err
		}
		if n > 0 {
			ev.Detail += fmt.Sprintf(" (watching %d attachments)", n)
		}
	}
	return ev, v, nil
}

// discover makes every attachment of a feed's current items a pdf source,
// so corrigenda edited in place inside a PDF are caught too. Upsert is
// idempotent, so re-discovering a known PDF is harmless. Only attachments
// on the feed's own host are followed: this is not "watch any URL".
func (s *Scheduler) discover(ctx context.Context, feed store.Source, doc normalize.Doc) (int, error) {
	feedURL, err := url.Parse(feed.URL)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range doc.Items {
		for _, a := range it.Attachments {
			u, err := url.Parse(a.URL)
			if err != nil || u.Host != feedURL.Host {
				continue
			}
			_, err = s.store.UpsertSource(ctx, store.Source{
				URL:           a.URL,
				Kind:          store.KindPDF,
				Category:      feed.Category,
				Language:      feed.Language,
				Label:         truncate(it.Headline, 120),
				ParentID:      feed.ID,
				CheckInterval: s.discovered,
				Enabled:       true,
			})
			if err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// backoff decides when to retry a failed source. Failures never alert
// anyone; they only slow polling down, so a site that is down for a day
// costs a handful of requests, not hundreds.
func (s *Scheduler) backoff(src store.Source, err error) time.Duration {
	var fe *fetcher.Error
	if errors.As(err, &fe) {
		switch fe.Kind {
		case fetcher.Permanent, fetcher.Blocked, fetcher.Disallowed:
			// A 404/403 rarely fixes itself within minutes.
			return s.jitter(maxBackoff)
		}
	}
	// Exponential: interval, 2x, 4x, ... capped. ConsecutiveFailures is the
	// count before this failure.
	d := src.CheckInterval << min(src.ConsecutiveFailures, 10)
	d = min(d, maxBackoff)
	if fe != nil && fe.RetryAfter > d {
		d = min(fe.RetryAfter, maxBackoff)
	}
	return s.jitter(d)
}

func displayLabel(src store.Source) string {
	if src.Label != "" {
		return src.Label
	}
	return src.URL
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return truncate(line, 200)
}

// truncate shortens s to at most n runes (not bytes, so Hindi text is
// never cut mid-character).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
