package summarizer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akkuraina/pehredaar/internal/differ"
	"github.com/akkuraina/pehredaar/internal/store"
)

// Template builds summaries from the stored diff with fixed phrases, no
// LLM. It can't judge importance, so everything is ImportanceOther: it
// never suppresses an alert.
type Template struct{}

// Limits that keep an alert readable on a phone.
const (
	maxBlocks   = 5   // feed items described; the rest become "and N more"
	maxPDFLines = 3   // removed/added lines quoted from a PDF diff, each
	maxLineLen  = 200 // runes per quoted line
)

func (Template) Summarize(_ context.Context, in Input) (Summary, error) {
	p := phrasesFor(in.Lang)
	var text string
	switch in.Source.Kind {
	case store.KindFeed:
		text = feedSummary(in.Change.Diff, p)
	default: // pdf, html
		text = docSummary(in.Change.Diff, in.Source, p)
	}
	return Summary{Text: text, Importance: store.ImportanceOther}, nil
}

func feedSummary(diff string, p phrases) string {
	blocks, more := differ.ParseFeed(diff)
	if len(blocks) > maxBlocks {
		more += len(blocks) - maxBlocks
		blocks = blocks[:maxBlocks]
	}
	var parts []string
	for _, b := range blocks {
		var s strings.Builder
		switch b.Op {
		case '+':
			s.WriteString(p.newNotice + ": " + b.Headline)
			for _, f := range b.Fields {
				if d := describeField(f, p); d != "" {
					s.WriteString("\n• " + d)
				}
			}
		case '~':
			s.WriteString(p.updatedNotice + ": " + b.Headline)
			for _, line := range describeEdit(b.Removed, b.Added, p) {
				s.WriteString("\n• " + line)
			}
		case '-':
			s.WriteString(p.removedNotice + ": " + b.Headline)
		}
		parts = append(parts, s.String())
	}
	if more > 0 {
		parts = append(parts, fmt.Sprintf(p.andMore, more))
	}
	return strings.Join(parts, "\n\n")
}

// docSummary describes a PDF change. Text from Hindi PDFs is garbled by
// pdftotext (broken matras), so it is never quoted; the reader is sent to
// the document instead.
func docSummary(diff string, src store.Source, p phrases) string {
	head := p.updatedDoc + ": " + src.Label
	if src.Language == store.LangHindi {
		return head + "\n" + p.openPDF
	}
	removed, added, _ := differ.ParseText(diff)
	var s strings.Builder
	s.WriteString(head)
	for _, l := range firstN(removed, maxPDFLines) {
		s.WriteString("\n• " + p.was + ": " + truncate(l, maxLineLen))
	}
	for _, l := range firstN(added, maxPDFLines) {
		s.WriteString("\n• " + p.now + ": " + truncate(l, maxLineLen))
	}
	if len(removed) > maxPDFLines || len(added) > maxPDFLines {
		s.WriteString("\n" + p.openPDF)
	}
	return s.String()
}

// field splits a canonical feed field into its label and value. Labels are
// the ones normalize writes ("start: ..."); the headline and createdAt
// fields have none.
func field(f string) (label, value string) {
	for _, l := range []string{"desc", "content", "start", "end", "link", "att"} {
		if v, ok := strings.CutPrefix(f, l+": "); ok {
			return l, v
		}
	}
	return "", f
}

// describeField renders one field of a new item, or "" for fields that
// don't belong in the text (attachments become links instead).
func describeField(f string, p phrases) string {
	label, value := field(f)
	name, ok := p.fieldNames[label]
	if !ok || label == "att" || label == "link" {
		return ""
	}
	return name + ": " + formatValue(value)
}

// describeEdit pairs removed and added fields by label, so a moved date
// reads "Start date: 1 Nov 2026 → 9 Nov 2026" rather than as two lines.
func describeEdit(removed, added []string, p phrases) []string {
	type pair struct{ old, new []string }
	var order []string
	byLabel := map[string]*pair{}
	get := func(label string) *pair {
		if byLabel[label] == nil {
			byLabel[label] = &pair{}
			order = append(order, label)
		}
		return byLabel[label]
	}
	for _, f := range removed {
		l, v := classify(f)
		get(l).old = append(get(l).old, v)
	}
	for _, f := range added {
		l, v := classify(f)
		get(l).new = append(get(l).new, v)
	}

	var out []string
	for _, l := range order {
		pr := byLabel[l]
		if l == "att" {
			// File names are long and mean little to a reader; say what
			// happened and let the link show the file.
			switch {
			case len(pr.old) > 0 && len(pr.new) > 0:
				out = append(out, p.attReplaced)
			case len(pr.new) > 0:
				out = append(out, p.attAdded)
			default:
				out = append(out, p.attRemoved)
			}
			continue
		}
		name := p.fieldNames[l]
		switch {
		case len(pr.old) > 0 && len(pr.new) > 0:
			out = append(out, fmt.Sprintf("%s: %s → %s", name, join(pr.old), join(pr.new)))
		case len(pr.new) > 0:
			out = append(out, fmt.Sprintf("%s: %s", name, join(pr.new)))
		default:
			out = append(out, fmt.Sprintf("%s: %s → —", name, join(pr.old)))
		}
	}
	return out
}

// classify labels a field, telling the two unlabeled ones apart: createdAt
// is a timestamp, anything else is the headline.
func classify(f string) (label, value string) {
	label, value = field(f)
	if label == "" {
		if _, ok := parseTime(value); ok {
			return "posted", formatValue(value)
		}
		return "title", value
	}
	return label, formatValue(value)
}

func join(vals []string) string { return strings.Join(vals, ", ") }

// ist is India Standard Time. A fixed zone avoids depending on the tz
// database, which Windows machines don't always ship to Go programs.
var ist = time.FixedZone("IST", 5*60*60+30*60)

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// formatValue shows API timestamps ("2026-11-01T18:30:00.000Z") as dates
// in IST ("2 Nov 2026"), which is how notices state them. Other values pass
// through unchanged.
func formatValue(v string) string {
	if t, ok := parseTime(v); ok {
		return t.In(ist).Format("2 Jan 2006")
	}
	return v
}

func firstN(s []string, n int) []string { return s[:min(len(s), n)] }

// truncate shortens s to at most n runes, so Hindi text is never cut
// mid-character.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
