package main

import (
	"strings"
	"testing"
)

func TestCheckpointDualModeWarning(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("MYSQL_DSN", "")
	t.Setenv("MONGO_URI", "")

	if got := checkpointDualModeWarning(); got == "" ||
		!strings.Contains(got, "sqlite") ||
		!strings.Contains(got, "POSTGRES_DSN") {
		t.Fatalf("sqlite warning missing expected fragments, got %q", got)
	}

	t.Setenv("MYSQL_DSN", "user:pass@tcp(localhost:3306)/db")
	if got := checkpointDualModeWarning(); !strings.Contains(got, "mysql") ||
		!strings.Contains(got, "POSTGRES_DSN") ||
		!strings.Contains(got, "RUNKITE_HTTP_URL") {
		t.Fatalf("mysql warning missing expected fragments, got %q", got)
	}

	t.Setenv("MYSQL_DSN", "")
	t.Setenv("MONGO_URI", "mongodb://localhost:27017")
	if got := checkpointDualModeWarning(); !strings.Contains(got, "mongodb") ||
		!strings.Contains(got, "POSTGRES_DSN") {
		t.Fatalf("mongodb warning missing expected fragments, got %q", got)
	}

	t.Setenv("POSTGRES_DSN", "postgres://localhost/runkite")
	if got := checkpointDualModeWarning(); got != "" {
		t.Fatalf("postgres control plane should not warn, got %q", got)
	}
}
