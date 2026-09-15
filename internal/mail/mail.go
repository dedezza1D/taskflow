// Package mail sends the few transactional messages this application needs.
//
// Deliberately small: net/smtp from the standard library, no dependency. The
// Sender interface exists so the desktop build can run without any mail at all
// (a nil Sender disables password recovery) and so tests can assert on what
// would have been sent.
package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"go.uber.org/zap"
)

type Message struct {
	To      string
	Subject string
	Body    string
}

type Sender interface {
	Send(ctx context.Context, msg Message) error
}

type Config struct {
	Host string
	Port int
	// Username empty means no authentication — normal for a local mail catcher,
	// and refused against anything else by RequireAuth below.
	Username string
	Password string
	From     string
	// StartTLS upgrades the connection before authenticating. Off only for a
	// local catcher; sending credentials in the clear otherwise is refused.
	StartTLS bool
}

type SMTPSender struct {
	cfg    Config
	logger *zap.Logger
}

func NewSMTP(cfg Config, logger *zap.Logger) (*SMTPSender, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("smtp host is required")
	}
	if cfg.From == "" {
		return nil, fmt.Errorf("smtp from address is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	// Refuse to hand credentials to a server we have not authenticated. This is
	// the one combination that silently leaks a password on every send.
	if cfg.Username != "" && !cfg.StartTLS {
		return nil, fmt.Errorf("SMTP_USERNAME set without SMTP_STARTTLS: credentials would cross the network in cleartext")
	}
	return &SMTPSender{cfg: cfg, logger: logger}, nil
}

func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprint(s.cfg.Port))

	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial smtp: %w", err)
	}

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer func() { _ = c.Quit() }()

	if s.cfg.StartTLS {
		if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := c.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("smtp from: %w", err)
	}
	if err := c.Rcpt(msg.To); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write([]byte(render(s.cfg.From, msg))); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	return nil
}

// render builds the RFC 5322 message. Header values are stripped of CR and LF
// before use: a newline smuggled into a subject or recipient would let the
// caller inject extra headers (a Bcc, say) — classic header injection.
func render(from string, msg Message) string {
	var b strings.Builder
	b.WriteString("From: " + sanitizeHeader(from) + "\r\n")
	b.WriteString("To: " + sanitizeHeader(msg.To) + "\r\n")
	b.WriteString("Subject: " + sanitizeHeader(msg.Subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(msg.Body, "\n", "\r\n"))
	return b.String()
}

func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

// LogSender writes messages to the log instead of delivering them. For local
// development without a mail catcher — and it prints the body, so a reset link
// is recoverable from the worker output.
type LogSender struct{ logger *zap.Logger }

func NewLogSender(logger *zap.Logger) *LogSender { return &LogSender{logger: logger} }

func (l *LogSender) Send(_ context.Context, msg Message) error {
	l.logger.Warn("mail not delivered (LogSender): printing instead",
		zap.String("to", msg.To),
		zap.String("subject", msg.Subject),
		zap.String("body", msg.Body),
	)
	return nil
}
