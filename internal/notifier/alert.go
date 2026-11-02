// Package notifier is Pehredaar's Telegram side: it sends alerts and runs
// the bot commands (/start, /watch, /list, /stop, /language) that manage
// subscriptions. Users never see anything else of Pehredaar.
package notifier

import (
	"errors"
	"html"
	"strings"

	"github.com/akkuraina/pehredaar/internal/store"
)

// ErrBlocked means the user blocked the bot (or deleted their account):
// Telegram will refuse every message to that chat, so the caller should
// mark the subscriber inactive instead of retrying.
var ErrBlocked = errors.New("notifier: user blocked the bot")

// Alert is one message about one change.
type Alert struct {
	Title string // category label, e.g. "SSC Notice Board"
	Text  string // plain-text summary
	Links []Link
	Lang  store.Language // language of the fixed words ("Open PDF")
}

// Link is a URL shown under an alert.
type Link struct {
	URL string
	PDF bool // labelled "Open PDF" rather than "Open link"
}

// maxTextRunes keeps an alert well under Telegram's 4096-character message
// limit, leaving room for the title and links.
const maxTextRunes = 3500

// html renders the alert in Telegram's HTML parse mode. HTML is used rather
// than MarkdownV2 because only <, > and & need escaping, while MarkdownV2
// reserves characters that are common in notices ( . - ( ) ).
func (a Alert) html() string {
	var b strings.Builder
	b.WriteString("🔔 <b>" + html.EscapeString(a.Title) + "</b>\n")
	b.WriteString(html.EscapeString(truncate(a.Text, maxTextRunes)))
	t := textsFor(a.Lang)
	for _, l := range a.Links {
		icon, label := "🔗", t.openLink
		if l.PDF {
			icon, label = "📄", t.openPDF
		}
		b.WriteString("\n" + icon + " <a href=\"" + html.EscapeString(l.URL) + "\">" + label + "</a>")
	}
	return b.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
