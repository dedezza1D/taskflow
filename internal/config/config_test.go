package config

import (
	"strings"
	"testing"
	"time"
)

// valid returns a config that passes Validate, so each test can break exactly
// one thing and prove that rule is what rejected it.
func valid() *Config {
	return &Config{
		Env:                      "dev",
		HTTPPort:                 "8080",
		DatabaseURL:              "postgres://u:p@localhost:5432/db",
		NATSURL:                  "nats://localhost:4222",
		NATSStreamName:           "TASKFLOW",
		NATSConsumerName:         "taskflow-worker",
		WorkerConcurrency:        10,
		WorkerMaxAttempts:        5,
		WorkerBackoffBase:        500 * time.Millisecond,
		WorkerBackoffMax:         10 * time.Second,
		WorkerAckWait:            30 * time.Second,
		WorkerHeartbeatInterval:  10 * time.Second,
		WorkerMaxProcessing:      5 * time.Minute,
		WorkerReconcileInterval:  time.Minute,
		WorkerReconcileStaleness: 10 * time.Minute,
		MaxUploadBytes:           25 << 20,
		WorkerOCRTimeout:         2 * time.Minute,
		WorkerRawRetention:       24 * time.Hour,
		AuthEnabled:              true,
		SecureCookies:            false,
		SessionTTL:               12 * time.Hour,
		ResetTokenTTL:            time.Hour,
	}
}

func TestValidBaselinePasses(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("baseline config should validate: %v", err)
	}
}

// The two production guards are the security-relevant ones: each disables a
// protection that is only safe on a single-user desktop install.
func TestProdRefusesDisabledAuth(t *testing.T) {
	c := valid()
	c.Env = "prod"
	c.SecureCookies = true
	c.AuthEnabled = false

	err := c.Validate()
	if err == nil {
		t.Fatal("ENV=prod with AUTH_ENABLED=false must be refused — it serves every request as admin")
	}
	if !strings.Contains(err.Error(), "AUTH_ENABLED") {
		t.Fatalf("error should name the offending setting, got: %v", err)
	}
}

func TestProdRefusesInsecureCookies(t *testing.T) {
	c := valid()
	c.Env = "prod"
	c.SecureCookies = false

	err := c.Validate()
	if err == nil {
		t.Fatal("ENV=prod with SECURE_COOKIES=false must be refused — the session cookie would travel in plaintext")
	}
	if !strings.Contains(err.Error(), "SECURE_COOKIES") {
		t.Fatalf("error should name the offending setting, got: %v", err)
	}
}

// Disabling auth is legitimate outside production: it is how the local desktop
// build runs. Refusing it everywhere would block Phase C.
func TestDevAllowsDisabledAuth(t *testing.T) {
	c := valid()
	c.AuthEnabled = false

	if err := c.Validate(); err != nil {
		t.Fatalf("AUTH_ENABLED=false must stay legal outside prod: %v", err)
	}
}

func TestSessionTTLMustBePositive(t *testing.T) {
	c := valid()
	c.SessionTTL = 0

	if err := c.Validate(); err == nil {
		t.Fatal("a zero session TTL must be refused")
	}
}

// OCR is a stage inside the leased attempt, so its ceiling has to fire first.
func TestOCRTimeoutMustFitInsideProcessingCeiling(t *testing.T) {
	c := valid()
	c.WorkerOCRTimeout = c.WorkerMaxProcessing

	if err := c.Validate(); err == nil {
		t.Fatal("WORKER_OCR_TIMEOUT >= WORKER_MAX_PROCESSING must be refused")
	}
}

// A recovery link built from the request Host would let an attacker mail a
// victim a link pointing at their own server, so the origin must be configured.
func TestRecoveryRequiresExplicitBaseURL(t *testing.T) {
	c := valid()
	c.SMTPHost = "smtp.example.test"
	c.AppBaseURL = ""

	if err := c.Validate(); err == nil {
		t.Fatal("recovery without APP_BASE_URL must be refused")
	}

	c.AppBaseURL = "https://taskflow.example.test"
	if err := c.Validate(); err != nil {
		t.Fatalf("recovery with a base URL should validate: %v", err)
	}
}

// Printing reset links to the log hands account takeover to anyone with log
// access — a development affordance only.
func TestProdRefusesMailLogOnly(t *testing.T) {
	c := valid()
	c.Env = "prod"
	c.SecureCookies = true
	c.MailLogOnlyMode = true
	c.AppBaseURL = "https://taskflow.example.test"

	if err := c.Validate(); err == nil {
		t.Fatal("MAIL_LOG_ONLY must be refused with ENV=prod")
	}
}

// Recovery is off by default: no SMTP host and no log-only mode means the
// endpoints stay closed, which is what the desktop build needs.
func TestRecoveryDisabledByDefault(t *testing.T) {
	if valid().RecoveryEnabled() {
		t.Fatal("recovery should be off when nothing is configured")
	}
}

// The relative default puts the API's uploads and the worker's reads in two
// different container filesystems. Nothing errors at upload; every document
// just dead-letters later. Production must name the shared mount explicitly.
func TestProdRequiresAbsoluteObjectsDir(t *testing.T) {
	c := valid()
	c.Env = "prod"
	c.SecureCookies = true
	c.ObjectsDir = "data/objects" // the Load default

	err := c.Validate()
	if err == nil {
		t.Fatal("ENV=prod with a relative OBJECTS_DIR must be refused")
	}
	if !strings.Contains(err.Error(), "OBJECTS_DIR") {
		t.Fatalf("error should name the offending setting, got: %v", err)
	}

	c.ObjectsDir = "/data/objects"
	if err := c.Validate(); err != nil {
		t.Fatalf("an absolute OBJECTS_DIR should validate in prod: %v", err)
	}

	// Dev keeps the relative default: `go run ./cmd/api` and `./cmd/worker`
	// share a working directory on the host.
	dev := valid()
	dev.ObjectsDir = "data/objects"
	if err := dev.Validate(); err != nil {
		t.Fatalf("a relative OBJECTS_DIR must stay legal outside prod: %v", err)
	}
}

func TestRawRetentionRejectsNegative(t *testing.T) {
	c := valid()
	c.WorkerRawRetention = -time.Hour

	if err := c.Validate(); err == nil {
		t.Fatal("a negative raw retention must be refused")
	}

	// Zero is legal: it means "shred as soon as the sweep sees it terminal".
	c.WorkerRawRetention = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("zero raw retention should be legal: %v", err)
	}
}
