package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Env      string
	HTTPPort string
	LogLevel string

	// OpenTelemetry (traces)
	OTELExporterOTLPEndpoint string
	OTELServiceName           string

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
}

func Load() *Config {
	return &Config{
		Env:      getEnv("ENV", "dev"),
		HTTPPort: getEnv("HTTP_PORT", "8080"),
		LogLevel: getEnv("LOG_LEVEL", "info"),

		OTELExporterOTLPEndpoint: getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		OTELServiceName:           getEnv("OTEL_SERVICE_NAME", ""),

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

	return nil
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
