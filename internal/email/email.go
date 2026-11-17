// Package email sends Pehredaar's emails through Resend's HTTP API
// (resend.com/docs/api-reference/emails/send-email): alerts, address
// confirmations and sign-in links.
//
// Without a verified domain, Resend only delivers to the account owner's
// own address (a 403 validation_error otherwise). That is enough to
// develop against; real sending needs a domain (see README).
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/akkuraina/pehredaar/internal/auth"
	"github.com/akkuraina/pehredaar/internal/notifier"
	"github.com/akkuraina/pehredaar/internal/store"
)

const (
	defaultEndpoint = "https://api.resend.com/emails"
	// DefaultFrom is Resend's shared testing sender, usable before you
	// own a domain.
	DefaultFrom = "Pehredaar <onboarding@resend.dev>"

	maxAttempts = 3
	maxWait     = time.Minute
)

// UnsubscribePurpose is the signing purpose of one-click unsubscribe links.
// The website verifies what the email signs, so both use this constant.
const UnsubscribePurpose = "unsubscribe"

// Config configures a Sender.
type Config struct {
	APIKey  string
	From    string // default DefaultFrom
	BaseURL string // the public website, for links in emails
	Signer  *auth.Signer
	// RatePerSecond caps API calls. Resend's default account limit is
	// small (a few per second), so we stay under it rather than lean on
	// 429 retries. Default 2.
	RatePerSecond float64
	HTTP          *http.Client
	Logger        *slog.Logger
	Endpoint      string // tests only
}

// Sender sends email. It is safe for concurrent use.
type Sender struct {
	cfg     Config
	limiter *rate.Limiter
}

// New returns a Sender with defaults filled in.
func New(cfg Config) *Sender {
	if cfg.From == "" {
		cfg.From = DefaultFrom
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.RatePerSecond <= 0 {
		cfg.RatePerSecond = 2
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Sender{cfg: cfg, limiter: rate.NewLimiter(rate.Limit(cfg.RatePerSecond), 1)}
}

// message is Resend's request body. Field names are Resend's.
type message struct {
	From    string            `json:"from"`
	To      []string          `json:"to"`
	Subject string            `json:"subject"`
	HTML    string            `json:"html"`
	Text    string            `json:"text"`
	Headers map[string]string `json:"headers,omitempty"`
	Tags    []tag             `json:"tags,omitempty"`
}

type tag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// APIError is an error response from Resend.
type APIError struct {
	Status  int
	Name    string // e.g. "validation_error", "rate_limit_exceeded"
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("resend: %d %s: %s", e.Status, e.Name, e.Message)
}

// send posts one message, retrying rate limits and server errors.
// idemKey makes retries (ours, or a resend after a crash within 24h) safe:
// Resend sends a message at most once per key.
func (s *Sender) send(ctx context.Context, m message, idemKey string) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := s.limiter.Wait(ctx); err != nil {
			return err
		}
		wait, err := s.post(ctx, body, idemKey)
		if err == nil {
			return nil
		}
		last = err
		if wait < 0 {
			return err // not worth retrying
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return last
}

// post makes one API call. A negative wait means "don't retry".
func (s *Sender) post(ctx context.Context, body []byte, idemKey string) (wait time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return -1, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Pehredaar/0.1 (+https://github.com/akkuraina/pehredaar)")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		return 2 * time.Second, fmt.Errorf("resend: %w", err) // network: retry
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 == 2 {
		return 0, nil
	}

	apiErr := &APIError{Status: resp.StatusCode}
	var parsed struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		apiErr.Name, apiErr.Message = parsed.Name, parsed.Message
	} else {
		apiErr.Message = strings.TrimSpace(string(raw))
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		if apiErr.Name == "daily_quota_exceeded" || apiErr.Name == "monthly_quota_exceeded" {
			return -1, apiErr // waiting a few seconds won't help
		}
		return retryAfter(resp.Header.Get("Retry-After")), apiErr
	case resp.StatusCode >= 500:
		return 2 * time.Second, apiErr
	default:
		return -1, apiErr // 4xx: the request itself is wrong
	}
}

func retryAfter(h string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n >= 0 {
		return min(time.Duration(n)*time.Second, maxWait)
	}
	return time.Second
}

// Deliver sends an alert email to an email contact. It implements
// outbox.Sender.
func (s *Sender) Deliver(ctx context.Context, to store.Recipient, a notifier.Alert) error {
	if to.Channel != store.ChannelEmail {
		return fmt.Errorf("email: contact %d is %s, not email", to.ID, to.Channel)
	}
	unsub := s.cfg.BaseURL + "/u/" + s.cfg.Signer.SignID(UnsubscribePurpose, to.ID)
	data := alertData{
		page:        page{Lang: a.Lang, BaseURL: s.cfg.BaseURL, Unsubscribe: unsub},
		Title:       a.Title,
		Lines:       strings.Split(strings.TrimSpace(a.Text), "\n"),
		Links:       a.Links,
		ChangeURL:   fmt.Sprintf("%s/c/%d", s.cfg.BaseURL, a.ChangeID),
		HasChangeID: a.ChangeID > 0,
	}
	htmlBody, text, err := render("alert", data)
	if err != nil {
		return err
	}
	m := message{
		From:    s.cfg.From,
		To:      []string{to.Address},
		Subject: alertSubject(a),
		HTML:    htmlBody,
		Text:    text,
		// RFC 8058 one-click unsubscribe. Gmail and Yahoo require it from
		// bulk senders and show their own "Unsubscribe" button with it.
		Headers: map[string]string{
			"List-Unsubscribe":      "<" + unsub + ">",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		},
		Tags: []tag{{Name: "kind", Value: "alert"}},
	}
	return s.send(ctx, m, fmt.Sprintf("alert-%d-%d", a.ChangeID, to.ID))
}

// alertSubject is the subject line: the feed, then the start of the
// summary, so the inbox list alone says what changed.
func alertSubject(a notifier.Alert) string {
	first, _, _ := strings.Cut(strings.TrimSpace(a.Text), "\n")
	return truncate("🦉 "+a.Title+": "+first, 110)
}

// SendConfirm emails a link that confirms an address (and signs in).
func (s *Sender) SendConfirm(ctx context.Context, to string, lang store.Language, link string) error {
	return s.sendLink(ctx, "confirm", to, lang, link)
}

// SendLogin emails a sign-in link.
func (s *Sender) SendLogin(ctx context.Context, to string, lang store.Language, link string) error {
	return s.sendLink(ctx, "login", to, lang, link)
}

func (s *Sender) sendLink(ctx context.Context, kind, to string, lang store.Language, link string) error {
	data := linkData{page: page{Lang: lang, BaseURL: s.cfg.BaseURL}, Link: link}
	htmlBody, text, err := render(kind, data)
	if err != nil {
		return err
	}
	subject := subjects[kind][lang == store.LangHindi]
	// No idempotency key: each request is a new link the person asked for.
	return s.send(ctx, message{
		From: s.cfg.From, To: []string{to}, Subject: subject, HTML: htmlBody, Text: text,
		Tags: []tag{{Name: "kind", Value: kind}},
	}, "")
}

var subjects = map[string]map[bool]string{
	"confirm": {false: "🦉 Confirm your email for Pehredaar", true: "🦉 पहरेदार के लिए अपना ईमेल कन्फ़र्म करें"},
	"login":   {false: "🦉 Your Pehredaar sign-in link", true: "🦉 पहरेदार में साइन-इन का लिंक"},
}

// IsTestingRestriction reports whether err is Resend refusing to send to
// anyone but the account owner (no verified domain yet).
func IsTestingRestriction(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusForbidden &&
		strings.Contains(strings.ToLower(ae.Message), "testing")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
