// Package fetcher downloads watched URLs politely: it identifies itself,
// obeys robots.txt, rate-limits per host, honors Retry-After, uses
// conditional GETs, and classifies failures so the scheduler can back off
// instead of alerting.
package fetcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/temoto/robotstxt"
	"golang.org/x/time/rate"
)

// Config controls fetcher behavior. Zero fields take the defaults from
// DefaultConfig.
type Config struct {
	UserAgent    string
	Timeout      time.Duration // whole request, including reading the body
	HostInterval time.Duration // minimum gap between requests to one host
	MaxBodyBytes int64
	RobotsTTL    time.Duration
	// MaxRetryAfter caps how long a server's Retry-After can pause a host,
	// so a bogus header can't silence a source for days.
	MaxRetryAfter time.Duration
}

// DefaultConfig returns conservative settings suitable for government sites.
func DefaultConfig() Config {
	return Config{
		UserAgent:     "Pehredaar/0.1 (+https://github.com/akkuraina/pehredaar)",
		Timeout:       30 * time.Second,
		HostInterval:  5 * time.Second,
		MaxBodyBytes:  20 << 20, // 20 MiB: large recruitment PDFs exist
		RobotsTTL:     robotsTTLDefault,
		MaxRetryAfter: 6 * time.Hour,
	}
}

// Request describes one fetch. ETag and LastModified come from the previous
// successful fetch and turn the request into a conditional GET.
type Request struct {
	URL          string
	ETag         string
	LastModified string
}

// Response is a successful fetch: either new content (200) or confirmation
// that nothing changed (304, NotModified true, Body nil).
type Response struct {
	URL          string // final URL after redirects
	StatusCode   int
	NotModified  bool
	Body         []byte
	ContentType  string // media type only, e.g. "application/pdf"
	ETag         string
	LastModified string
	FetchedAt    time.Time
}

// Fetcher is safe for concurrent use. Create one and share it: the per-host
// rate limits only work if all workers go through the same Fetcher.
type Fetcher struct {
	cfg    Config
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	hosts map[string]*hostState
}

// hostState is everything we track per host (host:port).
type hostState struct {
	limiter *rate.Limiter

	// pausedUntil is set from Retry-After. Guarded by Fetcher.mu.
	pausedUntil time.Time

	robotsMu      sync.Mutex // guards the two fields below
	robots        *robotstxt.RobotsData
	robotsExpires time.Time
}

// New returns a Fetcher. Zero-valued fields in cfg are filled from
// DefaultConfig.
func New(cfg Config) *Fetcher {
	def := DefaultConfig()
	if cfg.UserAgent == "" {
		cfg.UserAgent = def.UserAgent
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = def.Timeout
	}
	if cfg.HostInterval == 0 {
		cfg.HostInterval = def.HostInterval
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = def.MaxBodyBytes
	}
	if cfg.RobotsTTL == 0 {
		cfg.RobotsTTL = def.RobotsTTL
	}
	if cfg.MaxRetryAfter == 0 {
		cfg.MaxRetryAfter = def.MaxRetryAfter
	}
	return &Fetcher{
		cfg: cfg,
		// http.Client.Timeout covers connect, headers and body. We don't set
		// Accept-Encoding ourselves, so Go's transport negotiates gzip and
		// decompresses transparently.
		client: &http.Client{Timeout: cfg.Timeout},
		now:    time.Now,
		hosts:  make(map[string]*hostState),
	}
}

func (f *Fetcher) host(host string) *hostState {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.hosts[host]
	if !ok {
		// Burst 1: never send two requests back-to-back to the same host.
		h = &hostState{limiter: rate.NewLimiter(rate.Every(f.cfg.HostInterval), 1)}
		f.hosts[host] = h
	}
	return h
}

// Fetch downloads req.URL. It blocks while waiting for the host's rate
// limit, so callers should pass a context they can cancel.
func (f *Fetcher) Fetch(ctx context.Context, req Request) (*Response, error) {
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, &Error{Kind: Permanent, URL: req.URL, Err: fmt.Errorf("not an http(s) URL")}
	}
	h := f.host(u.Host)

	// A host that told us to back off fails fast instead of making callers
	// sleep for minutes; the scheduler reschedules using RetryAfter.
	f.mu.Lock()
	wait := h.pausedUntil.Sub(f.now())
	f.mu.Unlock()
	if wait > 0 {
		return nil, &Error{Kind: Temporary, URL: req.URL, RetryAfter: wait,
			Err: errors.New("host asked us to back off")}
	}

	ok, err := f.allowed(ctx, h, u)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &Error{Kind: Disallowed, URL: req.URL, Err: errors.New("robots.txt disallows this URL")}
	}

	if err := h.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return f.do(ctx, h, req)
}

func (f *Fetcher) do(ctx context.Context, h *hostState, req Request) (*Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, &Error{Kind: Permanent, URL: req.URL, Err: err}
	}
	httpReq.Header.Set("User-Agent", f.cfg.UserAgent)
	httpReq.Header.Set("Accept", "*/*")
	if req.ETag != "" {
		httpReq.Header.Set("If-None-Match", req.ETag)
	}
	if req.LastModified != "" {
		httpReq.Header.Set("If-Modified-Since", req.LastModified)
	}

	resp, err := f.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() // we're shutting down; not the site's fault
		}
		return nil, &Error{Kind: Temporary, URL: req.URL, Err: err}
	}
	// Go note: always close the body, and drain it, or the underlying TCP
	// connection can't be reused for the next request.
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}()

	out := &Response{
		URL:          resp.Request.URL.String(),
		StatusCode:   resp.StatusCode,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		FetchedAt:    f.now(),
	}

	switch code := resp.StatusCode; {
	case code == http.StatusNotModified:
		// Servers may omit validators on a 304; keep the ones we sent.
		if out.ETag == "" {
			out.ETag = req.ETag
		}
		if out.LastModified == "" {
			out.LastModified = req.LastModified
		}
		out.NotModified = true
		return out, nil

	case code >= 200 && code < 300:
		body, err := io.ReadAll(io.LimitReader(resp.Body, f.cfg.MaxBodyBytes+1))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &Error{Kind: Temporary, URL: req.URL, StatusCode: code, Err: err}
		}
		if int64(len(body)) > f.cfg.MaxBodyBytes {
			return nil, &Error{Kind: Permanent, URL: req.URL, StatusCode: code,
				Err: fmt.Errorf("body exceeds %d bytes", f.cfg.MaxBodyBytes)}
		}
		out.Body = body
		out.ContentType = mediaType(resp.Header.Get("Content-Type"))
		return out, nil

	case code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable:
		after := parseRetryAfter(resp.Header.Get("Retry-After"), f.now())
		if after == 0 && code == http.StatusTooManyRequests {
			after = time.Minute // 429 without a hint: still slow down
		}
		after = min(after, f.cfg.MaxRetryAfter)
		if after > 0 {
			f.mu.Lock()
			h.pausedUntil = f.now().Add(after)
			f.mu.Unlock()
		}
		return nil, &Error{Kind: Temporary, URL: req.URL, StatusCode: code, RetryAfter: after}

	case code >= 500 || code == http.StatusRequestTimeout:
		return nil, &Error{Kind: Temporary, URL: req.URL, StatusCode: code}

	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return nil, &Error{Kind: Blocked, URL: req.URL, StatusCode: code}

	default: // 404, 410 and other 4xx
		return nil, &Error{Kind: Permanent, URL: req.URL, StatusCode: code}
	}
}

// mediaType strips parameters: "application/json; charset=utf-8" → "application/json".
func mediaType(header string) string {
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(header))
	}
	return mt
}

// parseRetryAfter handles both forms allowed by HTTP: delay-seconds
// ("120") and an HTTP date. It returns 0 if the header is absent or invalid.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
