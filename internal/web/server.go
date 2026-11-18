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
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
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
	// TrustedProxies are the reverse proxies in front of the site (Caddy,
	// or the hosting platform's router). Only a request arriving from one
	// of them has its X-Forwarded-For header believed; see clientIP.
	// Empty: the site is reached directly and the header is ignored.
	TrustedProxies []netip.Prefix
	Logger         *slog.Logger
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
	// Pages always link with ?v=<content hash> (assetURL), so a URL's
	// content never changes and can be cached for a year.
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(365*24*time.Hour, http.FileServerFS(static))))

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
	mux.HandleFunc("GET /start/{exams}/{signed}", s.startPage)
	mux.HandleFunc("POST /start/{exams}/{signed}", s.limited(s.startWatching))
	mux.HandleFunc("GET /privacy", s.privacy)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("/", s.notFound) // anything else: the owl's 404, not Go's plain one

	// One-click unsubscribe (RFC 8058) is a POST from the mail provider's
	// servers, with no Origin header we could check, so it is exempt.
	cop := http.NewCrossOriginProtection()
	cop.AddInsecureBypassPattern("POST /u/{signed}")
	return securityHeaders(cop.Handler(mux))
}

// securityHeaders sets headers every response should carry. The CSP
// allows only our own scripts and styles plus Google Fonts, and lets
// only our own pages and the author's portfolio embed us in a frame.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; "+
			"style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; "+
			"img-src 'self' data:; connect-src 'self'; frame-ancestors 'self' https://akanksharaina.vercel.app; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

// assetVersions maps each static file to a short hash of its contents.
// Pages link to "/static/site.css?v=<hash>", so a deploy that changes a
// file changes its URL and browsers fetch it fresh, while unchanged files
// stay cached. The files are embedded, so this is computed once.
var assetVersions = func() map[string]string {
	out := map[string]string{}
	fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[strings.TrimPrefix(path, "static/")] = hex.EncodeToString(sum[:5])
		return nil
	})
	return out
}()

// assetURL is the versioned URL of a static file (template func "asset").
func assetURL(name string) string {
	return "/static/" + name + "?v=" + assetVersions[name]
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
		if !s.limiter.allow(s.clientIP(r), s.now()) {
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

// clientIP is the visitor's address, for rate limiting.
//
// Behind a reverse proxy every connection comes from the proxy, so the
// limit would be shared by all visitors. Proxies append the address they
// received from to X-Forwarded-For; but anyone can send that header with
// made-up entries on the left. So the list is read right to left, skipping
// our own trusted proxies, and the first address that isn't ours is the
// visitor: the one entry a trusted proxy wrote that the visitor couldn't
// forge. A request that didn't come from a trusted proxy is taken at its
// connection address and its header ignored.
func (s *Server) clientIP(r *http.Request) string {
	peer := remoteAddr(r)
	if !s.trusted(peer) {
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		ip, err := netip.ParseAddr(hop)
		if err != nil {
			break // garbage: fall back to the proxy's own address
		}
		ip = ip.Unmap()
		if !s.trusted(ip) {
			return ip.String()
		}
	}
	return peer.String()
}

func (s *Server) trusted(ip netip.Addr) bool {
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func remoteAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{} // the zero Addr is never trusted
	}
	return ap.Addr().Unmap()
}

// ParseProxies parses a comma-separated list of IPs and CIDR ranges, as in
// PEHREDAAR_TRUSTED_PROXIES. The word "private" stands for the loopback
// and private ranges, where platform routers and a local Caddy live.
func ParseProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
		case part == "private":
			for _, c := range []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7", "100.64.0.0/10"} {
				out = append(out, netip.MustParsePrefix(c))
			}
		case strings.Contains(part, "/"):
			p, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", part, err)
			}
			out = append(out, p.Masked())
		default:
			ip, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", part, err)
			}
			out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
		}
	}
	return out, nil
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

// healthz is for the hosting platform's health checks: 200 while the
// process is up and the database answers, 503 otherwise, so a hung or
// broken instance gets restarted. A government site being down is not
// our ill health, so source checks don't count here.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := s.st.Ping(ctx); err != nil {
		s.log.Error("web: health check: database", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("database unavailable\n"))
		return
	}
	w.Write([]byte("ok\n"))
}
