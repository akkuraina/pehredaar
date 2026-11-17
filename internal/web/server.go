// Package web is Pehredaar's public website: the landing page, the
// sign-up survey, email confirmation and sign-in, the personal feed, and
// one-click unsubscribe. It is separate from internal/dashboard (the
// operator's localhost-only view) and shares nothing with it but the store.
//
// Pages are server-rendered Go templates. JavaScript only adds motion and
// live updates (Server-Sent Events from /events); every page and form
// works without it.
package web

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/akkuraina/pehredaar/internal/auth"
	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Mailer sends the emails the website needs. *email.Sender implements it.
type Mailer interface {
	SendConfirm(ctx context.Context, to string, lang store.Language, link string) error
	SendLogin(ctx context.Context, to string, lang store.Language, link string) error
}

// Config holds the server's dependencies.
type Config struct {
	Store  store.Store
	Signer *auth.Signer
	// Mailer is nil when email isn't configured (no RESEND_API_KEY); the
	// survey then offers Telegram only.
	Mailer     Mailer
	Categories []config.Category
	// BotUsername returns the Telegram bot's @username without the @, or
	// "" if there is no bot. A func because the bot learns its name only
	// after it connects.
	BotUsername func() string
	BaseURL     string // e.g. https://pehredaar.in, no trailing slash
	Logger      *slog.Logger
}

// Server serves the website. Create it with New, run Hub in the
// background, and serve Handler.
type Server struct {
	cfg    Config
	st     store.Store
	tpl    *template.Template
	hub    *Hub
	log    *slog.Logger
	secure bool // BaseURL is https: mark cookies Secure
	now    func() time.Time

	limiter *ipLimiter

	noticesMu  sync.Mutex
	notices    []notice
	noticesAt  time.Time
	feedLabels map[string]string
}

// New builds a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.BotUsername == nil {
		cfg.BotUsername = func() string { return "" }
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	s := &Server{
		cfg:        cfg,
		st:         cfg.Store,
		log:        cfg.Logger,
		secure:     strings.HasPrefix(cfg.BaseURL, "https://"),
		now:        time.Now,
		limiter:    newIPLimiter(10, time.Minute),
		feedLabels: make(map[string]string),
	}
	for _, c := range cfg.Categories {
		s.feedLabels[c.ID] = c.Label
	}
	tpl, err := template.New("").Funcs(s.funcs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tpl = tpl
	s.hub = newHub(cfg.Store, cfg.Logger)
	return s, nil
}

// Hub returns the live-update hub; run its Run method in a goroutine.
func (s *Server) Hub() *Hub { return s.hub }

// Handler returns the site's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(24*time.Hour, http.FileServerFS(static))))

	mux.HandleFunc("GET /{$}", s.landing)
	mux.HandleFunc("GET /join", s.joinForm)
	mux.HandleFunc("POST /join", s.limited(s.joinSubmit))
	mux.HandleFunc("GET /confirm/{token}", s.confirmEmail)
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.limited(s.loginSubmit))
	mux.HandleFunc("GET /l/{token}", s.loginLink)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /feed", s.feed)
	mux.HandleFunc("GET /feed/more", s.feedMore)
	mux.HandleFunc("GET /c/{id}", s.changePage)
	mux.HandleFunc("GET /u/{signed}", s.unsubscribePage)
	mux.HandleFunc("POST /u/{signed}", s.unsubscribe)
	mux.HandleFunc("POST /account/delete", s.deleteAccount)
	mux.HandleFunc("GET /privacy", s.privacy)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("/", s.notFound) // anything else: the owl's 404, not Go's plain one

	// One-click unsubscribe (RFC 8058) is a POST from the mail provider's
	// servers, with no Origin header we could check, so it is exempt.
	cop := http.NewCrossOriginProtection()
	cop.AddInsecureBypassPattern("POST /u/{signed}")
	return securityHeaders(cop.Handler(mux))
}

// securityHeaders sets headers every response should carry. The CSP
// allows only our own scripts and styles plus Google Fonts.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; "+
			"style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; "+
			"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func cacheFor(d time.Duration, next http.Handler) http.Handler {
	v := "public, max-age=" + itoa(int64(d/time.Second))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", v)
		next.ServeHTTP(w, r)
	})
}

// --- sessions ---------------------------------------------------------------

const (
	sessionCookie = "pd_session"
	sessionTTL    = 30 * 24 * time.Hour
)

func (s *Server) setSession(w http.ResponseWriter, subscriberID int64) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.cfg.Signer.SignSession(subscriberID, sessionTTL),
		Path: "/", MaxAge: int(sessionTTL / time.Second),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
}

// current returns the signed-in subscriber, if any. A valid cookie for a
// subscriber that no longer exists (deleted account) counts as signed out.
func (s *Server) current(r *http.Request) (store.Subscriber, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.Subscriber{}, false
	}
	id, err := s.cfg.Signer.VerifySession(c.Value)
	if err != nil {
		return store.Subscriber{}, false
	}
	sub, err := s.st.Subscriber(r.Context(), id)
	if err != nil {
		return store.Subscriber{}, false
	}
	return sub, true
}

// --- rate limiting ------------------------------------------------------------

// limited caps form submissions per client IP, so the sign-up and sign-in
// forms can't be used to send floods of email.
func (s *Server) limited(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(clientIP(r), s.now()) {
			s.render(w, r, http.StatusTooManyRequests, "message", pageData{
				Title:   "Slow down",
				Heading: "Whoa, that's a lot of tries 🦉",
				Body:    "Give it a minute and try again.",
			})
			return
		}
		h(w, r)
	}
}

// clientIP is the connection's address. Behind a reverse proxy this would
// be the proxy's; reading X-Forwarded-For safely needs knowing which proxy
// to trust, which is a deploy-time decision (see README).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ipLimiter allows n events per window per IP (a fixed window: simple and
// good enough for abuse control).
type ipLimiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	seen   map[string]*bucket
}

type bucket struct {
	start time.Time
	count int
}

func newIPLimiter(n int, window time.Duration) *ipLimiter {
	return &ipLimiter{n: n, window: window, seen: make(map[string]*bucket)}
}

func (l *ipLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) > 10_000 { // drop stale entries now and then
		for k, b := range l.seen {
			if now.Sub(b.start) > l.window {
				delete(l.seen, k)
			}
		}
	}
	b := l.seen[ip]
	if b == nil || now.Sub(b.start) > l.window {
		l.seen[ip] = &bucket{start: now, count: 1}
		return true
	}
	b.count++
	return b.count <= l.n
}
