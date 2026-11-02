package dashboard

import (
	"fmt"
	"html/template"
	"strings"
	"time"
)

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"ago":       s.ago,
		"until":     s.until,
		"stamp":     stamp,
		"diffLines": diffLines,
		"short":     short,
		"took":      func(d time.Duration) time.Duration { return d.Round(time.Millisecond) },
		"hash":      func(h string) string { return h[:min(len(h), 12)] },
	}
}

// ago renders a past time as "3m ago". Times are shown relative because
// the question on this page is always "is it working right now?".
func (s *Server) ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return human(s.now().Sub(t)) + " ago"
}

func (s *Server) until(t time.Time) string {
	if t.IsZero() {
		return "due now"
	}
	d := t.Sub(s.now())
	if d <= 0 {
		return "due now"
	}
	return "in " + human(d)
}

func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// stamp shows an absolute local time, for hover titles and detail pages.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2 Jan 2006 15:04:05")
}

// short truncates s to n characters (runes, so Hindi isn't cut mid-letter).
func short(n int, s string) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

type diffLine struct {
	Class string
	Text  string
}

// diffLines classifies each line of a stored diff (see package differ's
// rendering) so the template can colour it.
func diffLines(diff string) []diffLine {
	var out []diffLine
	for line := range strings.SplitSeq(diff, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		class := "ctx"
		switch {
		case strings.HasPrefix(line, "@@"):
			class = "hunk"
		case strings.HasPrefix(line, "~ "):
			class = "edit"
		case strings.HasPrefix(line, "..."):
			class = "note"
		case strings.HasPrefix(trimmed, "+ "):
			class = "add"
		case strings.HasPrefix(trimmed, "- "):
			class = "del"
		}
		out = append(out, diffLine{Class: class, Text: line})
	}
	return out
}
