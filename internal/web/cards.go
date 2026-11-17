package web

import (
	"context"
	"fmt"
	"html/template"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/akkuraina/pehredaar/internal/differ"
	"github.com/akkuraina/pehredaar/internal/exams"
	"github.com/akkuraina/pehredaar/internal/normalize"
	"github.com/akkuraina/pehredaar/internal/notifier"
	"github.com/akkuraina/pehredaar/internal/outbox"
	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/summarizer"
)

// card is one caught change, as the feed and the landing ticker show it.
type card struct {
	ID         int64
	Exam       string // "SSC"
	Feed       string // "SSC Notice Board"
	Kind       string // importance: date, eligibility, fee, other
	KindLabel  string
	Headline   string
	Details    []detail
	Links      []notifier.Link
	Caught     time.Time
	Hindi      bool
	PDFChange  bool // the change is inside a document, not the feed listing
	DocLabel   string
	ShowSource bool
}

// detail is one line under a card's headline. Lines of the form
// "label: old → new" are split so the page can animate old into new.
type detail struct {
	Text     string
	Label    string
	Old, New string
	IsChange bool
}

var kindLabels = map[store.Importance]string{
	store.ImportanceDate:        "Date change",
	store.ImportanceEligibility: "Eligibility",
	store.ImportanceFee:         "Fee",
	store.ImportanceOther:       "Update",
	store.ImportanceUnknown:     "New",
}

// cardFor turns a stored change into a card. Changes still waiting for
// their summary get the plain template's text, which needs no LLM.
func (s *Server) cardFor(ctx context.Context, e store.FeedEntry, lang store.Language) card {
	text := e.Summary
	if text == "" {
		if sum, err := (summarizer.Template{}).Summarize(ctx, summarizer.Input{
			Change: e.Change, Source: e.Source, Lang: lang,
		}); err == nil {
			text = sum.Text
		}
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	c := card{
		ID:        e.Change.ID,
		Feed:      s.feedLabel(e.Source.Category),
		Kind:      string(e.Change.Importance),
		KindLabel: kindLabels[e.Change.Importance],
		Headline:  lines[0],
		Links:     outbox.AlertLinks(e.Source, e.Change.Diff),
		Caught:    e.Change.CreatedAt,
		Hindi:     lang == store.LangHindi,
		PDFChange: e.Source.Kind == store.KindPDF,
		DocLabel:  e.Source.Label,
	}
	if c.Kind == "" {
		c.Kind = "new"
	}
	if ex, ok := exams.ForCategory(e.Source.Category); ok {
		c.Exam = ex.Name
	}
	for _, l := range lines[1:] {
		if l = strings.TrimSpace(l); l != "" {
			c.Details = append(c.Details, parseDetail(l))
		}
	}
	return c
}

// parseDetail recognises the template's "• Label: old → new" lines.
func parseDetail(line string) detail {
	d := detail{Text: strings.TrimSpace(strings.TrimLeft(line, "•-* "))}
	old, nw, ok := strings.Cut(d.Text, " → ")
	if !ok {
		return d
	}
	d.IsChange, d.Old, d.New = true, old, nw
	if label, rest, ok := strings.Cut(old, ": "); ok && len(label) < 40 {
		d.Label, d.Old = label, rest
	}
	return d
}

func (s *Server) feedLabel(category string) string {
	if l, ok := s.feedLabels[category]; ok {
		return l
	}
	return category
}

// notice is one item currently listed on a watched feed: the "latest
// notices" shelf of the personal feed. It comes straight from the latest
// snapshot, so it is exactly what the official feed lists right now.
type notice struct {
	Category string
	Exam     string
	Feed     string
	Headline string
	Date     time.Time
	Links    []notifier.Link
}

// latestNotices returns the newest items of every curated feed, cached
// for a minute (they change only when a snapshot does).
func (s *Server) latestNotices(ctx context.Context) []notice {
	s.noticesMu.Lock()
	defer s.noticesMu.Unlock()
	if s.notices != nil && s.now().Sub(s.noticesAt) < time.Minute {
		return s.notices
	}
	srcs, err := s.st.Sources(ctx)
	if err != nil {
		s.log.Warn("web: listing sources", "err", err)
		return s.notices
	}
	var out []notice
	for _, src := range srcs {
		// English feeds only: the Hindi feeds repeat them with a lag.
		if src.ParentID != 0 || !src.Enabled || src.Kind != store.KindFeed || src.Language != store.LangEnglish {
			continue
		}
		snap, err := s.st.LatestSnapshot(ctx, src.ID)
		if err != nil {
			continue
		}
		out = append(out, noticesFrom(src, snap.Text, s.feedLabel(src.Category), 6)...)
	}
	slices.SortStableFunc(out, func(a, b notice) int { return b.Date.Compare(a.Date) })
	s.notices, s.noticesAt = out, s.now()
	return out
}

// noticesFrom reads up to n items from a feed's canonical text. Each line
// has the same shape as an "added" line of a feed diff, so the differ's
// parser (the single owner of that format) reads it.
func noticesFrom(src store.Source, text string, feed string, n int) []notice {
	var diff strings.Builder
	for line := range strings.SplitSeq(text, "\n") {
		if line != "" {
			diff.WriteString("+ " + line + "\n")
		}
	}
	blocks, _ := differ.ParseFeed(diff.String())
	exam := ""
	if ex, ok := exams.ForCategory(src.Category); ok {
		exam = ex.Name
	}
	var out []notice
	for _, b := range blocks {
		if len(out) == n {
			break
		}
		nt := notice{Category: src.Category, Exam: exam, Feed: feed, Headline: b.Headline}
		// The canonical line is "id | createdAt | headline | fields";
		// ParseFeed keeps createdAt out of Fields, so read it here.
		if _, rest, ok := strings.Cut(strings.TrimPrefix(firstLineWithID(text, b.ID), b.ID), " | "); ok {
			date, _, _ := strings.Cut(rest, " | ")
			nt.Date, _ = time.Parse(time.RFC3339, date)
		}
		for _, f := range b.Fields {
			if link, ok := normalize.AttachmentLink(src.URL, f); ok {
				nt.Links = append(nt.Links, notifier.Link{URL: link, PDF: strings.HasSuffix(strings.ToLower(link), ".pdf")})
			}
		}
		out = append(out, nt)
	}
	return out
}

func firstLineWithID(text, id string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, id+" | ") {
			return line
		}
	}
	return ""
}

// --- template helpers ------------------------------------------------------------

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"ago":     func(t time.Time) string { return ago(s.now(), t) },
		"iso":     func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"day":     func(t time.Time) string { return t.In(ist).Format("2 Jan 2006") },
		"join":    strings.Join,
		"hasText": func(s string) bool { return strings.TrimSpace(s) != "" },
		"asset":   assetURL,
	}
}

var ist = time.FixedZone("IST", 5*3600+1800)

// ago is "just now", "4 min ago", "3 h ago", "2 days ago" or a date. The
// page's script keeps it current; this is the no-JS starting point.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	case d < 7*24*time.Hour:
		n := int(d / (24 * time.Hour))
		if n == 1 {
			return "yesterday"
		}
		return fmt.Sprintf("%d days ago", n)
	}
	return t.In(ist).Format("2 Jan 2006")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
