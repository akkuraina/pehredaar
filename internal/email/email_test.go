package email

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/akkuraina/pehredaar/internal/auth"
	"github.com/akkuraina/pehredaar/internal/notifier"
	"github.com/akkuraina/pehredaar/internal/store"
)

// fakeResend answers with the queued status codes in order (200 after the
// queue runs out) and records every request.
type fakeResend struct {
	mu       sync.Mutex
	statuses []int
	bodies   []string
	reqs     []*http.Request
	bodyOf   []message
}

func (f *fakeResend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	var m message
	json.Unmarshal(raw, &m)
	f.reqs = append(f.reqs, r)
	f.bodyOf = append(f.bodyOf, m)
	status, body := http.StatusOK, `{"id":"abc"}`
	if len(f.statuses) > 0 {
		status, f.statuses = f.statuses[0], f.statuses[1:]
		body, f.bodies = f.bodies[0], f.bodies[1:]
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "0")
	}
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func (f *fakeResend) queue(status int, body string) {
	f.statuses = append(f.statuses, status)
	f.bodies = append(f.bodies, body)
}

var signer = auth.NewSigner([]byte(strings.Repeat("k", 32)))

func newSender(t *testing.T) (*Sender, *fakeResend) {
	t.Helper()
	f := &fakeResend{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return New(Config{
		APIKey: "re_test", BaseURL: "https://pehredaar.test/", Signer: signer,
		Endpoint: srv.URL, RatePerSecond: 1000,
	}), f
}

var recipient = store.Recipient{
	Contact:  store.Contact{ID: 9, Channel: store.ChannelEmail, Address: "asha@example.test"},
	Language: store.LangEnglish,
}

var alert = notifier.Alert{
	ChangeID: 5,
	Title:    "SSC Notice Board",
	Text:     "CGL last date moved from 12 Nov to 20 Nov.\n• Last date: 12 Nov 2026 → 20 Nov 2026 <b>",
	Links:    []notifier.Link{{URL: "https://ssc.gov.in/api/attachment/x.pdf", PDF: true}},
	Lang:     store.LangEnglish,
}

func TestDeliverAlert(t *testing.T) {
	s, f := newSender(t)
	if err := s.Deliver(t.Context(), recipient, alert); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(f.reqs))
	}
	r, m := f.reqs[0], f.bodyOf[0]
	if r.Header.Get("Authorization") != "Bearer re_test" || r.Header.Get("Idempotency-Key") != "alert-5-9" {
		t.Errorf("headers: auth=%q idem=%q", r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"))
	}
	if m.From != DefaultFrom || len(m.To) != 1 || m.To[0] != "asha@example.test" {
		t.Errorf("from/to = %q %v", m.From, m.To)
	}
	if !strings.HasPrefix(m.Subject, "🦉 SSC Notice Board: CGL last date moved") {
		t.Errorf("subject = %q", m.Subject)
	}

	unsub := m.Headers["List-Unsubscribe"]
	if !strings.HasPrefix(unsub, "<https://pehredaar.test/u/") || m.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Errorf("unsubscribe headers = %v", m.Headers)
	}
	signed := strings.TrimSuffix(strings.TrimPrefix(unsub, "<https://pehredaar.test/u/"), ">")
	if id, err := signer.VerifyID(UnsubscribePurpose, signed); err != nil || id != 9 {
		t.Errorf("unsubscribe link doesn't verify to contact 9: %d, %v", id, err)
	}

	for _, want := range []string{"Caught one", "x.pdf", "https://pehredaar.test/c/5", "&lt;b&gt;", "I don't blink"} {
		if !strings.Contains(m.HTML, want) {
			t.Errorf("HTML is missing %q", want)
		}
	}
	if strings.Contains(m.HTML, "<b>") && !strings.Contains(m.HTML, "&lt;b&gt;") {
		t.Error("notice text was not escaped in HTML")
	}
	if !strings.Contains(m.Text, "20 Nov 2026") || !strings.Contains(m.Text, "Unsubscribe: https://pehredaar.test/u/") {
		t.Errorf("text part:\n%s", m.Text)
	}
}

func TestDeliverHindi(t *testing.T) {
	s, f := newSender(t)
	r := recipient
	r.Language = store.LangHindi
	a := alert
	a.Lang = store.LangHindi
	if err := s.Deliver(t.Context(), r, a); err != nil {
		t.Fatal(err)
	}
	if h := f.bodyOf[0].HTML; !strings.Contains(h, "पकड़ लिया") || !strings.Contains(h, `lang="hi"`) {
		t.Error("Hindi alert lacks Hindi fixed words")
	}
}

func TestDeliverRejectsTelegramContact(t *testing.T) {
	s, f := newSender(t)
	r := recipient
	r.Channel = store.ChannelTelegram
	if err := s.Deliver(t.Context(), r, alert); err == nil || len(f.reqs) != 0 {
		t.Errorf("err=%v requests=%d; want an error and no request", err, len(f.reqs))
	}
}

func TestRetriesRateLimitsAndServerErrors(t *testing.T) {
	s, f := newSender(t)
	f.queue(429, `{"name":"rate_limit_exceeded","message":"slow down"}`)
	f.queue(500, `{"name":"application_error","message":"oops"}`)
	if err := s.SendLogin(t.Context(), "asha@example.test", store.LangEnglish, "https://x/l"); err != nil {
		t.Fatalf("err = %v, want success on the third try", err)
	}
	if len(f.reqs) != 3 {
		t.Errorf("%d requests, want 3", len(f.reqs))
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	s, f := newSender(t)
	f.queue(403, `{"name":"validation_error","message":"You can only send testing emails to your own email address"}`)
	err := s.SendConfirm(t.Context(), "friend@example.test", store.LangEnglish, "https://x/c")
	if err == nil || len(f.reqs) != 1 {
		t.Fatalf("err=%v requests=%d; want one failed request", err, len(f.reqs))
	}
	if !IsTestingRestriction(err) {
		t.Errorf("IsTestingRestriction(%v) = false", err)
	}
}

func TestQuotaIsNotRetried(t *testing.T) {
	s, f := newSender(t)
	f.queue(429, `{"name":"daily_quota_exceeded","message":"quota"}`)
	if err := s.SendLogin(t.Context(), "a@b.test", store.LangEnglish, "https://x"); err == nil || len(f.reqs) != 1 {
		t.Errorf("err=%v requests=%d; want one failed request", err, len(f.reqs))
	}
}

func TestLinkEmails(t *testing.T) {
	s, f := newSender(t)
	if err := s.SendConfirm(t.Context(), "a@b.test", store.LangEnglish, "https://pehredaar.test/confirm/tok"); err != nil {
		t.Fatal(err)
	}
	m := f.bodyOf[0]
	if m.Subject != "🦉 Confirm your email for Pehredaar" || !strings.Contains(m.HTML, "https://pehredaar.test/confirm/tok") ||
		!strings.Contains(m.Text, "https://pehredaar.test/confirm/tok") {
		t.Errorf("confirm email: subject=%q", m.Subject)
	}
	if f.reqs[0].Header.Get("Idempotency-Key") != "" {
		t.Error("link emails must not reuse an idempotency key")
	}
	if _, ok := m.Headers["List-Unsubscribe"]; ok {
		t.Error("a confirmation email has nothing to unsubscribe from")
	}
}
