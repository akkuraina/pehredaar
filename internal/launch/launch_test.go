package launch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akkuraina/pehredaar/internal/auth"
	"github.com/akkuraina/pehredaar/internal/notifier"
	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/store/sqlite"
)

// fakeSender records announcements; fail makes it return an error for
// the given contact ids.
type fakeSender struct {
	mu   sync.Mutex
	got  []string // "contact:EXAMS:lang"
	last notifier.Launch
	fail map[int64]error
}

func (f *fakeSender) Announce(_ context.Context, to store.Recipient, l notifier.Launch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[to.ID]; err != nil {
		return err
	}
	f.got = append(f.got, fmt.Sprintf("%d:%s:%s", to.ID, strings.Join(l.ExamIDs, ","), to.Language))
	f.last = l
	return nil
}

var signer = auth.NewSigner([]byte(strings.Repeat("s", 32)))

type fixture struct {
	st         *sqlite.Store
	tg, mail   *fakeSender
	announcer  *Announcer
	categories []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{st: st, tg: &fakeSender{}, mail: &fakeSender{}}
	f.announcer = New(Config{
		Store:      st,
		Categories: []string{"ssc:notice-board", "jee:nta-notices", "neet:nta-notices"},
		Senders:    map[store.Channel]Sender{store.ChannelTelegram: f.tg, store.ChannelEmail: f.mail},
		Signer:     signer,
		BaseURL:    "https://pehredaar.test",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		// The exams "go live" a second after the interests below were chosen.
		Now: func() time.Time { return time.Now().Add(time.Second) },
	})
	return f
}

func (f *fixture) telegramUser(t *testing.T, tgID int64, lang store.Language, interests ...string) (store.Subscriber, store.Contact) {
	t.Helper()
	sub, err := f.st.TelegramSubscriber(t.Context(), tgID, tgID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetLanguage(t.Context(), sub.ID, lang); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetInterests(t.Context(), sub.ID, interests); err != nil {
		t.Fatal(err)
	}
	c, err := f.st.ContactByAddress(t.Context(), store.ChannelTelegram, fmt.Sprint(tgID))
	if err != nil {
		t.Fatal(err)
	}
	return sub, c
}

func (f *fixture) addEmail(t *testing.T, subID int64, addr string, verified bool) store.Contact {
	t.Helper()
	c, err := f.st.AddEmail(t.Context(), subID, addr)
	if err != nil {
		t.Fatal(err)
	}
	if verified {
		if err := f.st.VerifyContact(t.Context(), c.ID); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestAnnounce(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	// Asha chose JEE, NEET and SSC on Telegram: one message, JEE and NEET
	// (SSC was always live), in Hindi.
	_, ashaTG := f.telegramUser(t, 101, store.LangHindi, "jee", "neet", "ssc")
	// Ravi has Telegram and a confirmed email: told on both.
	ravi, raviTG := f.telegramUser(t, 102, store.LangEnglish, "neet")
	raviMail := f.addEmail(t, ravi.ID, "ravi@example.test", true)
	// Meera's email was never confirmed: not told (yet).
	meera, _ := f.st.CreateSubscriber(ctx, store.LangEnglish)
	f.addEmail(t, meera.ID, "meera@example.test", false)
	if err := f.st.SetInterests(ctx, meera.ID, []string{"jee"}); err != nil {
		t.Fatal(err)
	}
	// Kabir already watches JEE: told only about NEET.
	kabir, kabirTG := f.telegramUser(t, 104, store.LangEnglish, "jee", "neet")
	if err := f.st.Subscribe(ctx, kabir.ID, store.Target{Category: "jee:nta-notices"}); err != nil {
		t.Fatal(err)
	}
	// Dev chose UPSC, which isn't live: nothing.
	f.telegramUser(t, 105, store.LangEnglish, "upsc")

	n, err := f.announcer.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantTG := []string{
		fmt.Sprintf("%d:jee,neet:hi", ashaTG.ID),
		fmt.Sprintf("%d:neet:en", raviTG.ID),
		fmt.Sprintf("%d:neet:en", kabirTG.ID),
	}
	wantMail := []string{fmt.Sprintf("%d:neet:en", raviMail.ID)}
	if n != 4 || !slices.Equal(f.tg.got, wantTG) || !slices.Equal(f.mail.got, wantMail) {
		t.Errorf("sent %d\ntelegram %v\nwant     %v\nemail %v\nwant  %v", n, f.tg.got, wantTG, f.mail.got, wantMail)
	}
	// The email link opens the confirm page for exactly Ravi's exams.
	if want := "https://pehredaar.test/start/neet/"; !strings.HasPrefix(f.mail.last.Link, want) {
		t.Errorf("link = %q", f.mail.last.Link)
	}
	signed := strings.TrimPrefix(f.mail.last.Link, "https://pehredaar.test/start/neet/")
	if id, err := signer.VerifyID(Purpose("neet"), signed); err != nil || id != ravi.ID {
		t.Errorf("link verifies to %d, %v; want %d", id, err, ravi.ID)
	}
	if _, err := signer.VerifyID(Purpose("jee"), signed); err == nil {
		t.Error("a NEET link verified for JEE")
	}

	// Nobody is told twice.
	if n, err := f.announcer.RunOnce(ctx); n != 0 || err != nil {
		t.Errorf("second run sent %d, %v", n, err)
	}

	// Meera confirms her email later: told on the next run.
	mc, _ := f.st.ContactByAddress(ctx, store.ChannelEmail, "meera@example.test")
	if err := f.st.VerifyContact(ctx, mc.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.announcer.RunOnce(ctx); n != 1 || f.mail.got[len(f.mail.got)-1] != fmt.Sprintf("%d:jee:en", mc.ID) {
		t.Errorf("after Meera confirmed: sent %d, email %v", n, f.mail.got)
	}
}

func TestUnreachableContactIsDeactivated(t *testing.T) {
	f := newFixture(t)
	_, c := f.telegramUser(t, 201, store.LangEnglish, "jee")
	f.tg.fail = map[int64]error{c.ID: notifier.ErrBlocked}

	if n, err := f.announcer.RunOnce(t.Context()); n != 0 || err != nil {
		t.Fatalf("sent %d, %v", n, err)
	}
	got, _ := f.st.Contact(t.Context(), c.ID)
	if got.Active {
		t.Error("blocked contact still active")
	}
	// Marked anyway: unblocking the bot doesn't bring the message back.
	f.tg.fail = nil
	if err := f.st.SetContactActive(t.Context(), c.ID, true); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.announcer.RunOnce(t.Context()); n != 0 {
		t.Errorf("re-sent after unblocking: %d", n)
	}
}

func TestNoChannelMeansWait(t *testing.T) {
	f := newFixture(t)
	f.announcer.cfg.Senders = map[store.Channel]Sender{store.ChannelTelegram: f.tg} // email not configured
	sub, _ := f.st.CreateSubscriber(t.Context(), store.LangEnglish)
	f.addEmail(t, sub.ID, "only@example.test", true)
	if err := f.st.SetInterests(t.Context(), sub.ID, []string{"jee"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.announcer.RunOnce(t.Context()); n != 0 {
		t.Fatalf("sent %d without an email sender", n)
	}
	// Email arrives on the server: now they're told.
	f.announcer.cfg.Senders[store.ChannelEmail] = f.mail
	if n, _ := f.announcer.RunOnce(t.Context()); n != 1 {
		t.Errorf("sent %d once email was configured, want 1", n)
	}
}
