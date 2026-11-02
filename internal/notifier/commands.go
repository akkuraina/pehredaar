package notifier

import (
	"context"
	"fmt"
	"html"
	"strings"

	"github.com/go-telegram/bot/models"

	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/store"
)

// Store is the part of store.Store the bot uses. Declaring it here (the
// consumer side) keeps the bot's dependencies visible; *sqlite.Store
// satisfies it without saying so, because Go interfaces are implicit.
type Store interface {
	UpsertSubscriber(ctx context.Context, telegramID, chatID int64) (store.Subscriber, error)
	SetLanguage(ctx context.Context, telegramID int64, lang store.Language) error
	Subscribe(ctx context.Context, telegramID int64, t store.Target) error
	Unsubscribe(ctx context.Context, telegramID int64, t store.Target) (bool, error)
	Subscriptions(ctx context.Context, telegramID int64) ([]store.Subscription, error)
}

// commands is the bot's logic, separate from the Telegram transport: each
// method takes who is asking and returns what to show, so it can be tested
// without a network.
type commands struct {
	store  Store
	cats   []config.Category
	labels map[string]string // category ID → label
}

func newCommands(st Store, cats []config.Category) *commands {
	labels := make(map[string]string, len(cats))
	for _, c := range cats {
		labels[c.ID] = c.Label
	}
	return &commands{store: st, cats: cats, labels: labels}
}

// user identifies who sent an update. In a private chat the two IDs are
// equal; they differ if the bot is added to a group.
type user struct {
	telegramID int64
	chatID     int64
}

// reply is what the bot shows in response to a command or button tap.
type reply struct {
	text  string // HTML
	kb    *models.InlineKeyboardMarkup
	toast string // button taps only: the small popup Telegram shows
}

// Callback data prefixes. Telegram limits callback data to 64 bytes, which
// "unsub:" plus a category ID fits easily.
const (
	cbLang   = "lang:"
	cbSub    = "sub:"
	cbUnsub  = "unsub:"
	unsubAll = "*"
)

// register makes sure the user exists (and is active again, if they had
// blocked the bot and came back) and returns their settings. Every command
// starts with it, so no command can fail with "unknown subscriber".
func (c *commands) register(ctx context.Context, u user) (store.Subscriber, error) {
	return c.store.UpsertSubscriber(ctx, u.telegramID, u.chatID)
}

func (c *commands) start(ctx context.Context, u user) (reply, error) {
	sub, err := c.register(ctx, u)
	if err != nil {
		return reply{}, err
	}
	return reply{
		text: textsFor(sub.Language).help + "\n\n" + chooseLanguage,
		kb:   languageKeyboard(),
	}, nil
}

func (c *commands) language(ctx context.Context, u user) (reply, error) {
	if _, err := c.register(ctx, u); err != nil {
		return reply{}, err
	}
	return reply{text: chooseLanguage, kb: languageKeyboard()}, nil
}

func (c *commands) help(ctx context.Context, u user) (reply, error) {
	sub, err := c.register(ctx, u)
	if err != nil {
		return reply{}, err
	}
	return reply{text: textsFor(sub.Language).help}, nil
}

func (c *commands) watch(ctx context.Context, u user) (reply, error) {
	sub, err := c.register(ctx, u)
	if err != nil {
		return reply{}, err
	}
	return c.watchView(ctx, u, textsFor(sub.Language))
}

// watchView lists every category; the ones already watched are ticked.
func (c *commands) watchView(ctx context.Context, u user, t texts) (reply, error) {
	watched, err := c.watched(ctx, u)
	if err != nil {
		return reply{}, err
	}
	kb := &models.InlineKeyboardMarkup{}
	for _, cat := range c.cats {
		label := cat.Label
		if watched[cat.ID] {
			label = "✅ " + label
		}
		kb.InlineKeyboard = append(kb.InlineKeyboard, []models.InlineKeyboardButton{
			{Text: label, CallbackData: cbSub + cat.ID},
		})
	}
	return reply{text: t.watchPrompt, kb: kb}, nil
}

func (c *commands) list(ctx context.Context, u user) (reply, error) {
	sub, err := c.register(ctx, u)
	if err != nil {
		return reply{}, err
	}
	t := textsFor(sub.Language)
	ids, err := c.watchedList(ctx, u)
	if err != nil {
		return reply{}, err
	}
	if len(ids) == 0 {
		return reply{text: t.noneWatched}, nil
	}
	var b strings.Builder
	b.WriteString(t.listHeader)
	for _, id := range ids {
		b.WriteString("\n• " + html.EscapeString(c.label(id)))
	}
	return reply{text: b.String(), kb: c.stopKeyboard(ids, t, false)}, nil
}

// stop handles "/stop" (show what can be removed) and "/stop all".
func (c *commands) stop(ctx context.Context, u user, args string) (reply, error) {
	sub, err := c.register(ctx, u)
	if err != nil {
		return reply{}, err
	}
	t := textsFor(sub.Language)
	if strings.EqualFold(strings.TrimSpace(args), "all") {
		if err := c.unsubscribeAll(ctx, u); err != nil {
			return reply{}, err
		}
		return reply{text: t.stoppedAll}, nil
	}
	return c.stopView(ctx, u, t)
}

func (c *commands) stopView(ctx context.Context, u user, t texts) (reply, error) {
	ids, err := c.watchedList(ctx, u)
	if err != nil {
		return reply{}, err
	}
	if len(ids) == 0 {
		return reply{text: t.noneWatched}, nil
	}
	return reply{text: t.stopPrompt, kb: c.stopKeyboard(ids, t, len(ids) > 1)}, nil
}

// callback handles a button tap. The reply replaces the message the button
// was on, so keyboards update in place instead of piling up.
func (c *commands) callback(ctx context.Context, u user, data string) (reply, error) {
	sub, err := c.register(ctx, u)
	if err != nil {
		return reply{}, err
	}
	t := textsFor(sub.Language)

	switch {
	case strings.HasPrefix(data, cbLang):
		lang := store.Language(strings.TrimPrefix(data, cbLang))
		if lang != store.LangEnglish && lang != store.LangHindi {
			return reply{}, fmt.Errorf("unknown language %q", lang)
		}
		if err := c.store.SetLanguage(ctx, u.telegramID, lang); err != nil {
			return reply{}, err
		}
		return reply{text: textsFor(lang).languageSet}, nil

	case strings.HasPrefix(data, cbSub):
		id := strings.TrimPrefix(data, cbSub)
		if _, ok := c.labels[id]; !ok {
			// A button from an old message for a category since removed
			// from sources.yaml.
			r, err := c.watchView(ctx, u, t)
			r.toast = t.unknownFeed
			return r, err
		}
		watched, err := c.watched(ctx, u)
		if err != nil {
			return reply{}, err
		}
		toast := fmt.Sprintf(t.alreadyWatched, c.label(id))
		if !watched[id] {
			if err := c.store.Subscribe(ctx, u.telegramID, store.Target{Category: id}); err != nil {
				return reply{}, err
			}
			toast = fmt.Sprintf(t.watching, c.label(id))
		}
		r, err := c.watchView(ctx, u, t)
		r.toast = toast
		return r, err

	case data == cbUnsub+unsubAll:
		if err := c.unsubscribeAll(ctx, u); err != nil {
			return reply{}, err
		}
		return reply{text: t.stoppedAll}, nil

	case strings.HasPrefix(data, cbUnsub):
		id := strings.TrimPrefix(data, cbUnsub)
		if _, err := c.store.Unsubscribe(ctx, u.telegramID, store.Target{Category: id}); err != nil {
			return reply{}, err
		}
		r, err := c.stopView(ctx, u, t)
		r.toast = fmt.Sprintf(t.stopped, c.label(id))
		return r, err
	}
	return reply{}, fmt.Errorf("unknown callback data %q", data)
}

func (c *commands) unsubscribeAll(ctx context.Context, u user) error {
	subs, err := c.store.Subscriptions(ctx, u.telegramID)
	if err != nil {
		return err
	}
	for _, s := range subs {
		if _, err := c.store.Unsubscribe(ctx, u.telegramID, s.Target); err != nil {
			return err
		}
	}
	return nil
}

// watchedList returns the user's category subscriptions in subscription
// order. v1 only creates category subscriptions; others are ignored here.
func (c *commands) watchedList(ctx context.Context, u user) ([]string, error) {
	subs, err := c.store.Subscriptions(ctx, u.telegramID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, s := range subs {
		if s.Target.Category != "" {
			ids = append(ids, s.Target.Category)
		}
	}
	return ids, nil
}

func (c *commands) watched(ctx context.Context, u user) (map[string]bool, error) {
	ids, err := c.watchedList(ctx, u)
	if err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m, nil
}

// label names a category. A subscription can outlive its category in
// sources.yaml; then the raw ID is the best name we have.
func (c *commands) label(id string) string {
	if l, ok := c.labels[id]; ok {
		return l
	}
	return id
}

func (c *commands) stopKeyboard(ids []string, t texts, withAll bool) *models.InlineKeyboardMarkup {
	kb := &models.InlineKeyboardMarkup{}
	for _, id := range ids {
		kb.InlineKeyboard = append(kb.InlineKeyboard, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf(t.stopButton, c.label(id)), CallbackData: cbUnsub + id},
		})
	}
	if withAll {
		kb.InlineKeyboard = append(kb.InlineKeyboard, []models.InlineKeyboardButton{
			{Text: t.stopAllButton, CallbackData: cbUnsub + unsubAll},
		})
	}
	return kb
}

func languageKeyboard() *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		{Text: "English", CallbackData: cbLang + string(store.LangEnglish)},
		{Text: "हिन्दी", CallbackData: cbLang + string(store.LangHindi)},
	}}}
}
