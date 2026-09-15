package mail

import (
	"strings"
	"testing"

	"go.uber.org/zap"
)

// A newline smuggled into a header value ends that header and starts another.
// In a recovery email — where the recipient comes from user input — that is a
// way to add a Bcc and receive someone else's reset link.
func TestRenderStripsHeaderInjection(t *testing.T) {
	got := render("sender@example.test", Message{
		To:      "victim@example.test\r\nBcc: attacker@evil.test",
		Subject: "Reset\r\nX-Injected: yes",
		Body:    "click here",
	})

	// Injection means a NEW LINE that starts with a header name. The text
	// "Bcc:" surviving inside the flattened To value is harmless — it is a
	// value, not a header — so assert on line starts, not on substrings.
	for _, line := range strings.Split(got, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Errorf("a newline in the recipient injected a Bcc header: %q", line)
		}
		if strings.HasPrefix(line, "X-Injected") {
			t.Errorf("a newline in the subject injected a header: %q", line)
		}
	}

	if !strings.Contains(got, "To: victim@example.testBcc: attacker@evil.test\r\n") {
		t.Errorf("recipient should survive as one flattened line, got:\n%s", got)
	}
}

func TestRenderProducesWellFormedMessage(t *testing.T) {
	got := render("sender@example.test", Message{
		To:      "user@example.test",
		Subject: "Assunto com acento: ação",
		Body:    "linha um\nlinha dois",
	})

	for _, want := range []string{
		"From: sender@example.test\r\n",
		"To: user@example.test\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=UTF-8\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing header %q", want)
		}
	}

	// Headers end at the first blank line; everything after is the body, and
	// its newlines must be CRLF like the rest of the message.
	head, body, found := strings.Cut(got, "\r\n\r\n")
	if !found {
		t.Fatal("no blank line separating headers from body")
	}
	if strings.Contains(head, "linha um") {
		t.Error("body leaked into the header block")
	}
	if body != "linha um\r\nlinha dois" {
		t.Errorf("body newlines not normalised to CRLF: %q", body)
	}
}

// Handing credentials to an unauthenticated connection leaks them on every
// send, so that combination is refused at construction.
func TestSMTPRefusesCredentialsWithoutStartTLS(t *testing.T) {
	_, err := NewSMTP(Config{
		Host:     "smtp.example.test",
		From:     "noreply@example.test",
		Username: "user",
		Password: "secret",
		StartTLS: false,
	}, zap.NewNop())

	if err == nil {
		t.Fatal("credentials without StartTLS must be refused")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("error should explain the problem, got: %v", err)
	}
}

func TestSMTPAllowsAnonymousWithoutStartTLS(t *testing.T) {
	// A local mail catcher: no credentials to leak, so no TLS required.
	if _, err := NewSMTP(Config{
		Host:     "mailpit",
		Port:     1025,
		From:     "noreply@example.test",
		StartTLS: false,
	}, zap.NewNop()); err != nil {
		t.Fatalf("anonymous SMTP should be allowed: %v", err)
	}
}

func TestSMTPRequiresHostAndFrom(t *testing.T) {
	if _, err := NewSMTP(Config{From: "a@b.test"}, zap.NewNop()); err == nil {
		t.Error("missing host must be refused")
	}
	if _, err := NewSMTP(Config{Host: "smtp.test"}, zap.NewNop()); err == nil {
		t.Error("missing from address must be refused")
	}
}
