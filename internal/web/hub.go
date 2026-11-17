package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/akkuraina/pehredaar/internal/store"
)

// Hub watches the store for new changes and fresh stats and fans them out
// to every open page over Server-Sent Events.
//
// Go note: one goroutine polls the database and broadcasts; each browser
// connection gets its own buffered channel. Polling every few seconds from
// one place is cheaper than every connection polling, and needs no new
// plumbing from the scheduler or outbox.
type Hub struct {
	st  store.Store
	log *slog.Logger

	mu      sync.Mutex
	clients map[chan hubEvent]struct{}
	lastID  int64
	stats   store.Stats
}

type hubEvent struct {
	change *store.FeedEntry // a new change, or
	stats  *store.Stats     // fresh numbers for the "watching now" pulse
}

const (
	hubPoll    = 3 * time.Second
	maxClients = 2000
	heartbeat  = 25 * time.Second
)

func newHub(st store.Store, log *slog.Logger) *Hub {
	return &Hub{st: st, log: log, clients: make(map[chan hubEvent]struct{})}
}

// Run polls until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) error {
	// Start from the newest existing change: pages render history
	// themselves; the hub only announces what's new.
	if latest, err := h.st.FeedChanges(ctx, store.FeedQuery{Limit: 1}); err == nil && len(latest) > 0 {
		h.lastID = latest[0].Change.ID
	}
	t := time.NewTicker(hubPoll)
	defer t.Stop()
	for {
		h.poll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (h *Hub) poll(ctx context.Context) {
	entries, err := h.st.FeedChanges(ctx, store.FeedQuery{AfterID: h.lastID, Limit: 20})
	if err != nil {
		if ctx.Err() == nil {
			h.log.Warn("web hub: polling changes", "err", err)
		}
		return
	}
	// FeedChanges is newest first; announce oldest first so pages that
	// prepend end up newest on top.
	slices.Reverse(entries)
	for i := range entries {
		e := entries[i]
		// A pending change has no summary yet. Hold it back until the
		// outbox has summarized it (normally seconds), so the live card
		// says something human; it shows up on a later poll. After two
		// minutes, show it anyway (with the plain template) rather than
		// let one stuck change stall the ticker.
		if e.Change.Status == store.StatusPending && time.Since(e.Change.CreatedAt) < 2*time.Minute {
			break
		}
		h.lastID = e.Change.ID
		h.broadcast(hubEvent{change: &e})
	}
	if st, err := h.st.Stats(ctx); err == nil {
		h.mu.Lock()
		changed := st != h.stats
		h.stats = st
		h.mu.Unlock()
		if changed {
			h.broadcast(hubEvent{stats: &st})
		}
	}
}

func (h *Hub) broadcast(ev hubEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c <- ev:
		default: // a slow client misses an update rather than stalling everyone
		}
	}
}

func (h *Hub) subscribe() (chan hubEvent, func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= maxClients {
		return nil, nil, false
	}
	c := make(chan hubEvent, 16)
	h.clients[c] = struct{}{}
	return c, func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
	}, true
}

func (h *Hub) currentStats() store.Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stats
}

// events streams live updates: "change" events carry a rendered card,
// "pulse" events the latest stats. ?cats=a,b limits changes to those
// categories (the personal feed); without it every change is sent (the
// landing page). Everything sent is public information.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	ch, unsubscribe, ok := s.hub.subscribe()
	if !ok {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	defer unsubscribe()

	var cats map[string]bool
	if v := r.URL.Query().Get("cats"); v != "" {
		cats = make(map[string]bool)
		for c := range strings.SplitSeq(v, ",") {
			cats[c] = true
		}
	}
	lang := store.LangEnglish
	if r.URL.Query().Get("lang") == string(store.LangHindi) {
		lang = store.LangHindi
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // tell nginx not to buffer
	// Tell the browser to wait 5s before reconnecting after a drop.
	if _, err := w.Write([]byte("retry: 5000\n\n")); err != nil {
		return
	}
	if st := s.hub.currentStats(); st != (store.Stats{}) {
		s.sendPulse(w, st)
	}
	rc.Flush()

	beat := time.NewTicker(heartbeat)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			// A comment line keeps proxies from closing an idle stream.
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
		case ev := <-ch:
			switch {
			case ev.stats != nil:
				s.sendPulse(w, *ev.stats)
			case ev.change != nil:
				if cats != nil && !cats[ev.change.Source.Category] {
					continue
				}
				// The hub's entry carries the English summary; fetch the
				// reader's language if it differs.
				e := *ev.change
				if lang != store.LangEnglish {
					if t, err := s.st.Summary(r.Context(), e.Change.ID, lang); err == nil {
						e.Summary = t
					}
				}
				var b strings.Builder
				if err := s.tpl.ExecuteTemplate(&b, "card", s.cardFor(r.Context(), e, lang)); err != nil {
					s.log.Error("web: rendering live card", "err", err)
					continue
				}
				writeEvent(w, "change", map[string]any{"id": e.Change.ID, "html": b.String()})
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

func (s *Server) sendPulse(w http.ResponseWriter, st store.Stats) {
	var last int64
	if !st.LastCheckAt.IsZero() {
		last = st.LastCheckAt.Unix()
	}
	writeEvent(w, "pulse", map[string]any{
		"feeds": st.Feeds, "sources": st.Sources, "caught": st.ChangesCaught, "lastCheck": last,
	})
}

func writeEvent(w http.ResponseWriter, name string, v any) {
	data, _ := json.Marshal(v) // maps of plain values can't fail
	w.Write([]byte("event: " + name + "\ndata: " + string(data) + "\n\n"))
}
