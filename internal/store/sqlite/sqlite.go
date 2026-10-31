// Package sqlite implements store.Store on top of SQLite, using the pure-Go
// modernc.org/sqlite driver (no cgo, so it builds on Windows without a C
// toolchain and cross-compiles cleanly).
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akkuraina/pehredaar/internal/store"

	// Blank import: the driver registers itself as "sqlite" with
	// database/sql in its init function. We never call it directly.
	_ "modernc.org/sqlite"
)

// Compile-time proof that *Store satisfies store.Store. If a method is
// missing or has the wrong signature, this line fails to build.
var _ store.Store = (*Store)(nil)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is a SQLite-backed store.Store. It is safe for concurrent use:
// *sql.DB is a connection pool that handles its own locking.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the database at path and applies any
// pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	// Pragmas are applied by the driver to every new connection in the pool:
	//   foreign_keys  SQLite ignores REFERENCES clauses unless this is on.
	//   journal_mode  WAL lets readers proceed while a writer is active.
	//   busy_timeout  wait up to 5s for a lock instead of failing at once.
	// _txlock=immediate makes transactions take the write lock up front,
	// avoiding "database is locked" errors when two read-then-write
	// transactions race to upgrade their locks.
	dsn := filepath.ToSlash(path) +
		"?_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying connection pool.
func (s *Store) Close() error { return s.db.Close() }

// migrate applies every embedded migration newer than the highest version
// recorded in schema_migrations. Each migration runs in its own transaction
// together with its bookkeeping row, so a failed migration leaves no trace.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("sqlite: create schema_migrations: %w", err)
	}

	var current int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("sqlite: read schema version: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("sqlite: list migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		// File names look like "0001_init.sql"; the number is the version.
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return fmt.Errorf("sqlite: migration %q: want NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("sqlite: migration %q: bad version: %w", e.Name(), err)
		}
		if version <= current {
			continue
		}

		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return fmt.Errorf("sqlite: read migration %q: %w", e.Name(), err)
		}
		err = s.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
				version, toMillis(s.now()))
			return err
		})
		if err != nil {
			return fmt.Errorf("sqlite: apply migration %q: %w", e.Name(), err)
		}
	}
	return nil
}

// inTx runs fn inside a transaction, committing if fn returns nil and
// rolling back otherwise.
//
// Go note: the deferred Rollback is a safety net. After a successful Commit
// it is a harmless no-op (it returns sql.ErrTxDone, which we ignore), and if
// fn panics it still releases the connection.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// scanner is the part of *sql.Row and *sql.Rows that we need, so one scan
// function can serve both single-row and multi-row queries.
type scanner interface {
	Scan(dest ...any) error
}

// Timestamps are stored as Unix milliseconds; the zero time.Time maps to NULL.

func toMillis(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func fromMillis(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.UnixMilli(n.Int64).UTC()
}

// nullID maps the domain's "0 means none" convention for IDs onto SQL NULL.
func nullID(id int64) sql.NullInt64 {
	return sql.NullInt64{Int64: id, Valid: id != 0}
}

// rowsAffectedOrNotFound turns "UPDATE matched nothing" into store.ErrNotFound.
func rowsAffectedOrNotFound(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, store.ErrNotFound)
	}
	return nil
}
