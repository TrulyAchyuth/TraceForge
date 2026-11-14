// Package config loads TraceForge's configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the server configuration.
type Config struct {
	DatabaseURL     string
	HTTPAddr        string
	OTLPAddr        string
	WebDir          string
	APIToken        string
	IngestToken     string
	LabToken        string
	RetentionHours  int
	DetectInterval  time.Duration
	LabTargets      string
	LogLevel        string
	LogFormat       string
	MaxQueuedSpans  int
	ShutdownTimeout time.Duration
}

// Load reads configuration from the environment, applying defaults and
// reporting every invalid value at once.
func Load() (Config, error) {
	var errs []error
	intVar := func(name string, def, lo, hi int) int {
		raw := os.Getenv(name)
		if raw == "" {
			return def
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < lo || v > hi {
			errs = append(errs, fmt.Errorf("%s must be an integer between %d and %d", name, lo, hi))
			return def
		}
		return v
	}
	str := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	c := Config{
		DatabaseURL:     os.Getenv("TF_DATABASE_URL"),
		HTTPAddr:        str("TF_HTTP_ADDR", ":8080"),
		OTLPAddr:        str("TF_OTLP_ADDR", ":4318"),
		WebDir:          str("TF_WEB_DIR", "web/dist"),
		APIToken:        os.Getenv("TF_API_TOKEN"),
		IngestToken:     os.Getenv("TF_INGEST_TOKEN"),
		LabToken:        os.Getenv("TF_LAB_TOKEN"),
		RetentionHours:  intVar("TF_RETENTION_HOURS", 24, 1, 720),
		DetectInterval:  time.Duration(intVar("TF_DETECT_INTERVAL_SECONDS", 10, 5, 300)) * time.Second,
		LabTargets:      os.Getenv("TF_LAB_TARGETS"),
		LogLevel:        str("TF_LOG_LEVEL", "info"),
		LogFormat:       str("TF_LOG_FORMAT", "json"),
		MaxQueuedSpans:  intVar("TF_INGEST_MAX_QUEUED_SPANS", 50000, 1000, 5_000_000),
		ShutdownTimeout: time.Duration(intVar("TF_SHUTDOWN_TIMEOUT_SECONDS", 15, 1, 120)) * time.Second,
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("TF_DATABASE_URL is required"))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, errors.New("TF_LOG_LEVEL must be debug, info, warn or error"))
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, errors.New("TF_LOG_FORMAT must be json or text"))
	}
	return c, errors.Join(errs...)
}
