package web

import (
	"bytes"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/akkuraina/pehredaar/internal/exams"
	"github.com/akkuraina/pehredaar/internal/store"
)

// base is what every page template gets.
type base struct {
	Title    string
	Signed   bool
	Page     string // which page, for nav highlighting and per-page JS
	Year     int
	EventsQS string // query for /events; empty: no live updates on this page
}

// pageData serves the simple pages (messages, errors, privacy).
type pageData struct {
	base
	Heading string
	Body    string
	Mood    string // owl expression: "happy", "alert", "sleepy"
	Action  *link
}

type link struct{ URL, Label string }

func (s *Server) base(r *http.Request, title, page string) base {
	_, signed := s.current(r)
	return base{Title: title, Signed: signed, Page: page, Year: s.now().In(ist).Year()}
}

// render executes a page template into a buffer first, so a template
// error becomes a clean 500 instead of half a page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	if pd, ok := data.(pageData); ok && pd.Page == "" {
		b := s.base(r, pd.Title, name)
		b.Title = pd.Title
		pd.base = b
		data = pd
	}
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("web: rendering", "template", name, "err", err)
		http.Error(w, "Something went wrong on our side.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *Server) oops(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("web: request failed", "path", r.URL.Path, "err", err)
	s.render(w, r, http.StatusInternalServerError, "message", pageData{
		Title: "Something broke", Heading: "I tripped over a branch 🦉", Mood: "sleepy",
		Body: "Something went wrong on our side. Please try again in a minute.",
	})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "message", pageData{
		Title: "Not found", Heading: "Nothing here, I looked everywhere 👀", Mood: "alert",
		Body: "That link doesn't lead anywhere. It may have expired.", Action: &link{"/", "Back home"},
	})
}

// --- landing --------------------------------------------------------------------

type examView struct {
	exams.Exam
	Live bool
}

type landingData struct {
	base
	Stats     store.Stats
	Cards     []card
	Spotlight *card // the latest catch with an old → new pair, if any
	Exams     []examView
}

func (s *Server) categoryIDs() []string {
	ids := make([]string, len(s.cfg.Categories))
	for i, c := range s.cfg.Categories {
		ids[i] = c.ID
	}
	return ids
}

func (s *Server) examViews() []examView {
	cats := s.categoryIDs()
	out := make([]examView, len(exams.Catalog))
	for i, e := range exams.Catalog {
		out[i] = examView{Exam: e, Live: e.Live(cats)}
	}
	return out
}

func (s *Server) landing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := s.st.Stats(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	entries, err := s.st.FeedChanges(ctx, store.FeedQuery{Limit: 8})
	if err != nil {
		s.oops(w, r, err)
		return
	}
	d := landingData{base: s.base(r, "Pehredaar: the owl that watches exam notices", "landing"), Stats: stats, Exams: s.examViews()}
	d.EventsQS = "?all=1"
	for _, e := range entries {
		c := s.cardFor(ctx, e, store.LangEnglish)
		d.Cards = append(d.Cards, c)
		if d.Spotlight == nil && slices.ContainsFunc(c.Details, func(x detail) bool { return x.IsChange }) {
			cc := c
			d.Spotlight = &cc
		}
	}
	s.render(w, r, http.StatusOK, "landing", d)
}

// --- personal feed ------------------------------------------------------------

type feedData struct {
	base
	Sub        store.Subscriber
	Contacts   []contactView
	Watching   []string // feed labels
	Cards      []card
	Notices    []notice
	ComingSoon []exams.Exam
	MoreBefore int64 // id for "load more"; 0: nothing more
	BotLink    string
	Notice     string // one-off banner, e.g. after confirming email
}

type contactView struct {
	Channel  string
	Address  string
	Verified bool
	Active   bool
}

const feedPage = 12

func (s *Server) feed(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	cats, err := s.subscribedCategories(r, sub.ID)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	d := feedData{base: s.base(r, "Your watch · Pehredaar", "feed"), Sub: sub}
	for _, c := range cats {
		d.Watching = append(d.Watching, s.feedLabel(c))
	}
	if len(cats) > 0 {
		d.EventsQS = "?cats=" + strings.Join(cats, ",") + "&lang=" + string(sub.Language)
		entries, err := s.st.FeedChanges(ctx, store.FeedQuery{Categories: cats, Lang: sub.Language, Limit: feedPage})
		if err != nil {
			s.oops(w, r, err)
			return
		}
		for _, e := range entries {
			d.Cards = append(d.Cards, s.cardFor(ctx, e, sub.Language))
		}
		if len(entries) == feedPage {
			d.MoreBefore = entries[len(entries)-1].Change.ID
		}
		want := make(map[string]bool, len(cats))
		for _, c := range cats {
			want[c] = true
		}
		for _, n := range s.latestNotices(ctx) {
			if want[n.Category] && len(d.Notices) < 12 {
				d.Notices = append(d.Notices, n)
			}
		}
	}

	contacts, err := s.st.Contacts(ctx, sub.ID)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	for _, c := range contacts {
		cv := contactView{Channel: string(c.Channel), Verified: c.Verified(), Active: c.Active}
		if c.Channel == store.ChannelEmail {
			cv.Address = c.Address
		}
		d.Contacts = append(d.Contacts, cv)
	}
	interests, err := s.st.Interests(ctx, sub.ID)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	live := s.categoryIDs()
	for _, id := range interests {
		if e, ok := exams.ByID(id); ok && !e.Live(live) {
			d.ComingSoon = append(d.ComingSoon, e)
		}
	}
	if !slices.ContainsFunc(contacts, func(c store.Contact) bool { return c.Channel == store.ChannelTelegram }) {
		d.BotLink = s.botLink("")
	}
	switch r.URL.Query().Get("m") {
	case "confirmed":
		d.Notice = "Email confirmed. I'll write the moment something changes. 🦉"
	case "linked":
		d.Notice = "Telegram linked. Alerts will come there too."
	case "saved":
		d.Notice = "Saved. I've updated what I'm watching for you."
	case "check-email":
		d.Notice = "Almost there: open the email I just sent and tap the link to turn on email alerts."
	}
	s.render(w, r, http.StatusOK, "feed", d)
}

// feedMore returns the next page of cards as an HTML fragment, for the
// "show older" button.
func (s *Server) feedMore(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.current(r)
	if !ok {
		http.Error(w, "signed out", http.StatusUnauthorized)
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	cats, err := s.subscribedCategories(r, sub.ID)
	if err != nil || len(cats) == 0 || before <= 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	entries, err := s.st.FeedChanges(r.Context(), store.FeedQuery{
		Categories: cats, Lang: sub.Language, BeforeID: before, Limit: feedPage,
	})
	if err != nil {
		s.oops(w, r, err)
		return
	}
	var buf bytes.Buffer
	for _, e := range entries {
		if err := s.tpl.ExecuteTemplate(&buf, "card", s.cardFor(r.Context(), e, sub.Language)); err != nil {
			s.oops(w, r, err)
			return
		}
	}
	if len(entries) == feedPage {
		w.Header().Set("X-More-Before", itoa(entries[len(entries)-1].Change.ID))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) subscribedCategories(r *http.Request, subID int64) ([]string, error) {
	subs, err := s.st.Subscriptions(r.Context(), subID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, x := range subs {
		if x.Target.Category != "" {
			out = append(out, x.Target.Category)
		}
	}
	return out, nil
}

// --- one change -----------------------------------------------------------------

type changeData struct {
	base
	Card   card
	Diff   string
	Source store.Source
}

// changePage shows one change in full. Linked from alert emails and
// shareable: everything on it is public information.
func (s *Server) changePage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	entries, err := s.st.FeedChanges(ctx, store.FeedQuery{AfterID: id - 1, BeforeID: id + 1, Limit: 1})
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if len(entries) == 0 {
		s.notFound(w, r)
		return
	}
	lang := store.LangEnglish
	if sub, ok := s.current(r); ok {
		lang = sub.Language
		if t, err := s.st.Summary(ctx, id, lang); err == nil {
			entries[0].Summary = t
		} else if !errors.Is(err, store.ErrNotFound) {
			s.oops(w, r, err)
			return
		}
	}
	c := s.cardFor(ctx, entries[0], lang)
	d := changeData{base: s.base(r, c.Headline+" · Pehredaar", "change"), Card: c, Source: entries[0].Source}
	// The raw diff of a Hindi PDF is garbled pdftotext output; don't show it.
	if !(entries[0].Source.Kind == store.KindPDF && entries[0].Source.Language == store.LangHindi) {
		d.Diff = entries[0].Change.Diff
	}
	s.render(w, r, http.StatusOK, "change", d)
}

func (s *Server) privacy(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "privacy", pageData{Title: "Privacy · Pehredaar"})
}

// botLink is the Telegram deep link that opens the bot, with an optional
// /start payload. Empty if there is no bot.
func (s *Server) botLink(payload string) string {
	name := s.cfg.BotUsername()
	if name == "" {
		return ""
	}
	u := "https://t.me/" + name
	if payload != "" {
		u += "?start=" + payload
	}
	return u
}
