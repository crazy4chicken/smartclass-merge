package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// migrationFiles holds every goose migration, rooted at this package.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey is the PostgreSQL advisory lock key ("nsc-mrg" in ASCII)
// serialising goose migrations across processes.
const migrationLockKey int64 = 0x6e73632d6d7267

// Migrate applies every pending goose migration from the embedded FS behind a
// session-level advisory lock, then logs the resulting schema version. It is
// idempotent: a second call applies nothing and logs "no pending migrations".
// The database itself is never created or dropped; the project schema is
// created when it is missing. Progress and warnings go to slog.Default().
func (s *Store) Migrate(ctx context.Context) error {
	logger := slog.Default()
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
			logger.Warn("release migration lock", "error", err)
		}
	}()

	// goose creates its version table before the first migration runs, and
	// PostgreSQL cannot put objects in a schema that does not exist yet, so the
	// project schema has to be there first. A role that owns the database has
	// the CREATE privilege this needs; on a locked-down database a DBA creates
	// the schema once and grants it, and this lookup leaves it alone.
	if s.ownsSchema {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, Schema).Scan(&exists); err != nil {
			return fmt.Errorf("look up schema %s: %w", Schema, err)
		}
		if !exists {
			if _, err := conn.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{Schema}.Sanitize()); err != nil {
				return fmt.Errorf("create schema %s (the DSN role needs CREATE on the database, or a DBA creates the schema once and grants it): %w", Schema, err)
			}
			logger.Info("created schema", "schema", Schema)
		}
	}

	// Closing the sql.DB does not close the pool it was opened from.
	db := stdlib.OpenDBFromPool(s.Pool)
	defer db.Close()

	// goose collects *.sql from the root of the FS it is given, so the
	// migrations directory has to be its root.
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("open migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations, goose.WithSlog(logger))
	if err != nil {
		return fmt.Errorf("init migrations: %w", err)
	}
	applied, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	if len(applied) == 0 {
		logger.Info("no pending migrations", "version", version)
		return nil
	}
	versions := make([]int64, 0, len(applied))
	for _, result := range applied {
		versions = append(versions, result.Source.Version)
	}
	logger.Info("migrations applied", "count", len(applied), "versions", versions, "version", version)
	return nil
}
