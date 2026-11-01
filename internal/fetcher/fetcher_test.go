package fetcher

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestFetcher uses a tiny host interval so tests stay fast.
func newTestFetcher() *Fetcher {
	return New(Config{HostInterval: time.Millisecond, Timeout: 2 * time.Second})
}

// server starts an httptest server whose robots.txt returns 404 (allow all)
// unless the handler deals with /robots.txt itself.
func server(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchOK(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if ua := r.Header.Get("User-Agent"); ua != DefaultConfig().UserAgent {
			t.Errorf("User-Agent = %q", ua)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("ETag", `W/"abc"`)
		w.Header().Set("Last-Modified", "Mon, 26 Oct 2026 13:07:31 GMT")
		w.Write([]byte(`{"ok":true}`))
	})

	resp, err := newTestFetcher().Fetch(t.Context(), Request{URL: srv.URL + "/feed"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != `{"ok":true}` || resp.ContentType != "application/json" ||
		resp.ETag != `W/"abc"` || resp.LastModified == "" || resp.NotModified {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestConditionalGet(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` && r.Header.Get("If-Modified-Since") == "yesterday" {
			w.WriteHeader(http.StatusNotModified) // and deliberately no validators
			return
		}
		w.Write([]byte("changed"))
	})

	resp, err := newTestFetcher().Fetch(t.Context(),
		Request{URL: srv.URL + "/doc.pdf", ETag: `"v1"`, LastModified: "yesterday"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.NotModified || resp.Body != nil {
		t.Errorf("want NotModified with no body, got %+v", resp)
	}
	if resp.ETag != `"v1"` || resp.LastModified != "yesterday" {
		t.Errorf("validators not carried over on 304: %+v", resp)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   ErrorKind
	}{
		{http.StatusInternalServerError, Temporary},
		{http.StatusBadGateway, Temporary},
		{http.StatusRequestTimeout, Temporary},
		{http.StatusForbidden, Blocked},
		{http.StatusUnauthorized, Blocked},
		{http.StatusNotFound, Permanent},
		{http.StatusGone, Permanent},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			srv := server(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(c.status)
			})
			_, err := newTestFetcher().Fetch(t.Context(), Request{URL: srv.URL + "/x"})
			if got := KindOf(err); got != c.want {
				t.Errorf("HTTP %d: kind = %v, want %v (err: %v)", c.status, got, c.want, err)
			}
		})
	}
}

func TestRetryAfterPausesHost(t *testing.T) {
	var hits atomic.Int32
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	f := newTestFetcher()

	_, err := f.Fetch(t.Context(), Request{URL: srv.URL + "/a"})
	var fe *Error
	if !asError(err, &fe) || fe.Kind != Temporary || fe.RetryAfter != 2*time.Minute {
		t.Fatalf("first fetch err = %v", err)
	}

	// A different URL on the same host must fail fast without a request.
	_, err = f.Fetch(t.Context(), Request{URL: srv.URL + "/b"})
	if !asError(err, &fe) || fe.Kind != Temporary || fe.RetryAfter <= 0 {
		t.Fatalf("second fetch err = %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server got %d requests, want 1", n)
	}
}

func TestRobots(t *testing.T) {
	var robotsHits atomic.Int32
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robotsHits.Add(1)
			w.Write([]byte("User-agent: Pehredaar\nDisallow: /private/\n\nUser-agent: *\nDisallow: /\n"))
			return
		}
		w.Write([]byte("ok"))
	})
	f := newTestFetcher()

	if _, err := f.Fetch(t.Context(), Request{URL: srv.URL + "/public"}); err != nil {
		t.Errorf("allowed URL failed: %v", err)
	}
	_, err := f.Fetch(t.Context(), Request{URL: srv.URL + "/private/x"})
	if KindOf(err) != Disallowed {
		t.Errorf("disallowed URL: err = %v, want Disallowed", err)
	}
	if n := robotsHits.Load(); n != 1 {
		t.Errorf("robots.txt fetched %d times, want 1 (cached)", n)
	}
}

func TestRobotsServerErrorDisallows(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		t.Error("page fetched although robots.txt was unavailable")
	})
	_, err := newTestFetcher().Fetch(t.Context(), Request{URL: srv.URL + "/x"})
	if KindOf(err) != Disallowed {
		t.Errorf("err = %v, want Disallowed (RFC 9309: 5xx robots means full disallow)", err)
	}
}

func TestBodyTooLarge(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Write(make([]byte, 2048))
	})
	f := New(Config{HostInterval: time.Millisecond, MaxBodyBytes: 1024})
	_, err := f.Fetch(t.Context(), Request{URL: srv.URL + "/big.pdf"})
	if KindOf(err) != Permanent {
		t.Errorf("err = %v, want Permanent", err)
	}
}

func TestTimeoutIsTemporary(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	})
	f := New(Config{HostInterval: time.Millisecond, Timeout: 100 * time.Millisecond})
	_, err := f.Fetch(t.Context(), Request{URL: srv.URL + "/slow"})
	if KindOf(err) != Temporary {
		t.Errorf("err = %v, want Temporary", err)
	}
}

func TestRateLimitSpacesRequests(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("ok"))
	})
	const gap = 150 * time.Millisecond
	f := New(Config{HostInterval: gap})

	start := time.Now()
	// robots.txt + 2 pages = 3 requests, so at least 2 gaps.
	for _, p := range []string{"/a", "/b"} {
		if _, err := f.Fetch(t.Context(), Request{URL: srv.URL + p}); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 2*gap {
		t.Errorf("3 requests took %v, want at least %v", elapsed, 2*gap)
	}
}

func TestBadURL(t *testing.T) {
	for _, u := range []string{"ftp://example.test/x", "not a url", "https://"} {
		_, err := newTestFetcher().Fetch(t.Context(), Request{URL: u})
		if KindOf(err) != Permanent {
			t.Errorf("%q: err = %v, want Permanent", u, err)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"30":                            30 * time.Second,
		"-5":                            0,
		"garbage":                       0,
		"Sun, 01 Nov 2026 10:05:00 GMT": 5 * time.Minute,
		"Sun, 01 Nov 2026 09:00:00 GMT": 0, // in the past
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

// asError is errors.As for *Error, named to keep assertions short.
func asError(err error, target **Error) bool { return errors.As(err, target) }
