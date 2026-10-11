// Package domain holds the smartclass-merge data model. The JSON tags are the
// HTTP contract, so they must not change without a versioned API break.
package domain

import "time"

// Status is a merge job's lifecycle state (merge_jobs.status).
type Status string

// Job status values (merge_jobs.status). A job is queued until a worker
// claims it, running while that worker merges and uploads, and terminal once
// it succeeded, failed (after max_attempts) or was canceled.
const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// Stage is the pipeline stage a running job is in (merge_jobs.stage).
type Stage string

// Job stage values (merge_jobs.stage). A queued or terminal job carries the
// empty stage: the zero value is "no stage", so there is no constant for it.
const (
	StageMerging   Stage = "merging"
	StageUploading Stage = "uploading"
)

// Segment is one recorded segment object the job merges: the filehouse key,
// the recorder's segment sequence number and its stored size in bytes.
type Segment struct {
	Key       string `json:"key"`
	Seq       int64  `json:"seq"`
	SizeBytes int64  `json:"size_bytes"`
}

// Source describes the segments one job merges and how they are decoded. The
// raw bitstream carries no timing, so FPS is what the requester declared and
// Codec the sniffed or requested elementary-stream codec.
type Source struct {
	Bucket   string    `json:"bucket"`
	Prefix   string    `json:"prefix"`
	Segments []Segment `json:"segments"`
	FPS      int       `json:"fps"`
	Codec    string    `json:"codec"`
}

// Dest is the merged object's destination. SizeBytes and SHA256 are filled in
// once the upload finished.
type Dest struct {
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

// Progress counts the work a running job has done. InputBytes is how much of
// InputTotal (the summed segment sizes) was downloaded, OutputBytes and
// UploadedBytes track the temp file and its transfer, OutputDurationMS and
// Frames come from ffmpeg's progress stream.
type Progress struct {
	InputBytes       int64 `json:"input_bytes"`
	InputTotal       int64 `json:"input_total"`
	OutputBytes      int64 `json:"output_bytes"`
	UploadedBytes    int64 `json:"uploaded_bytes"`
	OutputDurationMS int64 `json:"output_duration_ms"`
	Frames           int64 `json:"frames"`
}

// Job is one merge job: the request as accepted, the scheduler's bookkeeping
// and the result. NextAttemptAt and IdempotencyKey stay out of the job view:
// they are the scheduler's and the replay guard's, not the API's.
type Job struct {
	ID             string     `json:"id"`
	OwnerID        string     `json:"owner_id"`
	OwnerKind      string     `json:"owner_kind"`
	TeamID         string     `json:"team_id"`
	Status         Status     `json:"status"`
	Stage          Stage      `json:"stage"`
	Source         Source     `json:"source"`
	Dest           Dest       `json:"dest"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	NextAttemptAt  time.Time  `json:"-"`
	Progress       Progress   `json:"progress"`
	LastError      string     `json:"error"`
	IdempotencyKey string     `json:"-"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
}
