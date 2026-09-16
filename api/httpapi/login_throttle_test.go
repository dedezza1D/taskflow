package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/store"
)

// ---- the limiter on its own (no database) ---------------------------------

func TestLoginLimiterWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < loginMaxAttempts; i++ {
		if ok, _ := l.reserve("a@x.test"); !ok {
			t.Fatalf("attempt %d refused inside the allowance", i+1)
		}
	}
	ok, retry := l.reserve("a@x.test")
	if ok {
		t.Fatal("attempt past the allowance was let through")
	}
	if retry != loginWindow {
		t.Errorf("retryAfter = %s, want %s from the first attempt", retry, loginWindow)
	}

	if ok, _ := l.reserve("b@x.test"); !ok {
		t.Error("one address's limit spilled onto another")
	}

	now = now.Add(loginWindow)
	if ok, _ := l.reserve("a@x.test"); !ok {
		t.Error("address still refused after its window ran out")
	}
}

func TestLoginLimiterStaysBounded(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < loginMaxKeys+100; i++ {
		now = now.Add(time.Millisecond)
		l.reserve(fmt.Sprintf("made-up-%d@x.test", i))
	}
	if n := len(l.entries); n > loginMaxKeys {
		t.Fatalf("%d entries tracked; a flood of made-up addresses grows memory past the cap of %d", n, loginMaxKeys)
	}
}

// ---- through the real router ----------------------------------------------

func TestLoginIsThrottledPerAccount(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, password := makeUser(t, st, store.RoleViewer)

	for i := 0; i < loginMaxAttempts; i++ {
		resp := login(t, client, base, email, "wrong-password-"+strconv.Itoa(i))
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, resp.StatusCode)
		}
	}

	// The right password, once the allowance is spent, is refused too —
	// otherwise the 200 would tell the attacker which guess was correct.
	resp := login(t, client, base, email, password)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("correct password after %d failures: status %d, want 429", loginMaxAttempts, resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("429 without Retry-After")
	}
}

// A throttle that only engages for real accounts is a directory of who has one.
func TestLoginThrottleDoesNotRevealWhetherAccountExists(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	known, _ := makeUser(t, st, store.RoleViewer)
	unknown := "nobody-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.test"

	statuses := func(email string) []int {
		var out []int
		for i := 0; i <= loginMaxAttempts; i++ {
			resp := login(t, client, base, email, "wrong-password")
			resp.Body.Close()
			out = append(out, resp.StatusCode)
		}
		return out
	}

	k, u := statuses(known), statuses(unknown)
	if fmt.Sprint(k) != fmt.Sprint(u) {
		t.Fatalf("known account answered %v, unknown address %v; the difference reveals which exists", k, u)
	}
}

func TestSuccessfulLoginClearsTheCount(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, password := makeUser(t, st, store.RoleViewer)

	for i := 0; i < loginMaxAttempts-1; i++ {
		resp := login(t, client, base, email, "wrong-password")
		resp.Body.Close()
	}
	resp := login(t, client, base, email, password)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct password within the allowance: status %d, want 200", resp.StatusCode)
	}

	// A user who mistyped a few times and then got in starts fresh.
	for i := 0; i < loginMaxAttempts; i++ {
		resp := login(t, client, base, email, "wrong-password")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d after a successful login: status %d, want 401", i+1, resp.StatusCode)
		}
	}
}
