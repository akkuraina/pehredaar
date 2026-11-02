package notifier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/time/rate"

	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/store/sqlite"
)

var testCats = []config.Category{
	{ID: "ssc:notice-board", Label: "SSC Notice Board"},
	{ID: "ssc:results", Label: "SSC Results"},
}

var alice = user{telegramID: 111, chatID: 111}

func newTestCommands(t *testing.T) (*commands, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newCommands(st, testCats), st
}

// ok checks a command's error: ok(t)(c.watch(ctx, u)). It's curried
// because Go only spreads a multi-value call when it is the sole argument.
func ok(t *testing.T) func(reply, error) reply {
	return func(r reply, err error) reply {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
}

// buttons flattens a keyboard to "text=data" strings.
func buttons(kb *models.InlineKeyboardMarkup) []string {
	if kb == nil {
		return nil
	}
	var out []string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			out = append(out, b.Text+"="+b.CallbackData)
		}
	}
	return out
}

func TestStartRegistersAndOffersLanguages(t *testing.T) {
	c, st := newTestCommands(t)
	r := ok(t)(c.start(t.Context(), alice))
	if !strings.Contains(r.text, "/watch") || !strings.Contains(r.text, chooseLanguage) {
		t.Errorf("start text:\n%s", r.text)
	}
	if got := strings.Join(buttons(r.kb), ","); got != "English=lang:en,हिन्दी=lang:hi" {
		t.Errorf("buttons = %s", got)
	}
	if _, err := st.Subscriptions(t.Context(), alice.telegramID); err != nil {
		t.Errorf("subscriber not usable after /start: %v", err)
	}
}

func TestChooseHindiSwitchesBotLanguage(t *testing.T) {
	c, _ := newTestCommands(t)
	ok(t)(c.start(t.Context(), alice))
	r := ok(t)(c.callback(t.Context(), alice, "lang:hi"))
	if r.text != textsHI.languageSet {
		t.Errorf("text = %q", r.text)
	}
	if r = ok(t)(c.watch(t.Context(), alice)); r.text != textsHI.watchPrompt {
		t.Errorf("watch prompt after choosing Hindi = %q", r.text)
	}
	if _, err := c.callback(t.Context(), alice, "lang:fr"); err == nil {
		t.Error("unknown language accepted")
	}
}

func TestWatchListStop(t *testing.T) {
	c, st := newTestCommands(t)
	ctx := t.Context()

	r := ok(t)(c.watch(ctx, alice))
	want := "SSC Notice Board=sub:ssc:notice-board,SSC Results=sub:ssc:results"
	if got := strings.Join(buttons(r.kb), ","); got != want {
		t.Errorf("watch buttons = %s, want %s", got, want)
	}
	if r = ok(t)(c.list(ctx, alice)); r.text != textsEN.noneWatched {
		t.Errorf("empty list = %q", r.text)
	}

	// Subscribing ticks the button in place and toasts.
	r = ok(t)(c.callback(ctx, alice, "sub:ssc:results"))
	if r.toast != "Watching SSC Results" || !strings.Contains(strings.Join(buttons(r.kb), ","), "✅ SSC Results=") {
		t.Errorf("after subscribe: toast=%q buttons=%v", r.toast, buttons(r.kb))
	}
	// Tapping again doesn't duplicate.
	r = ok(t)(c.callback(ctx, alice, "sub:ssc:results"))
	if !strings.HasPrefix(r.toast, "Already watching") {
		t.Errorf("second tap toast = %q", r.toast)
	}
	ok(t)(c.callback(ctx, alice, "sub:ssc:notice-board"))

	subs, err := st.Subscriptions(ctx, alice.telegramID)
	if err != nil || len(subs) != 2 {
		t.Fatalf("subscriptions = %v, %v", subs, err)
	}

	r = ok(t)(c.list(ctx, alice))
	if r.text != "You're watching:\n• SSC Results\n• SSC Notice Board" {
		t.Errorf("list text = %q", r.text)
	}
	// /list has a stop button per feed; /stop adds "stop all" when there are several.
	if n := len(buttons(r.kb)); n != 2 {
		t.Errorf("list has %d buttons, want 2", n)
	}
	r = ok(t)(c.stop(ctx, alice, ""))
	if got := buttons(r.kb); len(got) != 3 || got[2] != "✖ Stop all=unsub:*" {
		t.Errorf("stop buttons = %v", got)
	}

	r = ok(t)(c.callback(ctx, alice, "unsub:ssc:results"))
	if r.toast != "Stopped watching SSC Results" || len(buttons(r.kb)) != 1 {
		t.Errorf("after unsubscribe: toast=%q buttons=%v", r.toast, buttons(r.kb))
	}
	r = ok(t)(c.stop(ctx, alice, "all"))
	if r.text != textsEN.stoppedAll {
		t.Errorf("/stop all = %q", r.text)
	}
	if subs, _ := st.Subscriptions(ctx, alice.telegramID); len(subs) != 0 {
		t.Errorf("subscriptions left after /stop all: %v", subs)
	}
}

func TestStaleCategoryButton(t *testing.T) {
	c, st := newTestCommands(t)
	r := ok(t)(c.callback(t.Context(), alice, "sub:ssc:gone"))
	if r.toast != textsEN.unknownFeed {
		t.Errorf("toast = %q", r.toast)
	}
	if subs, _ := st.Subscriptions(t.Context(), alice.telegramID); len(subs) != 0 {
		t.Errorf("subscribed to a removed category: %v", subs)
	}
}

func TestLabelsAreEscaped(t *testing.T) {
	c, _ := newTestCommands(t)
	c = newCommands(c.store, []config.Category{{ID: "x", Label: "Results <b>&</b> more"}})
	ok(t)(c.callback(t.Context(), alice, "sub:x"))
	r := ok(t)(c.list(t.Context(), alice))
	if !strings.Contains(r.text, "Results &lt;b&gt;&amp;&lt;/b&gt; more") {
		t.Errorf("label not escaped: %q", r.text)
	}
}

func TestAlertHTML(t *testing.T) {
	a := Alert{
		Title: "SSC Notice Board",
		Text:  "New notice: Age limit < 27 & fee revised",
		Links: []Link{{URL: "https://ssc.gov.in/api/attachment/a%20b.pdf", PDF: true}},
	}
	want := "🔔 <b>SSC Notice Board</b>\nNew notice: Age limit &lt; 27 &amp; fee revised\n" +
		`📄 <a href="https://ssc.gov.in/api/attachment/a%20b.pdf">Open PDF</a>`
	if got := a.html(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	a.Lang = store.LangHindi
	if !strings.Contains(a.html(), ">PDF खोलें</a>") {
		t.Errorf("Hindi link label missing: %s", a.html())
	}
}

// fakeAPI replays a list of errors for SendMessage, then succeeds.
type fakeAPI struct {
	errs  []error
	sent  []*bot.SendMessageParams
	calls int
}

func (f *fakeAPI) SendMessage(_ context.Context, p *bot.SendMessageParams) (*models.Message, error) {
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}
	f.sent = append(f.sent, p)
	return &models.Message{}, nil
}

func (f *fakeAPI) EditMessageText(context.Context, *bot.EditMessageTextParams) (*models.Message, error) {
	return &models.Message{}, nil
}

func (f *fakeAPI) AnswerCallbackQuery(context.Context, *bot.AnswerCallbackQueryParams) (bool, error) {
	return true, nil
}

func newTestBot(a api) (*Bot, *[]time.Duration) {
	b := newBot(a, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.chatRate = rate.Inf // the real 1/s limit would make retry tests slow
	var slept []time.Duration
	b.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return b, &slept
}

func TestSendRetriesFloodControl(t *testing.T) {
	f := &fakeAPI{errs: []error{&bot.TooManyRequestsError{Message: "slow down", RetryAfter: 7}}}
	b, slept := newTestBot(f)
	if err := b.Send(t.Context(), 42, Alert{Title: "T", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || len(*slept) != 1 || (*slept)[0] != 7*time.Second {
		t.Errorf("sent=%d slept=%v", len(f.sent), *slept)
	}
	if p := f.sent[0]; p.ChatID != int64(42) || p.ParseMode != models.ParseModeHTML ||
		p.LinkPreviewOptions == nil || !*p.LinkPreviewOptions.IsDisabled {
		t.Errorf("params = %+v", p)
	}
}

func TestSendBlocked(t *testing.T) {
	f := &fakeAPI{errs: []error{fmt.Errorf("%w, bot was blocked by the user", bot.ErrorForbidden)}}
	b, _ := newTestBot(f)
	err := b.Send(t.Context(), 42, Alert{})
	if !errors.Is(err, ErrBlocked) || f.calls != 1 {
		t.Errorf("err=%v calls=%d; want ErrBlocked after 1 call", err, f.calls)
	}
}

func TestSendGivesUp(t *testing.T) {
	boom := errors.New("connection reset")
	f := &fakeAPI{errs: []error{boom, boom, boom, boom, boom}}
	b, slept := newTestBot(f)
	err := b.Send(t.Context(), 42, Alert{})
	if !errors.Is(err, boom) || f.calls != maxSendAttempts || len(*slept) != maxSendAttempts-1 {
		t.Errorf("err=%v calls=%d slept=%v", err, f.calls, *slept)
	}
}

func TestSendBadRequestNotRetried(t *testing.T) {
	f := &fakeAPI{errs: []error{fmt.Errorf("%w, can't parse entities", bot.ErrorBadRequest)}}
	b, _ := newTestBot(f)
	if err := b.Send(t.Context(), 42, Alert{}); err == nil || f.calls != 1 {
		t.Errorf("err=%v calls=%d", err, f.calls)
	}
}
