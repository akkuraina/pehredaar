package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	proxies, err := ParseProxies("private, 203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{TrustedProxies: proxies}}
	direct := &Server{} // no proxies configured

	tests := []struct {
		name   string
		srv    *Server
		remote string
		xff    []string
		want   string
	}{
		{"direct visitor", s, "198.51.100.1:5000", nil, "198.51.100.1"},
		{"direct visitor lies in the header", s, "198.51.100.1:5000", []string{"1.2.3.4"}, "198.51.100.1"},
		{"via local Caddy", s, "127.0.0.1:40000", []string{"198.51.100.9"}, "198.51.100.9"},
		{"via platform router", s, "10.1.2.3:40000", []string{"198.51.100.9"}, "198.51.100.9"},
		{"spoofed entry on the left is ignored", s, "10.1.2.3:1", []string{"6.6.6.6, 198.51.100.9"}, "198.51.100.9"},
		{"chain of our proxies", s, "127.0.0.1:1", []string{"198.51.100.9, 203.0.113.7, 10.0.0.5"}, "198.51.100.9"},
		{"header split over lines", s, "127.0.0.1:1", []string{"6.6.6.6", "198.51.100.9"}, "198.51.100.9"},
		{"garbage falls back to the proxy", s, "127.0.0.1:1", []string{"not-an-ip"}, "127.0.0.1"},
		{"no header from a proxy", s, "127.0.0.1:1", nil, "127.0.0.1"},
		{"IPv6 visitor", s, "[::1]:1", []string{"2001:db8::1"}, "2001:db8::1"},
		{"nothing trusted: header ignored", direct, "127.0.0.1:1", []string{"198.51.100.9"}, "127.0.0.1"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tt.remote
		for _, v := range tt.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := tt.srv.clientIP(r); got != tt.want {
			t.Errorf("%s: clientIP = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestParseProxies(t *testing.T) {
	for _, bad := range []string{"10.0.0.0/33", "nope", "1.2.3"} {
		if _, err := ParseProxies(bad); err == nil {
			t.Errorf("ParseProxies(%q) accepted it", bad)
		}
	}
	if p, err := ParseProxies(""); err != nil || len(p) != 0 {
		t.Errorf("empty = %v, %v", p, err)
	}
}

// Behind a proxy, two visitors must have separate rate limits.
func TestRateLimitPerVisitorBehindProxy(t *testing.T) {
	e := newEnv(t)
	e.srv.cfg.TrustedProxies, _ = ParseProxies("private")
	post := func(visitor string) int {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/login", nil)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("X-Forwarded-For", visitor)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for range 11 {
		post("198.51.100.1")
	}
	if code := post("198.51.100.1"); code != http.StatusTooManyRequests {
		t.Errorf("busy visitor: %d, want 429", code)
	}
	if code := post("198.51.100.2"); code == http.StatusTooManyRequests {
		t.Error("a second visitor was limited along with the first")
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	resp, body := e.get(e.client(), "/healthz")
	if resp.StatusCode != 200 || body != "ok\n" {
		t.Errorf("healthy: %d %q", resp.StatusCode, body)
	}
	e.st.Close()
	if resp, _ := e.get(e.client(), "/healthz"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("with the database closed: %d, want 503", resp.StatusCode)
	}
}
