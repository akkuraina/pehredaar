package web

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akkuraina/pehredaar/internal/auth"
	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/email"
	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/store/sqlite"
)

// fakeMailer records emails. Sending happens in a goroutine, so tests
// read from sent with a timeout.
type fakeMailer struct {
	sent chan mailed
}

type mailed struct{ kind, to, link string }

func (m *fakeMailer) SendConfirm(_ context.Context, to string, _ store.Language, link string) error {
	m.sent <- mailed{"confirm", to, link}
	return nil
}

func (m *fakeMailer) SendLogin(_ context.Context, to string, _ store.Language, link string) error {
	m.sent <- mailed{"login", to, link}
	return nil
}

func (m *fakeMailer) next(t *testing.T) mailed {
	t.Helper()
	select {
	case x := <-m.sent:
		return x
	case <-time.After(3 * time.Second):
		t.Fatal("no email sent")
		return mailed{}
	}
}

func (m *fakeMailer) none(t *testing.T) {
	t.Helper()
	select {
	case x := <-m.sent:
		t.Fatalf("unexpected email: %+v", x)
	case <-time.After(150 * time.Millisecond):
	}
}

type env struct {
	t      *testing.T
	st     *sqlite.Store
	srv    *Server
	ts     *httptest.Server
	mail   *fakeMailer
	signer *auth.Signer
	feed   store.Source
}

var cats = []config.Category{
	{ID: "ssc:notice-board", Label: "SSC Notice Board"},
	{ID: "ssc:results", Label: "SSC Results"},
	{ID: "jee:nta-notices", Label: "JEE Main notices (NTA)"},
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	feed, err := st.UpsertSource(t.Context(), store.Source{
		URL: "https://ssc.example.test/feed", Kind: store.KindFeed, Category: "ssc:notice-board",
		Language: store.LangEnglish, Label: "SSC Notice Board", CheckInterval: time.Hour, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, mail: &fakeMailer{sent: make(chan mailed, 10)},
		signer: auth.NewSigner([]byte(strings.Repeat("s", 32))), feed: feed}
	e.srv, err = New(Config{
		Store: st, Signer: e.signer, Mailer: e.mail, Categories: cats,
		BotUsername: func() string { return "PehredaarBot" },
		BaseURL:     "http://pehredaar.test",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.ts = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

// client is a browser: it keeps cookies and doesn't follow redirects, so
// tests can see them.
func (e *env) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *env) get(c *http.Client, path string) (*http.Response, string) {
	e.t.Helper()
	resp, err := c.Get(e.ts.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (e *env) post(c *http.Client, path string, form url.Values) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin") // what a browser sends for our own forms
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// change saves a summarized change on the feed.
func (e *env) change(summary string) store.Change {
	e.t.Helper()
	ctx := e.t.Context()
	_, ch, err := e.st.SaveSnapshot(ctx, store.Snapshot{SourceID: e.feed.ID, ContentHash: summary, Text: summary},
		&store.Change{Kind: store.ChangeEdited, Diff: "~ n1 | CGL 2026\n    - end: 2026-11-12\n    + end: 2026-11-20"})
	if err != nil {
		e.t.Fatal(err)
	}
	ch.Status, ch.Importance = store.StatusNotified, store.ImportanceDate
	if err := e.st.UpdateChange(ctx, *ch); err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.SaveSummary(ctx, ch.ID, store.LangEnglish, summary); err != nil {
		e.t.Fatal(err)
	}
	return *ch
}

func pathOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Path
}

func joinForm(via, addr string, exams ...string) url.Values {
	f := url.Values{"via": {via}, "email": {addr}, "lang": {"en"}}
	for _, x := range exams {
		f.Add("exam", x)
	}
	return f
}

func TestLandingShowsRealCatches(t *testing.T) {
	e := newEnv(t)
	e.change("CGL 2026 last date moved from 12 Nov to 20 Nov.\n• End date: 12 Nov 2026 → 20 Nov 2026")
	resp, body := e.get(e.client(), "/")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{"I caught it.", "CGL 2026 last date moved", `class="morph-old">12 Nov 2026`, "data-events=\"/events?all=1\"", "Soon", "Live"} {
		if !strings.Contains(body, want) {
			t.Errorf("landing is missing %q", want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'self' https://akanksharaina.vercel.app;") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestEmailSignUpConfirmAndFeed(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	e.change("New notice: CGL 2026 dates")

	resp, _ := e.post(c, "/join", joinForm("email", "asha@example.test", "ssc", "upsc"))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/feed?m=check-email" {
		t.Fatalf("join: %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	m := e.mail.next(t)
	if m.kind != "confirm" || m.to != "asha@example.test" || !strings.HasPrefix(m.link, "http://pehredaar.test/confirm/") {
		t.Fatalf("mail = %+v", m)
	}
	contact, err := e.st.ContactByAddress(t.Context(), store.ChannelEmail, "asha@example.test")
	if err != nil || contact.Verified() {
		t.Fatalf("contact = %+v, %v; want unverified", contact, err)
	}
	// No feed was ticked, so a live exam means all its feeds.
	subs, _ := e.st.Subscriptions(t.Context(), contact.SubscriberID)
	if len(subs) != 2 {
		t.Errorf("subscriptions = %+v, want both SSC feeds", subs)
	}
	if got, _ := e.st.Interests(t.Context(), contact.SubscriberID); len(got) != 2 {
		t.Errorf("interests = %v", got)
	}

	// Signed in already (the visitor created the record), feed shows the change.
	_, body := e.get(c, "/feed?m=check-email")
	for _, want := range []string{"CGL 2026 dates", "Coming soon", "UPSC", "confirm via email", "open the email I just sent"} {
		if !strings.Contains(body, want) {
			t.Errorf("feed is missing %q", want)
		}
	}

	// A different browser clicks the emailed link: verified and signed in.
	other := e.client()
	resp, _ = e.get(other, pathOf(t, m.link))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/feed?m=confirmed" {
		t.Fatalf("confirm: %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if c, _ := e.st.Contact(t.Context(), contact.ID); !c.Verified() {
		t.Error("contact not verified after confirming")
	}
	if resp, _ := e.get(other, "/feed"); resp.StatusCode != 200 {
		t.Errorf("feed after confirm: %d", resp.StatusCode)
	}
	// The link works once.
	if resp, _ := e.get(e.client(), pathOf(t, m.link)); resp.StatusCode != http.StatusGone {
		t.Errorf("reused confirm link: %d, want 410", resp.StatusCode)
	}
}

func TestSignUpWithKnownEmailSendsSignInAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.st.CreateSubscriber(t.Context(), store.LangEnglish)
	c, _ := e.st.AddEmail(t.Context(), owner.ID, "taken@example.test")
	e.st.VerifyContact(t.Context(), c.ID)
	e.st.SetInterests(t.Context(), owner.ID, []string{"ssc"})

	resp, body := e.post(e.client(), "/join", joinForm("email", "Taken@Example.test", "cat"))
	if resp.StatusCode != 200 || !strings.Contains(body, "Check your inbox") {
		t.Fatalf("join with a known address: %d", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Error("a session was given for someone else's address")
	}
	if m := e.mail.next(t); m.kind != "login" || m.to != "taken@example.test" {
		t.Errorf("mail = %+v, want a sign-in link to the owner", m)
	}
	if got, _ := e.st.Interests(t.Context(), owner.ID); len(got) != 1 || got[0] != "ssc" {
		t.Errorf("owner's interests changed to %v", got)
	}
}

func TestJoinValidationAndHoneypot(t *testing.T) {
	e := newEnv(t)
	resp, body := e.post(e.client(), "/join", joinForm("email", "asha@example.test"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Pick at least one exam") {
		t.Errorf("no exam: %d", resp.StatusCode)
	}
	resp, body = e.post(e.client(), "/join", joinForm("email", "not an email", "ssc"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "doesn&#39;t look right") {
		t.Errorf("bad email: %d", resp.StatusCode)
	}
	f := joinForm("email", "bot@example.test", "ssc")
	f.Set("website", "http://spam.example")
	resp, _ = e.post(e.client(), "/join", f)
	if resp.StatusCode != 200 {
		t.Errorf("honeypot: %d", resp.StatusCode)
	}
	e.mail.none(t)
	if _, err := e.st.ContactByAddress(t.Context(), store.ChannelEmail, "bot@example.test"); err == nil {
		t.Error("honeypot submission was stored")
	}
}

func TestCrossSitePostRejected(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/join", strings.NewReader(joinForm("email", "a@b.test", "ssc").Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST: %d, want 403", resp.StatusCode)
	}
}

func TestTelegramSignUpGivesDeepLink(t *testing.T) {
	e := newEnv(t)
	resp, body := e.post(e.client(), "/join", joinForm("telegram", "", "ssc"))
	if resp.StatusCode != 200 || !strings.Contains(body, "https://t.me/PehredaarBot?start=") {
		t.Fatalf("telegram join: %d", resp.StatusCode)
	}
	i := strings.Index(body, "?start=")
	token := body[i+7 : i+7+43]
	tok, err := e.st.ConsumeToken(t.Context(), auth.HashToken(token), store.TokenLinkTelegram, time.Now())
	if err != nil || tok.SubscriberID == 0 {
		t.Errorf("link token = %+v, %v", tok, err)
	}
	e.mail.none(t)
}

func TestLoginLinkFlow(t *testing.T) {
	e := newEnv(t)
	sub, _ := e.st.CreateSubscriber(t.Context(), store.LangEnglish)
	c, _ := e.st.AddEmail(t.Context(), sub.ID, "asha@example.test")
	e.st.VerifyContact(t.Context(), c.ID)
	browser := e.client()

	if resp, _ := e.get(browser, "/feed"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("feed while signed out: %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Unknown address: same page, no email.
	_, body := e.post(browser, "/login", url.Values{"email": {"nobody@example.test"}})
	if !strings.Contains(body, "Check your inbox") {
		t.Error("unknown address got a different page")
	}
	e.mail.none(t)

	e.post(browser, "/login", url.Values{"email": {"asha@example.test"}})
	m := e.mail.next(t)
	if m.kind != "login" {
		t.Fatalf("mail = %+v", m)
	}
	resp, _ := e.get(browser, pathOf(t, m.link))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/feed" {
		t.Fatalf("login link: %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := e.get(browser, "/feed"); resp.StatusCode != 200 {
		t.Errorf("feed after login: %d", resp.StatusCode)
	}
	e.post(browser, "/logout", nil)
	if resp, _ := e.get(browser, "/feed"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("feed after logout: %d", resp.StatusCode)
	}
}

func TestUnsubscribe(t *testing.T) {
	e := newEnv(t)
	sub, _ := e.st.CreateSubscriber(t.Context(), store.LangEnglish)
	c, _ := e.st.AddEmail(t.Context(), sub.ID, "asha@example.test")
	e.st.VerifyContact(t.Context(), c.ID)
	signed := e.signer.SignID(email.UnsubscribePurpose, c.ID)

	// Opening the link only asks: mail scanners open links too.
	_, body := e.get(e.client(), "/u/"+signed)
	if !strings.Contains(body, "Stop emailing asha@example.test?") {
		t.Error("unsubscribe page doesn't ask first")
	}
	if got, _ := e.st.Contact(t.Context(), c.ID); !got.Active {
		t.Fatal("GET unsubscribed")
	}
	// RFC 8058 one-click: a POST from the mail provider, no browser headers.
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/u/"+signed, strings.NewReader("List-Unsubscribe=One-Click"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("one-click POST: %d", resp.StatusCode)
	}
	if got, _ := e.st.Contact(t.Context(), c.ID); got.Active {
		t.Error("still active after unsubscribing")
	}
	if resp, _ := e.get(e.client(), "/u/"+signed[:len(signed)-2]+"xx"); resp.StatusCode != 404 {
		t.Errorf("tampered link: %d, want 404", resp.StatusCode)
	}
}

func TestDeleteAccount(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	e.post(c, "/join", joinForm("email", "asha@example.test", "ssc"))
	e.mail.next(t)
	contact, _ := e.st.ContactByAddress(t.Context(), store.ChannelEmail, "asha@example.test")

	e.post(c, "/account/delete", url.Values{"confirm": {"nope"}})
	if _, err := e.st.Subscriber(t.Context(), contact.SubscriberID); err != nil {
		t.Fatal("deleted without typing DELETE")
	}
	_, body := e.post(c, "/account/delete", url.Values{"confirm": {"DELETE"}})
	if !strings.Contains(body, "forgotten you") {
		t.Error("no confirmation page")
	}
	if _, err := e.st.Subscriber(t.Context(), contact.SubscriberID); err == nil {
		t.Error("subscriber still exists")
	}
}

func TestEditSettingsWhenSignedIn(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	e.post(c, "/join", joinForm("email", "asha@example.test", "ssc"))
	e.mail.next(t)
	contact, _ := e.st.ContactByAddress(t.Context(), store.ChannelEmail, "asha@example.test")

	_, body := e.get(c, "/join")
	if !strings.Contains(body, "Save changes") || !strings.Contains(body, "Currently: asha@example.test") {
		t.Error("signed-in /join isn't in edit mode")
	}
	f := joinForm("email", "asha@example.test", "ssc", "gre")
	f.Add("feed", "ssc:results")
	resp, _ := e.post(c, "/join", f)
	if resp.Header.Get("Location") != "/feed?m=saved" {
		t.Errorf("save → %q", resp.Header.Get("Location"))
	}
	e.mail.none(t) // same address: no new confirmation
	subs, _ := e.st.Subscriptions(t.Context(), contact.SubscriberID)
	if len(subs) != 1 || subs[0].Target.Category != "ssc:results" {
		t.Errorf("subscriptions = %+v, want only results", subs)
	}
}

func TestChangePage(t *testing.T) {
	e := newEnv(t)
	ch := e.change("CGL 2026 last date moved.")
	resp, body := e.get(e.client(), "/c/"+itoa(ch.ID))
	if resp.StatusCode != 200 || !strings.Contains(body, "CGL 2026 last date moved.") || !strings.Contains(body, "end: 2026-11-20") {
		t.Errorf("change page: %d\n%s", resp.StatusCode, body[strings.Index(body, "<main"):])
	}
	if resp, _ := e.get(e.client(), "/c/999"); resp.StatusCode != 404 {
		t.Errorf("missing change: %d", resp.StatusCode)
	}
}

func TestLiveEventsStreamNewChanges(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.ts.URL+"/events?cats=ssc:notice-board", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		time.Sleep(100 * time.Millisecond) // let the stream subscribe
		e.change("Fresh catch: CHSL dates moved")
		e.srv.hub.poll(ctx)
	})
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var got string
	for sc.Scan() {
		if line := sc.Text(); line == "event: change" && sc.Scan() {
			got = sc.Text()
			break
		}
	}
	wg.Wait()
	if !strings.Contains(got, "Fresh catch: CHSL dates moved") || !strings.Contains(got, `class=\"card kind-date\"`) {
		t.Errorf("change event data = %q", got)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	var last int
	for range 12 {
		resp, _ := e.post(c, "/login", url.Values{"email": {"x@example.test"}})
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("12th sign-in attempt: %d, want 429", last)
	}
}
