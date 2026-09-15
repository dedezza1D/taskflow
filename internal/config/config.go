package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env      string
	HTTPPort string
	LogLevel string

	// OpenTelemetry (traces)
	OTELExporterOTLPEndpoint string
	OTELServiceName          string

	DatabaseURL string

	NATSURL          string
	NATSStreamName   string
	NATSConsumerName string

	WorkerPollTimeout time.Duration
	WorkerConcurrency int
	WorkerMaxAttempts int
	WorkerMetricsPort int
	WorkerBackoffBase time.Duration
	WorkerBackoffMax  time.Duration

	// Lease: modest ack deadline kept alive by heartbeats while a handler runs,
	// plus a ceiling that bounds a single attempt (see worker.RunWithLease).
	WorkerAckWait           time.Duration
	WorkerHeartbeatInterval time.Duration
	WorkerMaxProcessing     time.Duration

	// Reconciler: periodic sweep that rescues stuck tasks (lost enqueue or a
	// crashed worker) — see cmd/worker reconcileOnce.
	WorkerReconcileInterval  time.Duration
	WorkerReconcileStaleness time.Duration

	// Document pipeline: object storage root (shared between API and worker),
	// upload bound, and OCR's own sub-ceiling (must sit under the whole-task
	// lease ceiling so a hung tesseract fails its stage, not the lease).
	ObjectsDir       string
	MaxUploadBytes   int64
	WorkerOCRTimeout time.Duration
	OCRLanguages     string

	// Authentication. AuthEnabled false runs every request as a single local
	// admin — right for the desktop build, wide open for a served deployment,
	// so it defaults to ON and must be turned off deliberately.
	AuthEnabled   bool
	SecureCookies bool
	SessionTTL    time.Duration

	// Password recovery by email. Leaving SMTPHost empty disables the feature
	// entirely — the endpoints answer 404 and the login screen hides the link.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	SMTPStartTLS bool
	// AppBaseURL is the public origin recovery links point at. Not derived from
	// the request Host, which an attacker controls.
	AppBaseURL      string
	ResetTokenTTL   time.Duration
	MailLogOnlyMode bool

	// How long a terminal document may still hold raw bytes before the
	// retention sweep destroys them. This is the safety net, not the primary
	// mechanism: a document that completes is shredded inline, immediately. The
	// window exists so an operator can diagnose a dead-lettered document before
	// its original goes — past that the bytes are pure liability.
	WorkerRawRetention time.Duration
}

func Load() *Config {
	return &Config{
		Env:      getEnv("ENV", "dev"),
		HTTPPort: getEnv("HTTP_PORT", "8080"),
		LogLevel: getEnv("LOG_LEVEL", "info"),

		OTELExporterOTLPEndpoint: getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		OTELServiceName:          getEnv("OTEL_SERVICE_NAME", ""),

		DatabaseURL: getEnv("DATABASE_URL", "postgres://taskflow:taskflow@localhost:5432/taskflow?sslmode=disable"),

		NATSURL:          getEnv("NATS_URL", "nats://localhost:4222"),
		NATSStreamName:   getEnv("NATS_STREAM_NAME", "TASKFLOW"),
		NATSConsumerName: getEnv("NATS_CONSUMER_NAME", "taskflow-worker"),

		WorkerPollTimeout: getEnvAsDuration("WORKER_POLL_TIMEOUT", 2*time.Second),
		WorkerConcurrency: getEnvAsInt("WORKER_CONCURRENCY", 10),
		WorkerMaxAttempts: getEnvAsInt("WORKER_MAX_ATTEMPTS", 5),
		WorkerMetricsPort: getEnvAsInt("WORKER_METRICS_PORT", 9091),
		WorkerBackoffBase: getEnvAsDuration("WORKER_BACKOFF_BASE", 500*time.Millisecond),
		WorkerBackoffMax:  getEnvAsDuration("WORKER_BACKOFF_MAX", 10*time.Second),

		WorkerAckWait:           getEnvAsDuration("WORKER_ACK_WAIT", 30*time.Second),
		WorkerHeartbeatInterval: getEnvAsDuration("WORKER_HEARTBEAT_INTERVAL", 10*time.Second),
		WorkerMaxProcessing:     getEnvAsDuration("WORKER_MAX_PROCESSING", 5*time.Minute),

		WorkerReconcileInterval:  getEnvAsDuration("WORKER_RECONCILE_INTERVAL", 1*time.Minute),
		WorkerReconcileStaleness: getEnvAsDuration("WORKER_RECONCILE_STALENESS", 10*time.Minute),

		ObjectsDir:       getEnv("OBJECTS_DIR", "data/objects"),
		MaxUploadBytes:   getEnvAsInt64("MAX_UPLOAD_BYTES", 25<<20),
		WorkerOCRTimeout: getEnvAsDuration("WORKER_OCR_TIMEOUT", 2*time.Minute),
		OCRLanguages:     getEnv("OCR_LANGS", "eng"),

		AuthEnabled:   getEnvAsBool("AUTH_ENABLED", true),
		SecureCookies: getEnvAsBool("SECURE_COOKIES", false),
		SessionTTL:    getEnvAsDuration("SESSION_TTL", 12*time.Hour),

		SMTPHost:        getEnv("SMTP_HOST", ""),
		SMTPPort:        getEnvAsInt("SMTP_PORT", 587),
		SMTPUsername:    getEnv("SMTP_USERNAME", ""),
		SMTPPassword:    getEnv("SMTP_PASSWORD", ""),
		SMTPFrom:        getEnv("SMTP_FROM", ""),
		SMTPStartTLS:    getEnvAsBool("SMTP_STARTTLS", true),
		AppBaseURL:      getEnv("APP_BASE_URL", ""),
		ResetTokenTTL:   getEnvAsDuration("RESET_TOKEN_TTL", time.Hour),
		MailLogOnlyMode: getEnvAsBool("MAIL_LOG_ONLY", false),

		WorkerRawRetention: getEnvAsDuration("WORKER_RAW_RETENTION", 24*time.Hour),
	}
}

func (c *Config) Validate() error {
	if c.HTTPPort == "" {
		return fmt.Errorf("HTTP_PORT is required")
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if c.NATSURL == "" {
		return fmt.Errorf("NATS_URL is required")
	}
	if c.NATSStreamName == "" {
		return fmt.Errorf("NATS_STREAM_NAME is required")
	}
	if c.NATSConsumerName == "" {
		return fmt.Errorf("NATS_CONSUMER_NAME is required")
	}
	if c.WorkerConcurrency < 1 {
		return fmt.Errorf("WORKER_CONCURRENCY must be >= 1")
	}
	if c.WorkerMaxAttempts < 1 || c.WorkerMaxAttempts > 100 {
		return fmt.Errorf("WORKER_MAX_ATTEMPTS must be 1..100")
	}
	if c.WorkerBackoffBase <= 0 {
		return fmt.Errorf("WORKER_BACKOFF_BASE must be > 0")
	}
	if c.WorkerBackoffMax <= 0 {
		return fmt.Errorf("WORKER_BACKOFF_MAX must be > 0")
	}
	if c.WorkerAckWait <= 0 {
		return fmt.Errorf("WORKER_ACK_WAIT must be > 0")
	}
	if c.WorkerHeartbeatInterval <= 0 {
		return fmt.Errorf("WORKER_HEARTBEAT_INTERVAL must be > 0")
	}
	// Heartbeats must fire well before the ack deadline, or the message can be
	// redelivered out from under a healthy worker.
	if c.WorkerHeartbeatInterval >= c.WorkerAckWait {
		return fmt.Errorf("WORKER_HEARTBEAT_INTERVAL must be < WORKER_ACK_WAIT")
	}
	if c.WorkerMaxProcessing <= 0 {
		return fmt.Errorf("WORKER_MAX_PROCESSING must be > 0")
	}
	if c.WorkerReconcileInterval <= 0 {
		return fmt.Errorf("WORKER_RECONCILE_INTERVAL must be > 0")
	}
	// Staleness must exceed the processing ceiling, or the reconciler would rescue
	// tasks that are still legitimately running under the lease.
	if c.WorkerReconcileStaleness <= c.WorkerMaxProcessing {
		return fmt.Errorf("WORKER_RECONCILE_STALENESS must be > WORKER_MAX_PROCESSING")
	}
	if c.MaxUploadBytes <= 0 {
		return fmt.Errorf("MAX_UPLOAD_BYTES must be > 0")
	}
	if c.WorkerOCRTimeout <= 0 {
		return fmt.Errorf("WORKER_OCR_TIMEOUT must be > 0")
	}
	// OCR is a stage INSIDE the leased attempt: its sub-ceiling must be smaller
	// than the whole-task ceiling or it could never fire first.
	if c.WorkerOCRTimeout >= c.WorkerMaxProcessing {
		return fmt.Errorf("WORKER_OCR_TIMEOUT must be < WORKER_MAX_PROCESSING")
	}
	// A negative TTL would sweep raw bytes out from under a document that is
	// still being retried. Zero is legal and means "shred as soon as the sweep
	// sees it terminal".
	if c.WorkerRawRetention < 0 {
		return fmt.Errorf("WORKER_RAW_RETENTION must be >= 0")
	}
	if c.SessionTTL <= 0 {
		return fmt.Errorf("SESSION_TTL must be > 0")
	}
	// Disabling auth makes every request a local admin. That is correct for a
	// single-user desktop install on loopback and catastrophic for a served
	// deployment, so refuse the combination that can only be the latter.
	if !c.AuthEnabled && c.Env == "prod" {
		return fmt.Errorf("AUTH_ENABLED=false is not allowed with ENV=prod")
	}
	// Without Secure, a session cookie is sent over plain HTTP too, so anyone
	// who can force one request to http:// captures it. In production that is
	// never an acceptable trade, wherever TLS happens to terminate.
	if !c.SecureCookies && c.Env == "prod" {
		return fmt.Errorf("SECURE_COOKIES=false is not allowed with ENV=prod")
	}
	if c.ResetTokenTTL <= 0 {
		return fmt.Errorf("RESET_TOKEN_TTL must be > 0")
	}
	// A recovery link is useless — and dangerous if guessed from the request
	// Host — without a base URL the operator chose deliberately.
	if c.RecoveryEnabled() && c.AppBaseURL == "" {
		return fmt.Errorf("APP_BASE_URL is required when password recovery is enabled")
	}
	// Printing reset links to the log is a development affordance: anyone with
	// log access could take over an account.
	if c.MailLogOnlyMode && c.Env == "prod" {
		return fmt.Errorf("MAIL_LOG_ONLY=true is not allowed with ENV=prod")
	}
	// The API writes originals where the worker must read them back, so both
	// have to name the same mounted storage. The relative default resolves
	// inside each container's own filesystem: uploads succeed, every document
	// then dead-letters at OCR, and a restart discards the bytes. That failure is
	// silent, so refuse the configuration that produces it.
	if c.Env == "prod" && !isAbsPath(c.ObjectsDir) {
		return fmt.Errorf("OBJECTS_DIR must be an absolute path on storage shared by the API and the worker when ENV=prod (got %q)", c.ObjectsDir)
	}

	return nil
}

// isAbsPath accepts a slash-rooted path on any host OS: the served build runs in
// Linux containers, while its config tests may run on Windows, where
// filepath.IsAbs("/data/objects") is false for want of a drive letter.
func isAbsPath(p string) bool {
	return strings.HasPrefix(p, "/") || filepath.IsAbs(p)
}

// RecoveryEnabled reports whether password recovery by email is configured.
func (c *Config) RecoveryEnabled() bool {
	return c.AuthEnabled && (c.SMTPHost != "" || c.MailLogOnlyMode)
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvAsInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getEnvAsInt64(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func getEnvAsBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getEnvAsDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
