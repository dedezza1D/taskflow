package httpapi

// Password-recovery tests. The properties worth locking down here are mostly
// about what the endpoints DON'T reveal and DON'T allow twice.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/auth"
	"github.com/dedezza1D/taskflow/internal/mail"
	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// captureMailer records what would have been sent, so a test can pull the
// recovery link out of the message body exactly as a user would out of an inbox.
type captureMailer struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (c *captureMailer) Send(_ context.Context, msg mail.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg)
	return nil
}

func (c *captureMailer) messages() []mail.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]mail.Message(nil), c.sent...)
}

// waitForMail polls because the handler sends on a detached goroutine — the
// response deliberately does not wait for delivery.
func (c *captureMailer) waitForMail(t *testing.T, n int) []mail.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if msgs := c.messages(); len(msgs) >= n {
			return msgs
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected %d mail(s), got %d", n, len(c.messages()))
	return nil
}

func newRecoveryTestServer(t *testing.T) (base string, client *http.Client, st *store.Store, mailer *captureMailer) {
	t.Helper()

	st = mustStore(t)
	t.Cleanup(st.Close)

	fs, err := objects.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mailer = &captureMailer{}

	srv := NewServer(Config{
		Port:        "0",
		Objects:     fs,
		AuthEnabled: true,
		Mailer:      mailer,
		BaseURL:     "https://taskflow.test",
		ResetTTL:    time.Hour,
	}, zap.NewNop(), st, nil)

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	return ts.URL, &http.Client{Jar: jar}, st, mailer
}

func forgot(t *testing.T, client *http.Client, base, email string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(forgotPasswordRequest{Email: email})
	resp, err := client.Post(base+"/api/v1/auth/forgot-password", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("forgot-password: %v", err)
	}
	return resp
}

func reset(t *testing.T, client *http.Client, base, token, password string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(resetPasswordWithTokenRequest{Token: token, NewPassword: password})
	resp, err := client.Post(base+"/api/v1/auth/reset-password", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("reset-password: %v", err)
	}
	return resp
}

// tokenFromBody extracts the token the way a user clicking the link would.
func tokenFromBody(t *testing.T, body string) string {
	t.Helper()
	const marker = "token="
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no recovery link in message body: %q", body)
	}
	rest := body[i+len(marker):]
	if end := strings.IndexAny(rest, "\r\n \t"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func TestRecoveryHappyPath(t *testing.T) {
	base, client, st, mailer := newRecoveryTestServer(t)
	email, oldPassword := makeUser(t, st, store.RoleAnalyst)

	resp := forgot(t, client, base, email)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("forgot-password: expected 204, got %d", resp.StatusCode)
	}

	msgs := mailer.waitForMail(t, 1)
	if msgs[0].To != email {
		t.Fatalf("mail addressed to %q, want %q", msgs[0].To, email)
	}
	if !strings.Contains(msgs[0].Body, "https://taskflow.test/reset-password?token=") {
		t.Fatalf("body does not carry a link on the configured base URL: %q", msgs[0].Body)
	}

	token := tokenFromBody(t, msgs[0].Body)
	newPassword := "a-brand-new-passphrase"

	got := reset(t, client, base, token, newPassword)
	got.Body.Close()
	if got.StatusCode != http.StatusNoContent {
		t.Fatalf("reset: expected 204, got %d", got.StatusCode)
	}

	// Old password dead, new one works.
	old := login(t, client, base, email, oldPassword)
	old.Body.Close()
	if old.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password still works: got %d", old.StatusCode)
	}
	fresh := login(t, client, base, email, newPassword)
	defer fresh.Body.Close()
	if fresh.StatusCode != http.StatusOK {
		t.Fatalf("new password should work: got %d", fresh.StatusCode)
	}
}

// The token is emailed; the database must hold only its hash, exactly like a
// session token.
func TestRecoveryTokenIsStoredHashed(t *testing.T) {
	base, client, st, mailer := newRecoveryTestServer(t)
	email, _ := makeUser(t, st, store.RoleViewer)

	forgot(t, client, base, email).Body.Close()
	token := tokenFromBody(t, mailer.waitForMail(t, 1)[0].Body)

	ctx := context.Background()
	if _, err := st.ConsumePasswordResetToken(ctx, token); err == nil {
		t.Fatal("the raw token was accepted — it is being stored in plaintext")
	}
	if _, err := st.ConsumePasswordResetToken(ctx, auth.HashToken(token)); err != nil {
		t.Fatalf("hashed token should resolve: %v", err)
	}
}

// A recovery link is single use. Redeeming it twice must fail loudly, not
// silently reset the password again.
func TestRecoveryTokenIsSingleUse(t *testing.T) {
	base, client, st, mailer := newRecoveryTestServer(t)
	email, _ := makeUser(t, st, store.RoleViewer)

	forgot(t, client, base, email).Body.Close()
	token := tokenFromBody(t, mailer.waitForMail(t, 1)[0].Body)

	first := reset(t, client, base, token, "first-new-passphrase")
	first.Body.Close()
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("first reset: expected 204, got %d", first.StatusCode)
	}

	second := reset(t, client, base, token, "second-new-passphrase")
	defer second.Body.Close()
	if second.StatusCode != http.StatusGone {
		t.Fatalf("second reset: expected 410, got %d", second.StatusCode)
	}
	if code := decodeErrCode(t, second); code != "token_used" {
		t.Fatalf("error code = %q, want token_used", code)
	}

	// And the second attempt's password must NOT have taken effect.
	shouldFail := login(t, client, base, email, "second-new-passphrase")
	shouldFail.Body.Close()
	if shouldFail.StatusCode != http.StatusUnauthorized {
		t.Fatal("the replayed link changed the password anyway")
	}
}

// Redeeming a link kills every live session: recovery is what someone does when
// they suspect their account is compromised.
func TestRecoveryRevokesExistingSessions(t *testing.T) {
	base, client, st, mailer := newRecoveryTestServer(t)
	email, password := makeUser(t, st, store.RoleAnalyst)

	jar, _ := cookiejar.New(nil)
	victim := &http.Client{Jar: jar}
	login(t, victim, base, email, password).Body.Close()

	alive, err := victim.Get(base + "/api/v1/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	alive.Body.Close()
	if alive.StatusCode != http.StatusOK {
		t.Fatalf("session should start alive, got %d", alive.StatusCode)
	}

	forgot(t, client, base, email).Body.Close()
	token := tokenFromBody(t, mailer.waitForMail(t, 1)[0].Body)
	reset(t, client, base, token, "recovered-passphrase-ok").Body.Close()

	after, err := victim.Get(base + "/api/v1/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session survived the recovery: got %d", after.StatusCode)
	}
}

// An unknown address must be indistinguishable from a known one, or the
// endpoint becomes a directory of who has an account — and no mail may go out.
func TestForgotPasswordDoesNotRevealAccountExistence(t *testing.T) {
	base, client, st, mailer := newRecoveryTestServer(t)
	email, _ := makeUser(t, st, store.RoleViewer)

	known := forgot(t, client, base, email)
	known.Body.Close()

	unknown := forgot(t, client, base, "nobody-"+uuid.NewString()+"@example.test")
	unknown.Body.Close()

	if known.StatusCode != unknown.StatusCode {
		t.Fatalf("status differs: known=%d unknown=%d", known.StatusCode, unknown.StatusCode)
	}
	if known.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for both, got %d", known.StatusCode)
	}

	mailer.waitForMail(t, 1)
	for _, m := range mailer.messages() {
		if m.To != email {
			t.Fatalf("mail sent to an address with no account: %q", m.To)
		}
	}
}

// Without a throttle the endpoint is a way to flood someone's inbox.
func TestForgotPasswordIsThrottledPerAccount(t *testing.T) {
	base, client, st, mailer := newRecoveryTestServer(t)
	email, _ := makeUser(t, st, store.RoleViewer)

	forgot(t, client, base, email).Body.Close()
	mailer.waitForMail(t, 1)

	// Second request inside the window: still 204, but no second mail.
	second := forgot(t, client, base, email)
	second.Body.Close()
	if second.StatusCode != http.StatusNoContent {
		t.Fatalf("throttled request must still answer 204, got %d", second.StatusCode)
	}

	time.Sleep(500 * time.Millisecond)
	if n := len(mailer.messages()); n != 1 {
		t.Fatalf("throttle did not hold: %d messages sent", n)
	}
}

func TestResetRejectsUnknownToken(t *testing.T) {
	base, client, _, _ := newRecoveryTestServer(t)

	resp := reset(t, client, base, "not-a-real-token", "a-long-enough-passphrase")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expected 410, got %d", resp.StatusCode)
	}
	if code := decodeErrCode(t, resp); code != "token_invalid" {
		t.Fatalf("error code = %q, want token_invalid", code)
	}
}

// A short password must be refused WITHOUT consuming the token, or the user
// loses their one link to a typo.
func TestWeakPasswordDoesNotBurnTheToken(t *testing.T) {
	base, client, st, mailer := newRecoveryTestServer(t)
	email, _ := makeUser(t, st, store.RoleViewer)

	forgot(t, client, base, email).Body.Close()
	token := tokenFromBody(t, mailer.waitForMail(t, 1)[0].Body)

	weak := reset(t, client, base, token, "short")
	weak.Body.Close()
	if weak.StatusCode != http.StatusBadRequest {
		t.Fatalf("weak password: expected 400, got %d", weak.StatusCode)
	}

	// The same link must still work with an acceptable password.
	good := reset(t, client, base, token, "a-perfectly-fine-passphrase")
	defer good.Body.Close()
	if good.StatusCode != http.StatusNoContent {
		t.Fatalf("token was burned by the rejected attempt: got %d", good.StatusCode)
	}
}

// With no mailer the endpoints must not pretend to work.
func TestRecoveryDisabledWithoutMailer(t *testing.T) {
	st := mustStore(t)
	defer st.Close()

	fs, _ := objects.NewFS(t.TempDir())
	srv := NewServer(Config{Port: "0", Objects: fs, AuthEnabled: true}, zap.NewNop(), st, nil)
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	body, _ := json.Marshal(forgotPasswordRequest{Email: "someone@example.test"})
	resp, err := ts.Client().Post(ts.URL+"/api/v1/auth/forgot-password", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 with no mailer, got %d", resp.StatusCode)
	}

	// And the login screen must be told, so it hides the link.
	cfgResp, err := ts.Client().Get(ts.URL + "/api/v1/auth/config")
	if err != nil {
		t.Fatal(err)
	}
	defer cfgResp.Body.Close()
	var ac authConfigResponse
	if err := json.NewDecoder(cfgResp.Body).Decode(&ac); err != nil {
		t.Fatal(err)
	}
	if ac.PasswordRecovery {
		t.Fatal("/auth/config advertises recovery that is not configured")
	}
	if !ac.AuthEnabled {
		t.Fatal("/auth/config should report auth as enabled")
	}
}
