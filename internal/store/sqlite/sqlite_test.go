package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/store/storetest"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	// t.TempDir is deleted automatically when the test ends.
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return openTemp(t) })
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for i := range 2 {
		s, err := Open(t.Context(), path)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		var applied int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
			t.Fatal(err)
		}
		if applied != 1 {
			t.Errorf("open #%d: %d migrations recorded, want 1", i+1, applied)
		}
		s.Close()
	}
}

func TestPragmasApplied(t *testing.T) {
	s := openTemp(t)
	var fk int
	var mode string
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if fk != 1 || mode != "wal" {
		t.Errorf("foreign_keys=%d journal_mode=%q, want 1 and wal", fk, mode)
	}
}
