package notifier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/time/rate"

	"github.com/akkuraina/pehredaar/internal/config"
)

// api is the part of *bot.Bot used for sending, so tests can fake it.
type api interface {
	SendMessage(ctx context.Context, p *bot.SendMessageParams) (*models.Message, error)
	EditMessageText(ctx context.Context, p *bot.EditMessageTextParams) (*models.Message, error)
	AnswerCallbackQuery(ctx context.Context, p *bot.AnswerCallbackQueryParams) (bool, error)
}

// Bot is the Telegram bot: it answers commands and sends alerts. Create it
// with New and call Run; Send is safe to call concurrently, from any
// goroutine, while Run is going.
type Bot struct {
	tg   *bot.Bot // nil in tests
	api  api
	cmds *commands
	log  *slog.Logger

	// Telegram's limits: about 30 messages per second overall, and about one
	// per second to any single chat. We stay a little under the first.
	global   *rate.Limiter
	chatRate rate.Limit
	mu       sync.Mutex
	perChat  map[int64]*rate.Limiter

	sleep func(ctx context.Context, d time.Duration) error // replaceable in tests
}

// New connects to Telegram (it checks the token with getMe, so a bad token
// fails here, at startup) and registers the command handlers.
func New(token string, st Store, cats []config.Category, log *slog.Logger) (*Bot, error) {
	if log == nil {
		log = slog.Default()
	}
	b := newBot(nil, st, cats, log)
	tg, err := bot.New(token,
		bot.WithDefaultHandler(b.onOther),
		bot.WithCallbackQueryDataHandler("", bot.MatchTypePrefix, b.onCallback),
		bot.WithErrorsHandler(func(err error) {
			// The library redacts the token from request errors.
			log.Warn("telegram", "err", err)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("notifier: connecting to Telegram: %w", err)
	}
	b.tg, b.api = tg, tg

	for name, fn := range map[string]func(context.Context, user, string) (reply, error){
		"start":    func(ctx context.Context, u user, _ string) (reply, error) { return b.cmds.start(ctx, u) },
		"help":     func(ctx context.Context, u user, _ string) (reply, error) { return b.cmds.help(ctx, u) },
		"language": func(ctx context.Context, u user, _ string) (reply, error) { return b.cmds.language(ctx, u) },
		"watch":    func(ctx context.Context, u user, _ string) (reply, error) { return b.cmds.watch(ctx, u) },
		"list":     func(ctx context.Context, u user, _ string) (reply, error) { return b.cmds.list(ctx, u) },
		"stop":     b.cmds.stop,
	} {
		tg.RegisterHandler(bot.HandlerTypeMessageText, name, bot.MatchTypeCommandStartOnly, b.onCommand(fn))
	}
	return b, nil
}

func newBot(a api, st Store, cats []config.Category, log *slog.Logger) *Bot {
	return &Bot{
		api:      a,
		cmds:     newCommands(st, cats),
		log:      log,
		global:   rate.NewLimiter(25, 5),
		chatRate: 1,
		perChat:  make(map[int64]*rate.Limiter),
		sleep:    sleepCtx,
	}
}

// Run publishes the command menu and long-polls Telegram for updates until
// ctx is cancelled. Long polling needs no public URL, so this works from a
// laptop.
func (b *Bot) Run(ctx context.Context) error {
	for _, lang := range []struct{ code, start, watch, list, stop, language string }{
		{"", "Start and help", "Choose feeds to watch", "What you're watching", "Stop watching a feed", "Change alert language"},
		{"hi", "शुरू करें और मदद", "फ़ीड चुनें", "आप किन पर नज़र रख रहे हैं", "किसी फ़ीड को हटाएं", "अलर्ट की भाषा बदलें"},
	} {
		_, err := b.tg.SetMyCommands(ctx, &bot.SetMyCommandsParams{
			LanguageCode: lang.code,
			Commands: []models.BotCommand{
				{Command: "watch", Description: lang.watch},
				{Command: "list", Description: lang.list},
				{Command: "stop", Description: lang.stop},
				{Command: "language", Description: lang.language},
				{Command: "start", Description: lang.start},
			},
		})
		if err != nil {
			// Only the "/" menu is affected; the commands still work.
			b.log.Warn("telegram: setting command menu", "lang", lang.code, "err", err)
		}
	}
	b.log.Info("telegram bot running")
	b.tg.Start(ctx) // returns when ctx is cancelled
	return nil
}

// onCommand adapts a command to a library handler.
func (b *Bot) onCommand(fn func(context.Context, user, string) (reply, error)) bot.HandlerFunc {
	return func(ctx context.Context, _ *bot.Bot, upd *models.Update) {
		m := upd.Message
		if m == nil || m.From == nil {
			return
		}
		u := user{telegramID: m.From.ID, chatID: m.Chat.ID}
		_, args, _ := strings.Cut(m.Text, " ")
		r, err := fn(ctx, u, args)
		b.respond(ctx, u, nil, r, err)
	}
}

// onOther answers any message that isn't a known command with the help
// text, so a user typing "hi" isn't met with silence.
func (b *Bot) onOther(ctx context.Context, _ *bot.Bot, upd *models.Update) {
	m := upd.Message
	if m == nil || m.From == nil || m.Chat.Type != models.ChatTypePrivate {
		return // in groups, stay quiet unless addressed with a command
	}
	u := user{telegramID: m.From.ID, chatID: m.Chat.ID}
	r, err := b.cmds.help(ctx, u)
	b.respond(ctx, u, nil, r, err)
}

func (b *Bot) onCallback(ctx context.Context, _ *bot.Bot, upd *models.Update) {
	q := upd.CallbackQuery
	if q == nil {
		return
	}
	// The message the button was on, if Telegram still lets us edit it.
	var msg *models.Message
	chatID := q.From.ID
	if q.Message.Message != nil {
		msg = q.Message.Message
		chatID = msg.Chat.ID
	}
	u := user{telegramID: q.From.ID, chatID: chatID}
	r, err := b.cmds.callback(ctx, u, q.Data)

	// Always answer, or the button shows a spinner for several seconds.
	if _, aerr := b.api.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: q.ID, Text: r.toast,
	}); aerr != nil {
		b.log.Warn("telegram: answering button", "err", aerr)
	}
	b.respond(ctx, u, msg, r, err)
}

// respond shows r: by editing msg in place when the reply comes from a
// button on it, otherwise as a new message.
func (b *Bot) respond(ctx context.Context, u user, msg *models.Message, r reply, err error) {
	if err != nil {
		b.log.Error("telegram: handling update", "user", u.telegramID, "err", err)
		r = reply{text: textsEN.somethingWrong}
		msg = nil
	}
	if r.text == "" {
		return
	}
	var markup models.ReplyMarkup
	if r.kb != nil {
		markup = r.kb
	}
	if msg != nil {
		_, err = b.api.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID: msg.Chat.ID, MessageID: msg.ID,
			Text: r.text, ParseMode: models.ParseModeHTML, ReplyMarkup: markup,
		})
		// Telegram rejects an edit that changes nothing ("message is not
		// modified"), e.g. tapping an already-ticked feed. That's fine.
		if err != nil && !strings.Contains(err.Error(), "message is not modified") {
			b.log.Warn("telegram: editing message", "user", u.telegramID, "err", err)
		}
		return
	}
	if _, err := b.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: u.chatID, Text: r.text, ParseMode: models.ParseModeHTML, ReplyMarkup: markup,
	}); err != nil {
		b.log.Warn("telegram: replying", "user", u.telegramID, "err", err)
	}
}

// maxSendAttempts bounds retries of one alert. Combined with mark-then-send
// delivery, an alert that still fails is dropped rather than risk a
// duplicate later.
const maxSendAttempts = 4

// maxRetryAfter caps how long one message waits on Telegram's flood control.
const maxRetryAfter = time.Minute

// Send delivers an alert to a chat, waiting for the rate limits and
// retrying on flood control and network errors. It returns ErrBlocked if
// the user has blocked the bot.
func (b *Bot) Send(ctx context.Context, chatID int64, a Alert) error {
	params := &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      a.html(),
		ParseMode: models.ParseModeHTML,
		// A preview of a PDF link is a grey box with a file name: noise.
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
	}
	var err error
	for attempt := 1; attempt <= maxSendAttempts; attempt++ {
		if err := b.wait(ctx, chatID); err != nil {
			return err
		}
		_, err = b.api.SendMessage(ctx, params)
		if err == nil {
			return nil
		}

		var flood *bot.TooManyRequestsError
		var delay time.Duration
		switch {
		case errors.Is(err, bot.ErrorForbidden):
			return fmt.Errorf("%w: %v", ErrBlocked, err)
		case errors.Is(err, bot.ErrorBadRequest), errors.Is(err, bot.ErrorUnauthorized):
			return err // retrying the same request can't succeed
		case errors.As(err, &flood):
			delay = min(time.Duration(flood.RetryAfter)*time.Second, maxRetryAfter)
		default: // network error or a Telegram 5xx
			delay = time.Duration(attempt) * 2 * time.Second
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt < maxSendAttempts {
			if err := b.sleep(ctx, delay); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("notifier: giving up after %d attempts: %w", maxSendAttempts, err)
}

// wait blocks until both the global and the chat's rate limit allow one
// more message.
func (b *Bot) wait(ctx context.Context, chatID int64) error {
	b.mu.Lock()
	lim, ok := b.perChat[chatID]
	if !ok {
		lim = rate.NewLimiter(b.chatRate, 1)
		b.perChat[chatID] = lim
	}
	b.mu.Unlock()
	if err := lim.Wait(ctx); err != nil {
		return err
	}
	return b.global.Wait(ctx)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
