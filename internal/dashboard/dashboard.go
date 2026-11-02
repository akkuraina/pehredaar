// Package dashboard serves a small, local, read-mostly web UI for the
// operator: what is being watched, what each check concluded, and every
// change with its diff. It is not the user interface (that's Telegram) and
// has no login, so it must only ever listen on localhost.
//
// It is plain net/http + html/template with no JavaScript framework: pages
// are rendered on the server, and the overview simply reloads itself.
package dashboard

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/akkuraina/pehredaar/internal/scheduler"
	"github.com/akkuraina/pehredaar/internal/store"
)

// Store is the slice of store.Store the dashboard reads. Declaring it here
// documents that the dashboard never writes to the database.
type Store interface {
	Sources(ctx context.Context) ([]store.Source, error)
	Source(ctx context.Context, id int64) (store.Source, error)
	Snapshot(ctx context.Context, id int64) (store.Snapshot, error)
	Snapshots(ctx context.Context, sourceID int64, limit int) ([]store.Snapshot, error)
	LatestSnapshot(ctx context.Context, sourceID int64) (store.Snapshot, error)
	Change(ctx context.Context, id int64) (store.Change, error)
	RecentChanges(ctx context.Context, sourceID int64, limit int) ([]store.Change, error)
}

// Checker is the slice of *scheduler.Scheduler the dashboard uses.
type Checker interface {
	CheckNow(ctx context.Context, id int64) (scheduler.Event, error)
	Events() []scheduler.Event
	Running() map[int64]bool
}

//go:embed templates/*.html
var templateFS embed.FS

// Server renders the dashboard pages.
type Server struct {
	store Store
	check Checker
	log   *slog.Logger
	pages map[string]*template.Template
	now   func() time.Time
}

// New returns the dashboard as an http.Handler.
func New(st Store, ch Checker, log *slog.Logger) (http.Handler, error) {
	s := &Server{store: st, check: ch, log: log, now: time.Now, pages: make(map[string]*template.Template)}

	// Each page is parsed together with the shared layout, giving one
	// template set per page. (One set for all pages would make their
	// {{define "content"}} blocks overwrite each other.)
	for _, page := range []string{"overview", "source", "change", "snapshot"} {
		t, err := template.New("").Funcs(s.funcs()).ParseFS(templateFS,
			"templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("dashboard: parse %s: %w", page, err)
		}
		s.pages[page] = t
	}

	mux := http.NewServeMux()
	// Go 1.22+ patterns: method and {wildcards} are matched by the mux.
	mux.HandleFunc("GET /{$}", s.overview)
	mux.HandleFunc("GET /sources/{id}", s.source)
	mux.HandleFunc("POST /sources/{id}/check", s.checkNow)
	mux.HandleFunc("GET /changes/{id}", s.change)
	mux.HandleFunc("GET /snapshots/{id}", s.snapshot)

	// Rejects cross-site POSTs, so a web page you visit can't press
	// "check now" on your behalf (Go 1.25+, uses Sec-Fetch-Site/Origin).
	return http.NewCrossOriginProtection().Handler(mux), nil
}

// render executes a page into a buffer first, so a template error becomes
// a clean 500 instead of half a page.
func (s *Server) render(w http.ResponseWriter, page string, data any) {
	var b strings.Builder
	if err := s.pages[page].ExecuteTemplate(&b, "layout", data); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, b.String())
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.log.Error("dashboard", "err", err)
	http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad id %q: %w", r.PathValue("id"), store.ErrNotFound)
	}
	return id, nil
}

// --- view models -------------------------------------------------------------

// sourceRow is a source plus what the template needs to show its status.
type sourceRow struct {
	store.Source
	Running bool
	Parent  string // parent feed label, for discovered PDFs
}

func (r sourceRow) Status() string {
	switch {
	case r.Running:
		return "running"
	case !r.Enabled:
		return "disabled"
	case r.LastCheckedAt.IsZero():
		return "new"
	case r.ConsecutiveFailures > 0:
		return "failing"
	}
	return "ok"
}

type changeRow struct {
	store.Change
	Source string // label
}

func (c changeRow) Headline() string {
	line, _, _ := strings.Cut(c.Diff, "\n")
	return line
}

// --- handlers ----------------------------------------------------------------

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, err := s.store.Sources(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	labels := labelsByID(all)
	running := s.check.Running()

	var feeds, pdfs []sourceRow
	failing := 0
	for _, src := range all {
		row := sourceRow{Source: src, Running: running[src.ID], Parent: labels[src.ParentID]}
		if row.Status() == "failing" {
			failing++
		}
		if src.ParentID == 0 {
			feeds = append(feeds, row)
		} else {
			pdfs = append(pdfs, row)
		}
	}
	// Most recently checked PDFs first: that's where activity is.
	slices.SortStableFunc(pdfs, func(a, b sourceRow) int { return b.LastCheckedAt.Compare(a.LastCheckedAt) })

	changes, err := s.store.RecentChanges(ctx, 0, 30)
	if err != nil {
		s.fail(w, err)
		return
	}

	events := s.check.Events()
	counts := map[string]int{} // string keys: templates index maps with plain strings
	for _, ev := range events {
		counts[string(ev.Outcome)]++
	}

	s.render(w, "overview", map[string]any{
		"Title":   "Overview",
		"Refresh": 30,
		"Feeds":   feeds,
		"PDFs":    pdfs,
		"Failing": failing,
		"Changes": changeRows(changes, labels),
		"Events":  events[:min(len(events), 60)],
		"Counts":  counts,
		"Checks":  len(events),
	})
}

func (s *Server) source(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	src, err := s.store.Source(ctx, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	all, err := s.store.Sources(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	labels := labelsByID(all)
	running := s.check.Running()

	var children []sourceRow
	for _, c := range all {
		if c.ParentID == id {
			children = append(children, sourceRow{Source: c, Running: running[c.ID]})
		}
	}
	snaps, err := s.store.Snapshots(ctx, id, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	latest, err := s.store.LatestSnapshot(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	changes, err := s.store.RecentChanges(ctx, id, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	var events []scheduler.Event
	for _, ev := range s.check.Events() {
		if ev.SourceID == id {
			events = append(events, ev)
		}
	}

	s.render(w, "source", map[string]any{
		"Title":    src.Label,
		"Source":   sourceRow{Source: src, Running: running[id], Parent: labels[src.ParentID]},
		"Children": children,
		"Snaps":    snaps,
		"Latest":   latest,
		"Changes":  changeRows(changes, labels),
		"Events":   events,
		"Flash":    r.URL.Query().Get("flash"),
	})
}

func (s *Server) checkNow(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The check runs while the browser waits: usually a second or two, up
	// to the per-host rate-limit gap if the scheduler is busy with the
	// same site. The timeout keeps a hung site from hanging the page.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	var flash string
	ev, err := s.check.CheckNow(ctx, id)
	switch {
	case errors.Is(err, scheduler.ErrBusy):
		flash = "Already being checked; refresh in a moment."
	case err != nil:
		s.fail(w, err)
		return
	default:
		flash = fmt.Sprintf("Checked in %v: %s", ev.Duration.Round(time.Millisecond), ev.Outcome)
		if ev.Detail != "" {
			flash += " (" + ev.Detail + ")"
		}
	}
	// Post/Redirect/Get: reloading the result page won't re-submit the check.
	http.Redirect(w, r, fmt.Sprintf("/sources/%d?flash=%s", id, url.QueryEscape(flash)), http.StatusSeeOther)
}

func (s *Server) change(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	ch, err := s.store.Change(ctx, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	src, err := s.store.Source(ctx, ch.SourceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "change", map[string]any{
		"Title":  fmt.Sprintf("Change #%d", id),
		"Change": ch,
		"Source": src,
	})
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	snap, err := s.store.Snapshot(ctx, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	src, err := s.store.Source(ctx, snap.SourceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "snapshot", map[string]any{
		"Title":  fmt.Sprintf("Snapshot #%d", id),
		"Snap":   snap,
		"Source": src,
		"Lines":  strings.Count(snap.Text, "\n") + 1,
	})
}

func labelsByID(all []store.Source) map[int64]string {
	m := make(map[int64]string, len(all))
	for _, s := range all {
		m[s.ID] = s.Label
	}
	return m
}

func changeRows(chs []store.Change, labels map[int64]string) []changeRow {
	out := make([]changeRow, len(chs))
	for i, c := range chs {
		out[i] = changeRow{Change: c, Source: labels[c.SourceID]}
	}
	return out
}
