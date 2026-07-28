package sqlite

import (
	"path/filepath"
	"testing"
)

// modernc.org/sqlite silently ignores mattn/go-sqlite3 DSN forms
// (`_journal_mode`, `_busy_timeout`, `_foreign_keys`). These tests pin
// the `_pragma=...` forms New() must use so a future "cleanup" can't
// reintroduce a DSN that opens cleanly while leaving delete-journal +
// busy_timeout=0 in production.
func TestNew_fileBackedAppliesWALAndBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pragma.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode=%q, want wal (mattn-style DSN params are ignored by modernc)", journalMode)
	}

	var busyTimeout int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout=%d, want 5000", busyTimeout)
	}

	var foreignKeys int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d, want 1", foreignKeys)
	}
}

func TestNew_memoryAppliesBusyTimeoutAndForeignKeys(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var busyTimeout int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout=%d, want 5000", busyTimeout)
	}

	var foreignKeys int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d, want 1", foreignKeys)
	}
}
