package fetcher

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/temoto/robotstxt"
)

// robotsAgent is the product token matched against robots.txt groups.
const robotsAgent = "Pehredaar"

// allowed reports whether robots.txt on u's host permits fetching u.
//
// The rules follow RFC 9309: a 4xx robots.txt (typically 404) means
// everything is allowed; a 5xx means assume everything is disallowed for
// now. temoto/robotstxt's FromStatusAndBytes implements exactly that. A
// network error is returned as a Temporary error and not cached, so the
// next check tries again.
func (f *Fetcher) allowed(ctx context.Context, h *hostState, u *url.URL) (bool, error) {
	// Hold the per-host robots lock across the fetch, so that when several
	// workers hit a new host at once only one of them downloads robots.txt.
	h.robotsMu.Lock()
	defer h.robotsMu.Unlock()

	if h.robots == nil || f.now().After(h.robotsExpires) {
		data, status, err := f.fetchRobots(ctx, h, u)
		if err != nil {
			return false, err
		}
		ttl := f.cfg.RobotsTTL
		if status >= 500 {
			// "Disallow all" due to a server error is a temporary state;
			// don't let it silence the host for a whole day.
			ttl = robotsErrorTTL
		}
		h.robots = data
		h.robotsExpires = f.now().Add(ttl)
	}
	return h.robots.TestAgent(u.RequestURI(), robotsAgent), nil
}

func (f *Fetcher) fetchRobots(ctx context.Context, h *hostState, u *url.URL) (*robotstxt.RobotsData, int, error) {
	robotsURL := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/robots.txt"}).String()

	if err := h.limiter.Wait(ctx); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil)
	if err != nil {
		return nil, 0, &Error{Kind: Permanent, URL: robotsURL, Err: err}
	}
	req.Header.Set("User-Agent", f.cfg.UserAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, &Error{Kind: Temporary, URL: robotsURL, Err: err}
	}
	defer resp.Body.Close()

	// robots.txt files are tiny; 512 KiB is the limit RFC 9309 says
	// crawlers must at least support.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return nil, 0, &Error{Kind: Temporary, URL: robotsURL, Err: err}
	}
	data, err := robotstxt.FromStatusAndBytes(resp.StatusCode, body)
	if err != nil {
		// Unparseable robots.txt: treat as temporary and look again later
		// rather than guessing at what the site meant.
		return nil, 0, &Error{Kind: Temporary, URL: robotsURL, StatusCode: resp.StatusCode, Err: err}
	}
	return data, resp.StatusCode, nil
}

const (
	// robotsTTLDefault is how long a fetched robots.txt is trusted. Google
	// uses up to 24 hours; so do we.
	robotsTTLDefault = 24 * time.Hour
	// robotsErrorTTL is how soon we re-check after a 5xx robots.txt.
	robotsErrorTTL = 10 * time.Minute
)
