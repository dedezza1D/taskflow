package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "test-launch-token"

func guarded(t *testing.T) (http.Handler, string) {
	t.Helper()
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 49152}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("reached"))
	})
	return withLaunchGuard(inner, allowedHosts(addr), testToken), "127.0.0.1:49152"
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func request(method, host, target string, cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

var validCookie = &http.Cookie{Name: launchCookie, Value: testToken}

// A page that rebinds its own hostname to 127.0.0.1 is same-origin with the API
// as far as the browser is concerned — but it still sends its own Host.
func TestGuardRejectsForeignHostEvenWithTheCookie(t *testing.T) {
	h, _ := guarded(t)
	for _, host := range []string{"evil.example:49152", "127.0.0.1:1", "127.0.0.1", "192.168.0.10:49152"} {
		rec := serve(h, request(http.MethodGet, host, "/api/v1/documents", validCookie))
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: status %d, want 403", host, rec.Code)
		}
	}
}

func TestGuardRequiresTheLaunchCookie(t *testing.T) {
	h, host := guarded(t)

	cases := []struct {
		name   string
		cookie *http.Cookie
		path   string
	}{
		{"no cookie, API", nil, "/api/v1/documents"},
		{"no cookie, UI", nil, "/"},
		{"wrong cookie", &http.Cookie{Name: launchCookie, Value: "guess"}, "/api/v1/documents"},
	}
	for _, tc := range cases {
		rec := serve(h, request(http.MethodGet, host, tc.path, tc.cookie))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", tc.name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "reached") {
			t.Errorf("%s: request reached the application", tc.name)
		}
	}

	// A cross-site form POST uploading a file is exactly a request with no cookie.
	rec := serve(h, request(http.MethodPost, host, "/api/v1/documents", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("cookieless POST: status %d, want 401", rec.Code)
	}

	for _, host := range []string{host, "localhost:49152", "LOCALHOST:49152"} {
		rec := serve(h, request(http.MethodGet, host, "/api/v1/documents", validCookie))
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q with the cookie: status %d, want 200", host, rec.Code)
		}
	}
}

func TestLaunchLinkExchangesTheTokenForACookie(t *testing.T) {
	h, host := guarded(t)

	rec := serve(h, request(http.MethodGet, host, "/?launch=wrong", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong token: status %d, want 403", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("wrong token was issued a cookie")
	}

	rec = serve(h, request(http.MethodGet, host, "/?launch="+testToken+"&tab=reports", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("right token: status %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, testToken) || loc != "/?tab=reports" {
		t.Errorf("redirect to %q; the token must leave the URL and the rest of the query stay", loc)
	}

	var got *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == launchCookie {
			got = c
		}
	}
	if got == nil {
		t.Fatal("no launch cookie issued")
	}
	if !got.HttpOnly {
		t.Error("launch cookie readable by scripts")
	}
	if got.SameSite != http.SameSiteStrictMode {
		t.Error("launch cookie is not SameSite=Strict; cross-site requests would carry it")
	}

	rec = serve(h, request(http.MethodGet, host, "/api/v1/documents", got))
	if rec.Code != http.StatusOK {
		t.Fatalf("with the issued cookie: status %d, want 200", rec.Code)
	}
}

func TestRequireLoopback(t *testing.T) {
	ok := []net.Addr{
		&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		&net.TCPAddr{IP: net.IPv6loopback, Port: 1},
	}
	for _, a := range ok {
		if err := requireLoopback(a); err != nil {
			t.Errorf("%s refused: %v", a, err)
		}
	}
	bad := []net.Addr{
		&net.TCPAddr{IP: net.IPv4zero, Port: 1},
		&net.TCPAddr{IP: net.IPv4(192, 168, 0, 10), Port: 1},
	}
	for _, a := range bad {
		if err := requireLoopback(a); err == nil {
			t.Errorf("%s accepted; it is reachable from the network", a)
		}
	}
}
