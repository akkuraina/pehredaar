// Package notifier is Pehredaar's Telegram side: it sends alerts and runs
// the bot commands (/start, /watch, /list, /stop, /language) that manage
// subscriptions. Users never see anything else of Pehredaar.
package notifier

import (
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/akkuraina/pehredaar/internal/store"
)

// ErrUnreachable means a contact can never receive messages again as it
// is (blocked the bot, address bounced, unsubscribed), so the caller should
// deactivate the contact instead of retrying. Senders wrap it.
var ErrUnreachable = errors.New("notifier: contact unreachable")

// ErrBlocked means the user blocked the bot (or deleted their account).
// It wraps ErrUnreachable.
var ErrBlocked = fmt.Errorf("%w: user blocked the bot", ErrUnreachable)

// Alert is one message about one change. Every channel (Telegram, email)
// renders the same Alert in its own format.
type Alert struct {
	ChangeID int64  // for links back to the website
	Title    string // category label, e.g. "SSC Notice Board"
	Text     string // plain-text summary
	Links    []Link
	Lang     store.Language // language of the fixed words ("Open PDF")
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
