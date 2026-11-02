package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/notifier"
	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/store/sqlite"
	"github.com/akkuraina/pehredaar/internal/summarizer"
)

const feedURL = "https://ssc.gov.in/api/general-website/portal/notice-boards?page=1&language=english"

// A new notice with a PDF, as the differ renders it.
const addedDiff = "+ n1 | 2026-11-01T10:00:00.000Z | Revised dates for JE 2026 | att: uploads/masterData/NoticeBoards/je dates.pdf (1000 B)"

type sentAlert struct {
	chatID int64
	alert  notifier.Alert
}

// fakeNotifier records alerts; chats in blocked get ErrBlocked.
type fakeNotifier struct {
	mu      sync.Mutex
	sent    []sentAlert
	blocked map[int64]bool
}

func (f *fakeNotifier) Send(_ context.Context, chatID int64, a notifier.Alert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blocked[chatID] {
		return notifier.ErrBlocked
	}
	f.sent = append(f.sent, sentAlert{chatID, a})
	return nil
}

// failingSummarizer fails n times, then returns s.
type failingSummarizer struct {
	n     int
	calls int
	s     summarizer.Summary
}

func (f *failingSummarizer) Summarize(context.Context, summarizer.Input) (summarizer.Summary, error) {
	f.calls++
	if f.calls <= f.n {
		return summarizer.Summary{}, errors.New("llm down")
	}
	return f.s, nil
}

type env struct {
	t     *testing.T
	st    *sqlite.Store
	notif *fakeNotifier
	p     *Processor
}

func newEnv(t *testing.T, sum summarizer.Summarizer) *env {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n := &fakeNotifier{blocked: map[int64]bool{}}
	p := New(Config{
		Store:      st,
		Summarizer: sum,
		Notifier:   n,
		Categories: []config.Category{{ID: "ssc:notice-board", Label: "SSC Notice Board"}},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return &env{t: t, st: st, notif: n, p: p}
}

// source adds a source with a baseline snapshot.
func (e *env) source(url string, kind store.Kind, lang store.Language) store.Source {
	e.t.Helper()
	ctx := e.t.Context()
	src, err := e.st.UpsertSource(ctx, store.Source{
		URL: url, Kind: kind, Category: "ssc:notice-board", Language: lang,
		Label: "Notice Board " + string(lang), CheckInterval: time.Hour, Enabled: true,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := e.st.SaveSnapshot(ctx, store.Snapshot{SourceID: src.ID, ContentHash: "base", Text: "base"}, nil); err != nil {
		e.t.Fatal(err)
	}
	return src
}

// change records a pending change on src, as the scheduler would.
func (e *env) change(src store.Source, kind store.ChangeKind, diff string) store.Change {
	e.t.Helper()
	_, ch, err := e.st.SaveSnapshot(e.t.Context(),
		store.Snapshot{SourceID: src.ID, ContentHash: diff, Text: diff},
		&store.Change{Kind: kind, Diff: diff})
	if err != nil {
		e.t.Fatal(err)
	}
	return *ch
}

func (e *env) subscriber(tgID int64, lang store.Language) store.Subscriber {
	e.t.Helper()
	ctx := e.t.Context()
	s, err := e.st.UpsertSubscriber(ctx, tgID, tgID)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.SetLanguage(ctx, tgID, lang); err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.Subscribe(ctx, tgID, store.Target{Category: "ssc:notice-board"}); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) run() {
	e.t.Helper()
	if err := e.p.ProcessOnce(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) status(id int64) store.Change {
	e.t.Helper()
	c, err := e.st.Change(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func TestOneChangeOneAlertPerSubscriber(t *testing.T) {
	e := newEnv(t, nil)
	src := e.source(feedURL, store.KindFeed, store.LangEnglish)
	e.subscriber(1, store.LangEnglish)
	e.subscriber(2, store.LangHindi)
	ch := e.change(src, store.ChangeAdded, addedDiff)

	e.run()
	e.run() // a second round must not resend

	if len(e.notif.sent) != 2 {
		t.Fatalf("sent %d alerts, want 2: %+v", len(e.notif.sent), e.notif.sent)
	}
	byChat := map[int64]notifier.Alert{}
	for _, s := range e.notif.sent {
		byChat[s.chatID] = s.alert
	}
	if a := byChat[1]; a.Title != "SSC Notice Board" || a.Text != "New notice: Revised dates for JE 2026" || a.Lang != store.LangEnglish {
		t.Errorf("English alert = %+v", a)
	}
	if a := byChat[2]; a.Text != "नई सूचना: Revised dates for JE 2026" || a.Lang != store.LangHindi {
		t.Errorf("Hindi alert = %+v", a)
	}
	wantLink := notifier.Link{URL: "https://ssc.gov.in/api/attachment/uploads/masterData/NoticeBoards/je%20dates.pdf", PDF: true}
	if l := byChat[1].Links; len(l) != 1 || l[0] != wantLink {
		t.Errorf("links = %+v, want %+v", l, wantLink)
	}
	if c := e.status(ch.ID); c.Status != store.StatusNotified || c.Importance != store.ImportanceOther {
		t.Errorf("change = %s/%s, want notified/other", c.Status, c.Importance)
	}
	// Summaries are cached per language.
	if s, err := e.st.Summary(t.Context(), ch.ID, store.LangHindi); err != nil || !strings.HasPrefix(s, "नई सूचना") {
		t.Errorf("Hindi summary cache = %q, %v", s, err)
	}
}

func TestHindiSourceOnlyAlertsHindiSubscribers(t *testing.T) {
	e := newEnv(t, nil)
	src := e.source(strings.Replace(feedURL, "english", "hindi", 1), store.KindFeed, store.LangHindi)
	e.subscriber(1, store.LangEnglish)
	e.subscriber(2, store.LangHindi)
	e.change(src, store.ChangeAdded, addedDiff)

	e.run()
	if len(e.notif.sent) != 1 || e.notif.sent[0].chatID != 2 {
		t.Errorf("sent = %+v, want only chat 2", e.notif.sent)
	}
}

func TestNoSubscribersClosesWithoutSummarizing(t *testing.T) {
	sum := &failingSummarizer{}
	e := newEnv(t, sum)
	src := e.source(feedURL, store.KindFeed, store.LangEnglish)
	ch := e.change(src, store.ChangeAdded, addedDiff)

	e.run()
	if c := e.status(ch.ID); c.Status != store.StatusNotified || sum.calls != 0 {
		t.Errorf("status=%s summarizer calls=%d; want notified, 0", c.Status, sum.calls)
	}
	// Subscribing afterwards does not replay it.
	e.subscriber(1, store.LangEnglish)
	e.run()
	if len(e.notif.sent) != 0 {
		t.Errorf("old change sent to new subscriber: %+v", e.notif.sent)
	}
}

func TestBlockedSubscriberDeactivated(t *testing.T) {
	e := newEnv(t, nil)
	src := e.source(feedURL, store.KindFeed, store.LangEnglish)
	e.subscriber(1, store.LangEnglish)
	e.subscriber(2, store.LangEnglish)
	e.notif.blocked[1] = true
	ch := e.change(src, store.ChangeAdded, addedDiff)

	e.run()
	if len(e.notif.sent) != 1 || e.notif.sent[0].chatID != 2 {
		t.Errorf("sent = %+v, want only chat 2", e.notif.sent)
	}
	if c := e.status(ch.ID); c.Status != store.StatusNotified {
		t.Errorf("status = %s", c.Status)
	}
	subs, err := e.st.SubscribersFor(t.Context(), src)
	if err != nil || len(subs) != 1 || subs[0].TelegramID != 2 {
		t.Errorf("active subscribers = %+v, %v; want only 2", subs, err)
	}
}

func TestSummarizerFailureRetriesThenFallsBack(t *testing.T) {
	sum := &failingSummarizer{n: 100}
	e := newEnv(t, sum)
	src := e.source(feedURL, store.KindFeed, store.LangEnglish)
	e.subscriber(1, store.LangEnglish)
	ch := e.change(src, store.ChangeAdded, addedDiff)

	for i := 1; i < MaxAttempts; i++ {
		e.run()
		c := e.status(ch.ID)
		if c.Status != store.StatusPending || c.Attempts != i || len(e.notif.sent) != 0 {
			t.Fatalf("round %d: status=%s attempts=%d sent=%d", i, c.Status, c.Attempts, len(e.notif.sent))
		}
	}
	e.run() // attempt MaxAttempts fails too: the template fallback is used
	if len(e.notif.sent) != 1 || e.notif.sent[0].alert.Text != "New notice: Revised dates for JE 2026" {
		t.Errorf("fallback alert = %+v", e.notif.sent)
	}
	if c := e.status(ch.ID); c.Status != store.StatusNotified {
		t.Errorf("status = %s", c.Status)
	}
}

func TestCosmeticSuppressed(t *testing.T) {
	sum := &failingSummarizer{s: summarizer.Summary{Text: "Typo fixed", Importance: store.ImportanceCosmetic}}
	e := newEnv(t, sum)
	src := e.source(feedURL, store.KindFeed, store.LangEnglish)
	e.subscriber(1, store.LangEnglish)
	ch := e.change(src, store.ChangeEdited, "~ n1 | Notice\n    - Notcie\n    + Notice")

	e.run()
	if c := e.status(ch.ID); c.Status != store.StatusSuppressed || len(e.notif.sent) != 0 {
		t.Errorf("status=%s sent=%d; want suppressed, 0", c.Status, len(e.notif.sent))
	}
}

// A crash after some alerts went out: on restart the change is still
// "summarized", and only the subscribers not yet marked get it.
func TestResumeAfterCrashSendsEachAlertOnce(t *testing.T) {
	e := newEnv(t, nil)
	src := e.source(feedURL, store.KindFeed, store.LangEnglish)
	a := e.subscriber(1, store.LangEnglish)
	e.subscriber(2, store.LangEnglish)
	ch := e.change(src, store.ChangeAdded, addedDiff)

	// Simulate the state the crash left: summarized, subscriber 1 done.
	ctx := t.Context()
	ch.Status, ch.Importance = store.StatusSummarized, store.ImportanceOther
	if err := e.st.UpdateChange(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SaveSummary(ctx, ch.ID, store.LangEnglish, "cached summary"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.MarkDelivered(ctx, ch.ID, a.ID); err != nil {
		t.Fatal(err)
	}

	e.run()
	if len(e.notif.sent) != 1 || e.notif.sent[0].chatID != 2 || e.notif.sent[0].alert.Text != "cached summary" {
		t.Errorf("sent = %+v, want only chat 2 with the cached summary", e.notif.sent)
	}
}

func TestStaleChangeClosedWithoutAlert(t *testing.T) {
	e := newEnv(t, nil)
	src := e.source(feedURL, store.KindFeed, store.LangEnglish)
	e.subscriber(1, store.LangEnglish)
	ch := e.change(src, store.ChangeAdded, addedDiff)
	e.p.now = func() time.Time { return time.Now().Add(MaxAge + time.Hour) }

	e.run()
	if c := e.status(ch.ID); c.Status != store.StatusNotified || len(e.notif.sent) != 0 {
		t.Errorf("status=%s sent=%d; want notified, 0", c.Status, len(e.notif.sent))
	}
}

func TestPDFChangeLinksTheDocument(t *testing.T) {
	e := newEnv(t, nil)
	src := e.source("https://ssc.gov.in/api/attachment/uploads/x.pdf", store.KindPDF, store.LangEnglish)
	e.subscriber(1, store.LangEnglish)
	e.change(src, store.ChangeEdited, "@@ -1 +1 @@\n- Last date 12 Nov\n+ Last date 20 Nov")

	e.run()
	if len(e.notif.sent) != 1 {
		t.Fatalf("sent = %+v", e.notif.sent)
	}
	a := e.notif.sent[0].alert
	if len(a.Links) != 1 || a.Links[0] != (notifier.Link{URL: src.URL, PDF: true}) {
		t.Errorf("links = %+v", a.Links)
	}
	if !strings.Contains(a.Text, "Was: Last date 12 Nov") || !strings.Contains(a.Text, "Now: Last date 20 Nov") {
		t.Errorf("text = %q", a.Text)
	}
}

func TestAlertLinksSkipBadRedirects(t *testing.T) {
	src := store.Source{URL: feedURL, Kind: store.KindFeed}
	diff := "+ a | 2026-11-01 | A | link: javascript:alert(1)\n" +
		"+ b | 2026-11-01 | B | link: https://ssc.gov.in/x\n" +
		"- c | 2026-10-01 | C | att: uploads/old.pdf (1 B)"
	got := alertLinks(src, diff)
	if len(got) != 1 || got[0] != (notifier.Link{URL: "https://ssc.gov.in/x"}) {
		t.Errorf("links = %+v", got)
	}
}
