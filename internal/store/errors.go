package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Sentinel errors returned by every store method. They are errors.Is
// compatible and wrap the underlying driver error when one is the cause.
var (
	// ErrNotFound reports a missing row.
	ErrNotFound = errors.New("not found")
	// ErrConflict reports a uniqueness or state conflict: a guarded transition
	// matched no row, or an insert collided with an existing row. The row is
	// there (or was) and its state does not admit the write.
	ErrConflict = errors.New("conflict")
	// ErrNotCancelable reports a cancel request against a job that already
	// reached a terminal state other than canceled.
	ErrNotCancelable = errors.New("status not cancelable")
	// ErrValidation reports a value rejected before it reached PostgreSQL.
	ErrValidation = errors.New("validation")
)

// PostgreSQL SQLSTATE codes handled by classify.
const (
	sqlstateUniqueViolation     = "23505"
	sqlstateForeignKeyViolation = "23503"
	sqlstateCheckViolation      = "23514"
)

// pgErrorCode returns the SQLSTATE of err, or "" when it is not a PostgreSQL
// error.
func pgErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// withSentinel wraps err with a sentinel keeping both errors.Is targets.
func withSentinel(sentinel, err error) error {
	if err == nil {
		return sentinel
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

// classify maps driver errors onto the package sentinels: no rows becomes
// ErrNotFound, unique and check violations become ErrConflict, missing foreign
// keys become ErrNotFound. Every other error is returned unchanged.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return withSentinel(ErrNotFound, err)
	}
	switch pgErrorCode(err) {
	case sqlstateUniqueViolation, sqlstateCheckViolation:
		return withSentinel(ErrConflict, err)
	case sqlstateForeignKeyViolation:
		return withSentinel(ErrNotFound, err)
	}
	return err
}

// timeOrNil maps a scanned nullable TIMESTAMPTZ column to a *time.Time: NULL
// becomes nil.
func timeOrNil(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	at := value.Time
	return &at
}
