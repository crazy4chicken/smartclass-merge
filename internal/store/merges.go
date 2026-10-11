package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crazy4chicken/smartclass-merge/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// JobStore persists merge jobs: the create/replay path, the guarded state
// transitions the scheduler and the API drive and the keyset-paginated list.
type JobStore struct{ db dbtx }

// jobColumns is selected into a domain.Job; it must stay in sync with scanJob.
// output_codec is written by Succeed but never read back: the merged object's
// codec is the engine's result, and the job view has no member for it.
const jobColumns = `id, owner_id, owner_kind, team_id, status, stage,
	source_bucket, source_prefix, source_segments, source_fps, source_codec,
	dest_bucket, dest_key, dest_size_bytes, dest_sha256,
	attempts, max_attempts, next_attempt_at,
	progress_input_bytes, progress_input_total, progress_output_bytes, progress_uploaded_bytes,
	output_duration_ms, output_frames, last_error, idempotency_key,
	created_at, updated_at, started_at, finished_at`

// Job list bounds; a filter limit of zero means the default.
const (
	defaultJobListLimit = 100
	maxJobListLimit     = 1000
)

// Visibility values accepted by ListFilter.Visibility.
const (
	VisibilityAny  = "any"
	VisibilityTeam = "team"
	VisibilityOwn  = "own"
)

// cursorSeparator splits the two members of a list cursor: an RFC 3339
// timestamp and a job id, neither of which can contain it.
const cursorSeparator = "|"

// progressLease is how far Progress pushes a job's lease forward. It matches
// the default MERGE_JOB_LEASE, and a longer configured lease is kept: progress
// only ever moves the lease forward, so a throttled reporter can never shorten
// the window an operator chose. Renew calls are the authoritative heartbeat.
const progressLease = 2 * time.Minute

// CreateResult reports whether Create inserted the job or replayed an existing
// one: with an idempotency key that owner already used, the stored job is
// returned unchanged so a retried request is not a second merge.
type CreateResult struct {
	Job      *domain.Job
	Replayed bool
}

// SucceedPatch is the engine's result as stored on a successful job: the
// uploaded object and what ffmpeg produced.
type SucceedPatch struct {
	SizeBytes  int64
	SHA256     string
	Codec      string
	DurationMS int64
	Frames     int64
}

// ListFilter narrows JobStore.List. A zero-value field does not filter, so an
// empty Status, OwnerKind or Visibility lists every value; Visibility only
// accepts VisibilityAny, VisibilityTeam or VisibilityOwn, and the API layer
// derives it from the caller's permission ladder. VisibilityTeam matches
// Team, VisibilityOwn matches Subject; a scope whose acting subject or team is
// empty matches nothing.
type ListFilter struct {
	Limit      int
	Cursor     string
	Status     domain.Status
	Visibility string
	Subject    string
	Team       string
	OwnerKind  string
}

// Page is one list page: the jobs and the cursor that continues after the last
// one. NextCursor is empty on the last page.
type Page struct {
	Items      []domain.Job
	NextCursor string
}

// Create writes one job and returns the stored row; the job it is given is
// updated in place to the same row. A missing id, status or next_attempt_at is
// filled in (ULID, queued, now). When the job carries an idempotency key that
// its owner already used, nothing is inserted and the stored job comes back
// with Replayed set, so a client retrying a create after a timeout gets the
// same job instead of a second merge. MaxAttempts comes from the caller
// (MERGE_MAX_ATTEMPTS) and must be positive.
func (j *JobStore) Create(ctx context.Context, job *domain.Job) (CreateResult, error) {
	if job == nil {
		return CreateResult{}, fmt.Errorf("%w: nil job", ErrValidation)
	}
	if job.MaxAttempts <= 0 {
		return CreateResult{}, fmt.Errorf("%w: max_attempts must be positive", ErrValidation)
	}
	if job.ID == "" {
		job.ID = domain.NewID()
	}
	if job.Status == "" {
		job.Status = domain.StatusQueued
	}
	if job.NextAttemptAt.IsZero() {
		job.NextAttemptAt = time.Now()
	}
	segments, err := marshalSegments(job.Source.Segments)
	if err != nil {
		return CreateResult{}, fmt.Errorf("create merge job %s: %w", job.ID, err)
	}

	const insert = `
		INSERT INTO merge_jobs (id, owner_id, owner_kind, team_id, status,
		                        source_bucket, source_prefix, source_segments, source_fps, source_codec,
		                        dest_bucket, dest_key, max_attempts, next_attempt_at, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (owner_id, idempotency_key) WHERE idempotency_key <> '' DO NOTHING
		RETURNING ` + jobColumns
	stored, err := scanJob(j.db.QueryRow(ctx, insert,
		job.ID, job.OwnerID, job.OwnerKind, job.TeamID, job.Status,
		job.Source.Bucket, job.Source.Prefix, segments, job.Source.FPS, job.Source.Codec,
		job.Dest.Bucket, job.Dest.Key, job.MaxAttempts, job.NextAttemptAt, job.IdempotencyKey))
	if err == nil {
		*job = *stored
		return CreateResult{Job: job}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return CreateResult{}, fmt.Errorf("create merge job %s: %w", job.ID, err)
	}

	// The insert was skipped by the idempotency index: the owner already
	// created a job under this key, so replay it. No other statement removes a
	// job row, so the conflicting row is still there.
	const replay = `
		SELECT ` + jobColumns + `
		FROM merge_jobs
		WHERE owner_id = $1 AND idempotency_key = $2`
	stored, err = scanJob(j.db.QueryRow(ctx, replay, job.OwnerID, job.IdempotencyKey))
	if err != nil {
		return CreateResult{}, fmt.Errorf("replay merge job for owner %s: %w", job.OwnerID, err)
	}
	*job = *stored
	return CreateResult{Job: job, Replayed: true}, nil
}

// Get returns one job by id. It returns ErrNotFound when no such job exists.
func (j *JobStore) Get(ctx context.Context, id string) (*domain.Job, error) {
	const query = `SELECT ` + jobColumns + ` FROM merge_jobs WHERE id = $1`
	job, err := scanJob(j.db.QueryRow(ctx, query, id))
	if err != nil {
		return nil, fmt.Errorf("get merge job %s: %w", id, err)
	}
	return job, nil
}

// Cancel moves a queued or running job to canceled and returns the stored row.
// Canceling an already canceled job is a no-op that returns it unchanged, so
// the endpoint is idempotent; a job that already succeeded or failed reports
// ErrNotCancelable and an unknown id reports ErrNotFound.
func (j *JobStore) Cancel(ctx context.Context, id string) (*domain.Job, error) {
	const query = `
		UPDATE merge_jobs
		SET status = 'canceled', stage = '', lease_owner = '', lease_expires_at = NULL,
		    finished_at = now(), updated_at = now()
		WHERE id = $1 AND status IN ('queued','running')
		RETURNING ` + jobColumns
	job, err := scanJob(j.db.QueryRow(ctx, query, id))
	if err == nil {
		return job, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("cancel merge job %s: %w", id, err)
	}
	// The guard matched no live row: the job is no longer queued or running.
	current, err := j.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("cancel merge job %s: %w", id, err)
	}
	if current.Status != domain.StatusCanceled {
		return nil, fmt.Errorf("cancel merge job %s: %w", id, ErrNotCancelable)
	}
	return current, nil
}

// Claim atomically takes up to limit claimable jobs for owner: queued jobs
// whose next_attempt_at came due, plus running jobs whose lease expired, which
// is how a job whose worker died gets picked up again. Each claim moves the job
// to running with an empty stage, stamps the lease and started_at (once) and
// increments attempts. Jobs another worker is processing are skipped (FOR
// UPDATE SKIP LOCKED); progress counters are left alone until the new worker
// reports progress. An expired lease is claimed even when attempts already
// reached max_attempts: a crashed worker must not strand a job in running, and
// the next failure makes it terminal instead.
func (j *JobStore) Claim(ctx context.Context, owner string, limit int, lease time.Duration, now time.Time) ([]domain.Job, error) {
	const query = `
		UPDATE merge_jobs
		SET status = 'running', stage = '', attempts = attempts + 1,
		    lease_owner = $1,
		    lease_expires_at = $2::timestamptz + make_interval(secs => $3::double precision),
		    started_at = COALESCE(started_at, $2::timestamptz),
		    updated_at = $2::timestamptz
		WHERE id IN (
			SELECT id FROM merge_jobs
			WHERE (status = 'queued' AND next_attempt_at <= $2::timestamptz)
			   OR (status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at < $2::timestamptz)
			ORDER BY next_attempt_at, id
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		RETURNING ` + jobColumns
	jobs, err := j.query(ctx, query, owner, now, lease.Seconds(), claimBatchLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("claim merge jobs for %s: %w", owner, err)
	}
	return jobs, nil
}

// Renew extends the lease of a running job this owner holds and reports whether
// the row matched. A false result means the job is no longer running here — it
// was canceled or its lease was reclaimed — so the caller must stop working on
// it.
func (j *JobStore) Renew(ctx context.Context, id, owner string, lease time.Duration) (bool, error) {
	const query = `
		UPDATE merge_jobs
		SET lease_expires_at = now() + make_interval(secs => $3::double precision), updated_at = now()
		WHERE id = $1 AND lease_owner = $2 AND status = 'running'`
	tag, err := j.db.Exec(ctx, query, id, owner, lease.Seconds())
	if err != nil {
		return false, fmt.Errorf("renew merge job %s: %w", id, classify(err))
	}
	return tag.RowsAffected() > 0, nil
}

// Progress stores the latest counters of a running job this owner holds, moves
// it to stage and pushes its lease forward, so a long merge is never stolen
// while it is reporting. A false result means the row did not match, with the
// same meaning as in Renew.
func (j *JobStore) Progress(ctx context.Context, id, owner string, p domain.Progress, stage domain.Stage) (bool, error) {
	const query = `
		UPDATE merge_jobs
		SET stage = $3,
		    progress_input_bytes = $4, progress_input_total = $5,
		    progress_output_bytes = $6, progress_uploaded_bytes = $7,
		    output_duration_ms = $8, output_frames = $9,
		    lease_expires_at = GREATEST(COALESCE(lease_expires_at, now()), now() + make_interval(secs => $10::double precision)),
		    updated_at = now()
		WHERE id = $1 AND lease_owner = $2 AND status = 'running'`
	tag, err := j.db.Exec(ctx, query, id, owner, stage,
		p.InputBytes, p.InputTotal, p.OutputBytes, p.UploadedBytes, p.OutputDurationMS, p.Frames,
		progressLease.Seconds())
	if err != nil {
		return false, fmt.Errorf("update merge job %s progress: %w", id, classify(err))
	}
	return tag.RowsAffected() > 0, nil
}

// Status returns a job's current status without locking it. The scheduler polls
// it while a merge runs to notice a cancel that arrived through the API. An
// unknown id reports ErrNotFound.
func (j *JobStore) Status(ctx context.Context, id string) (domain.Status, error) {
	const query = `SELECT status FROM merge_jobs WHERE id = $1`
	var status domain.Status
	if err := j.db.QueryRow(ctx, query, id).Scan(&status); err != nil {
		return "", fmt.Errorf("status of merge job %s: %w", id, classify(err))
	}
	return status, nil
}

// Succeed stores the engine's result and closes the job: the destination object
// and what ffmpeg produced. It only applies to a running job this owner still
// holds and reports ErrConflict otherwise, so the result of a job that was
// canceled or reclaimed meanwhile never overwrites its terminal state. The
// error of earlier attempts is cleared.
func (j *JobStore) Succeed(ctx context.Context, id, owner string, res SucceedPatch) (*domain.Job, error) {
	const query = `
		UPDATE merge_jobs
		SET status = 'succeeded', stage = '',
		    dest_size_bytes = $3, dest_sha256 = $4, output_codec = $5,
		    output_duration_ms = $6, output_frames = $7,
		    progress_output_bytes = $3, progress_uploaded_bytes = $3,
		    last_error = '', lease_owner = '', lease_expires_at = NULL,
		    finished_at = now(), updated_at = now()
		WHERE id = $1 AND lease_owner = $2 AND status = 'running'
		RETURNING ` + jobColumns
	job, err := scanJob(j.db.QueryRow(ctx, query, id, owner,
		res.SizeBytes, res.SHA256, res.Codec, res.DurationMS, res.Frames))
	if err != nil {
		return nil, fmt.Errorf("succeed merge job %s: %w", id, guardError(err))
	}
	return job, nil
}

// Fail records why a running job this owner holds could not finish. A non-zero
// retryAt requeues it for that instant (stage and lease cleared, attempts kept);
// a zero retryAt makes it terminal. Like Succeed it only applies to a running
// job this owner still holds and reports ErrConflict otherwise, so a late
// failure cannot overwrite a canceled job.
func (j *JobStore) Fail(ctx context.Context, id, owner, msg string, retryAt time.Time) (*domain.Job, error) {
	const query = `
		UPDATE merge_jobs
		SET status = CASE WHEN $4::timestamptz IS NULL THEN 'failed' ELSE 'queued' END,
		    stage = '',
		    last_error = $3,
		    next_attempt_at = COALESCE($4::timestamptz, next_attempt_at),
		    finished_at = CASE WHEN $4::timestamptz IS NULL THEN now() ELSE finished_at END,
		    lease_owner = '', lease_expires_at = NULL,
		    updated_at = now()
		WHERE id = $1 AND lease_owner = $2 AND status = 'running'
		RETURNING ` + jobColumns
	var retry *time.Time
	if !retryAt.IsZero() {
		retry = &retryAt
	}
	job, err := scanJob(j.db.QueryRow(ctx, query, id, owner, msg, retry))
	if err != nil {
		return nil, fmt.Errorf("fail merge job %s: %w", id, guardError(err))
	}
	return job, nil
}

// List returns one page of jobs matching f, newest first. Paging is keyset-based
// on (created_at, id), the same order the query reads, so a page never repeats or
// skips a job: pass Page.NextCursor back as ListFilter.Cursor until it is empty.
// A malformed cursor and an unknown visibility report ErrValidation.
func (j *JobStore) List(ctx context.Context, f ListFilter) (Page, error) {
	if f.Visibility != "" && f.Visibility != VisibilityAny && f.Visibility != VisibilityTeam && f.Visibility != VisibilityOwn {
		return Page{}, fmt.Errorf("%w: unknown visibility %q", ErrValidation, f.Visibility)
	}
	var cursor *time.Time
	var cursorID string
	if f.Cursor != "" {
		at, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return Page{}, fmt.Errorf("%w: %v", ErrValidation, err)
		}
		cursor, cursorID = &at, id
	}
	limit := jobListLimit(f.Limit)
	const query = `
		SELECT ` + jobColumns + `
		FROM merge_jobs
		WHERE ($1 = '' OR status = $1)
		  AND ($2 = '' OR owner_kind = $2)
		  AND ($3 = '' OR $3 = 'any'
		       OR ($3 = 'team' AND team_id = $4)
		       OR ($3 = 'own' AND owner_id = $5))
		  AND ($6::timestamptz IS NULL OR (created_at, id) < ($6::timestamptz, $7::text))
		ORDER BY created_at DESC, id DESC
		LIMIT $8`
	jobs, err := j.query(ctx, query, f.Status, f.OwnerKind, f.Visibility, f.Team, f.Subject,
		cursor, cursorID, limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("list merge jobs: %w", err)
	}
	// One row more than asked for tells whether another page exists without a
	// second round trip; that row stays out of the page.
	if len(jobs) > limit {
		jobs = jobs[:limit]
		return Page{Items: jobs, NextCursor: encodeCursor(jobs[limit-1])}, nil
	}
	return Page{Items: jobs}, nil
}

// query runs a SELECT over jobColumns and scans every row.
func (j *JobStore) query(ctx context.Context, sql string, args ...any) ([]domain.Job, error) {
	rows, err := j.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	jobs := make([]domain.Job, 0, 16)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *job)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return jobs, nil
}

// scanJob reads one row selected with jobColumns.
func scanJob(row pgx.Row) (*domain.Job, error) {
	var (
		job         domain.Job
		rawSegments []byte
		startedAt   pgtype.Timestamptz
		finishedAt  pgtype.Timestamptz
	)
	err := row.Scan(
		&job.ID, &job.OwnerID, &job.OwnerKind, &job.TeamID, &job.Status, &job.Stage,
		&job.Source.Bucket, &job.Source.Prefix, &rawSegments, &job.Source.FPS, &job.Source.Codec,
		&job.Dest.Bucket, &job.Dest.Key, &job.Dest.SizeBytes, &job.Dest.SHA256,
		&job.Attempts, &job.MaxAttempts, &job.NextAttemptAt,
		&job.Progress.InputBytes, &job.Progress.InputTotal, &job.Progress.OutputBytes,
		&job.Progress.UploadedBytes, &job.Progress.OutputDurationMS, &job.Progress.Frames,
		&job.LastError, &job.IdempotencyKey,
		&job.CreatedAt, &job.UpdatedAt, &startedAt, &finishedAt,
	)
	if err != nil {
		return nil, classify(err)
	}
	if err := json.Unmarshal(rawSegments, &job.Source.Segments); err != nil {
		return nil, fmt.Errorf("decode source_segments column: %w", err)
	}
	if job.Source.Segments == nil {
		job.Source.Segments = []domain.Segment{}
	}
	job.StartedAt = timeOrNil(startedAt)
	job.FinishedAt = timeOrNil(finishedAt)
	return &job, nil
}

// marshalSegments renders the segment list for the source_segments column; an
// empty list is stored as [] rather than null.
func marshalSegments(segments []domain.Segment) ([]byte, error) {
	if len(segments) == 0 {
		return []byte("[]"), nil
	}
	payload, err := json.Marshal(segments)
	if err != nil {
		return nil, fmt.Errorf("encode source segments: %w", err)
	}
	return payload, nil
}

// encodeCursor renders a job's position as the list cursor: the (created_at,
// id) pair the next page continues after, base64url encoded so clients treat
// the value as opaque.
func encodeCursor(job domain.Job) string {
	raw := job.CreatedAt.UTC().Format(time.RFC3339Nano) + cursorSeparator + job.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses a cursor produced by encodeCursor.
func decodeCursor(cursor string) (time.Time, string, error) {
	malformed := errors.New("malformed cursor")
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", malformed
	}
	at, id, found := strings.Cut(string(raw), cursorSeparator)
	if !found || id == "" {
		return time.Time{}, "", malformed
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, "", malformed
	}
	return parsed.UTC(), id, nil
}

// guardError maps a guarded transition that matched no row onto ErrConflict:
// the job exists, it just is not running under this lease any more.
func guardError(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrConflict
	}
	return err
}

// claimBatchLimit clamps a claim batch to a sane positive size; a zero or
// negative limit means the caller wants the default of one.
func claimBatchLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	return limit
}

// jobListLimit applies the default and maximum page sizes.
func jobListLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultJobListLimit
	case limit > maxJobListLimit:
		return maxJobListLimit
	default:
		return limit
	}
}
