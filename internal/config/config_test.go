package config

import "testing"

func TestLoad(t *testing.T) {
	t.Setenv("TF_DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("missing TF_DATABASE_URL must be an error")
	}
	t.Setenv("TF_DATABASE_URL", "postgres://x")
	t.Setenv("TF_RETENTION_HOURS", "0")
	t.Setenv("TF_LOG_LEVEL", "loud")
	if _, err := Load(); err == nil {
		t.Fatal("invalid retention and log level must be reported")
	}
	t.Setenv("TF_RETENTION_HOURS", "48")
	t.Setenv("TF_LOG_LEVEL", "debug")
	c, err := Load()
	if err != nil || c.RetentionHours != 48 || c.HTTPAddr != ":8080" {
		t.Fatalf("got %+v, %v", c, err)
	}
}
