// Package store owns every PostgreSQL interaction: connection pooling, goose
// migrations and the SQL behind merge jobs.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dbtx is the subset of pgx shared by *pgxpool.Pool and pgx.Tx. Every typed
// sub-store holds one, so the same SQL works inside and outside a transaction.
type dbtx interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Row is one scannable result row.
type Row = pgx.Row

// Schema is the PostgreSQL schema every smartclass-merge object lives in. The
// name is fixed to the project and is not configurable: the pool pins
// search_path to it, so the service never creates anything in the database's
// public schema, which PostgreSQL 15 and newer reserves behind an explicit
// grant. A DSN that pins search_path itself overrides it.
const Schema = "smartclass_merge"

// Store is the pgx-backed persistence layer. The typed sub-stores are
// assembled by New and share its pool.
type Store struct {
	Pool *pgxpool.Pool

	// jobs is the merge job sub-store, exposed through Jobs.
	jobs *JobStore

	// ownsSchema is true when the DSN left search_path alone, so Schema is this
	// service's to create: Migrate creates it before goose runs. A DSN that
	// pins its own search_path owns that schema instead.
	ownsSchema bool
}

// New connects to PostgreSQL, verifies the connection and assembles the typed
// sub-stores. The connection reports application_name=smartclass-merge and
// search_path=smartclass_merge.
func New(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "smartclass-merge"
	_, pinnedSchema := cfg.ConnConfig.RuntimeParams["search_path"]
	if !pinnedSchema {
		cfg.ConnConfig.RuntimeParams["search_path"] = Schema
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{
		Pool:       pool,
		jobs:       &JobStore{db: pool},
		ownsSchema: !pinnedSchema,
	}, nil
}

// Jobs returns the merge job sub-store.
func (s *Store) Jobs() *JobStore {
	return s.jobs
}

// Ping verifies that the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.Pool.Ping(ctx)
}

// Close releases every pooled connection.
func (s *Store) Close() {
	s.Pool.Close()
}
