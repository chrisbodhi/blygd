package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestIsPublic(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "172.16.5.4", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fc00::1", "::ffff:127.0.0.1", "255.255.255.255", "198.18.0.1"} {
		if IsPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s reported public", s)
		}
	}
	for _, s := range []string{"93.184.216.34", "1.1.1.1", "2606:4700:4700::1111"} {
		if !IsPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s reported non-public", s)
		}
	}
}

func TestGuardBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) }))
	defer srv.Close()
	c := &Client{}
	_, err := c.Get(context.Background(), srv.URL, VerifyLimits, "")
	if err == nil || !errors.Is(err, ErrBlockedAddr) {
		t.Fatalf("expected blocked address, got %v", err)
	}
	c.AllowPrivate = true
	resp, err := c.Get(context.Background(), srv.URL, VerifyLimits, "")
	if err != nil || string(resp.Body) != "hi" {
		t.Fatalf("allowed fetch failed: %v", err)
	}
}

func TestBounds(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("x", 2048))) })
	mux.HandleFunc("/hop/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"x", http.StatusFound)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{AllowPrivate: true}
	lim := Limits{MaxRedirects: 3, Timeout: 100 * time.Millisecond, MaxBody: 1024}

	if _, err := c.Get(context.Background(), srv.URL+"/big", lim, ""); !errors.Is(err, ErrTooLarge) {
		t.Errorf("big body: %v", err)
	}
	if _, err := c.Get(context.Background(), srv.URL+"/hop/", lim, ""); !errors.Is(err, ErrTooManyHops) {
		t.Errorf("redirect loop: %v", err)
	}
	if _, err := c.Get(context.Background(), srv.URL+"/slow", lim, ""); err == nil {
		t.Errorf("slow response did not time out")
	}
	if _, err := c.Get(context.Background(), "file:///etc/passwd", lim, ""); !errors.Is(err, ErrNotHTTP) {
		t.Errorf("file URL: %v", err)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"120":                           2 * time.Minute,
		"Wed, 07 Oct 2026 13:00:00 GMT": time.Hour,
		"Wed, 07 Oct 2026 11:00:00 GMT": 0, // in the past
		"soon":                          0,
		"":                              0,
		"-5":                            0,
	} {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		if got := RetryAfter(h, now); got != want {
			t.Errorf("Retry-After %q: %v, want %v", v, got, want)
		}
	}
}
