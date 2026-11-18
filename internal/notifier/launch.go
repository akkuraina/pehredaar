package notifier

import (
	"context"
	"fmt"
	"html"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/akkuraina/pehredaar/internal/exams"
	"github.com/akkuraina/pehredaar/internal/store"
)

// Launch tells someone that exams they asked about in the survey are now
// watched. Every channel renders it its own way: email with a link to a
// confirm page, Telegram with a button.
type Launch struct {
	ExamIDs []string // catalog ids, e.g. "jee", "neet"
	Names   []string // display names, same order: "JEE", "NEET"
	Link    string   // web page that starts watching them (email)
	Lang    store.Language
}

// ListNames joins exam names for a sentence: "JEE", "JEE and NEET",
// "JEE, NEET and IBPS" (और in Hindi).
func ListNames(names []string, lang store.Language) string {
	and := " and "
	if lang == store.LangHindi {
		and = " और "
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + and + names[len(names)-1]
}

// cbLive starts watching every feed of the listed exams: "live:jee,neet".
// Telegram allows 64 bytes of callback data: room for every exam id.
const cbLive = "live:"

// Announce sends a launch message to a Telegram contact, with a button
// that subscribes them to the exams' feeds in one tap.
func (b *Bot) Announce(ctx context.Context, to store.Recipient, l Launch) error {
	t := textsFor(l.Lang)
	names := html.EscapeString(ListNames(l.Names, l.Lang))
	return b.sendParams(ctx, to.ChatID, &bot.SendMessageParams{
		ChatID:    to.ChatID,
		Text:      fmt.Sprintf(t.launch, names),
		ParseMode: models.ParseModeHTML,
		ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{{
			Text:         fmt.Sprintf(t.launchButton, ListNames(l.Names, l.Lang)),
			CallbackData: cbLive + strings.Join(l.ExamIDs, ","),
		}}}},
	})
}

// startExams handles the launch button: subscribe to every live feed of
// the listed exams. Tapping it twice is harmless (Subscribe is
// idempotent), and an exam that has no feeds any more is skipped.
func (c *commands) startExams(ctx context.Context, subID int64, ids string, lang store.Language) (reply, error) {
	t := textsFor(lang)
	catIDs := make([]string, len(c.cats))
	for i, cat := range c.cats {
		catIDs[i] = cat.ID
	}
	var names []string
	for _, id := range strings.Split(ids, ",") {
		e, ok := exams.ByID(id)
		if !ok {
			continue
		}
		cats := e.Categories(catIDs)
		for _, cat := range cats {
			if err := c.store.Subscribe(ctx, subID, store.Target{Category: cat}); err != nil {
				return reply{}, err
			}
		}
		if len(cats) > 0 {
			names = append(names, e.Name)
		}
	}
	if len(names) == 0 {
		r, err := c.watchView(ctx, subID, t)
		r.toast = t.unknownFeed
		return r, err
	}
	list := ListNames(names, lang)
	return reply{
		text:  fmt.Sprintf(t.launchDone, html.EscapeString(list)),
		toast: fmt.Sprintf(t.watching, list),
	}, nil
}
