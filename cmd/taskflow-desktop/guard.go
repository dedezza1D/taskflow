package main

// The desktop build turns authentication off, and "bound to loopback" was the
// argument that this was safe. It is not, on its own:
//
//   - Any web page the user visits can reach 127.0.0.1. Through DNS rebinding —
//     a hostname that first resolves to the attacker's server, then to
//     127.0.0.1 — the page becomes same-origin with this API and can read every
//     report and erase every document. The port is random, but a script can
//     scan for it in seconds.
//   - Even without rebinding, a page can send simple cross-site requests (a
//     form POST uploading a file) that the browser delivers without asking.
//   - Loopback is shared by every account on the machine. "The OS login is the
//     boundary" does not hold on a shared computer.
//
// So every request must pass two checks:
//
//  1. Host is exactly the loopback address this process bound. A rebound page
//     still sends its own hostname, and that alone ends the attack.
//  2. It carries the launch cookie. The binary generates a random token at
//     startup and hands it only to whoever started it, inside the URL it
//     prints; the window opens that URL once, the token is exchanged for an
//     HttpOnly, SameSite=Strict cookie, and the token leaves the address bar.
//     Another local account never sees the token, and a cross-site request
//     never carries a SameSite=Strict cookie.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
)

const (
	launchParam  = "launch"
	launchCookie = "taskflow_local"
)

func newLaunchToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// requireLoopback refuses a listen address anyone else could reach. With
// authentication off, binding a LAN interface would publish every document on
// the network; no flag value should be able to do that.
func requireLoopback(addr net.Addr) error {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		return fmt.Errorf("refusing to serve on %s: without authentication the desktop build may only listen on loopback", addr)
	}
	return nil
}

// allowedHosts lists the Host values a legitimate request can carry for the
// port this process bound.
func allowedHosts(addr net.Addr) map[string]bool {
	port := strconv.Itoa(addr.(*net.TCPAddr).Port)
	return map[string]bool{
		net.JoinHostPort("127.0.0.1", port): true,
		net.JoinHostPort("localhost", port): true,
		net.JoinHostPort("::1", port):       true,
	}
}

func withLaunchGuard(next http.Handler, hosts map[string]bool, token string) http.Handler {
	want := []byte(token)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[strings.ToLower(r.Host)] {
			http.Error(w, "unexpected Host", http.StatusForbidden)
			return
		}

		if q := r.URL.Query(); q.Has(launchParam) {
			if subtle.ConstantTimeCompare([]byte(q.Get(launchParam)), want) != 1 {
				http.Error(w, "invalid launch link; open TaskFlow Compliance from its app", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     launchCookie,
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			// Drop the token from the address bar and from any Referer.
			q.Del(launchParam)
			clean := *r.URL
			clean.RawQuery = q.Encode()
			http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
			return
		}

		c, err := r.Cookie(launchCookie)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), want) != 1 {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthenticated","details":"open TaskFlow Compliance from its app"}`))
				return
			}
			http.Error(w, "open TaskFlow Compliance from its app", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
