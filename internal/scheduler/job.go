package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/crazy4chicken/smartclass-merge/internal/domain"
	"github.com/crazy4chicken/smartclass-merge/internal/merge"
	"github.com/crazy4chicken/smartclass-merge/internal/store"
)

// runJob performs one claimed attempt: it marks the starting stage, runs the
// engine behind the progress heartbeat and records the terminal outcome. Every
// path leaves the row in a state the control plane can see — merging or
// uploading while it runs, succeeded, queued with the next attempt, failed, or
// canceled by a caller.
func (s *Scheduler) runJob(ctx context.Context, cancel context.CancelFunc, job domain.Job) {
	started := time.Now()
	s.log.Info("scheduler: job started",
		"job_id", job.ID,
		"attempt", job.Attempts,
		"max_attempts", s.maxAttempts(job),
		"segments", len(job.Source.Segments),
		"source", job.Source.Bucket,
		"dest", job.Dest.Bucket+"/"+job.Dest.Key,
	)

	// Mark the stage before the first byte moves so the API shows a working
	// job immediately, instead of the empty stage a fresh claim leaves behind.
	ok, err := s.store.Jobs().Progress(ctx, job.ID, s.opts.Owner, job.Progress, domain.StageMerging)
	if err != nil {
		s.log.Warn("scheduler: mark job stage", "job_id", job.ID, "error", err)
	} else if !ok {
		// The row stopped being ours between the claim and this write: it was
		// canceled, or its lease was stolen. Nobody waits for this merge, so
		// it must not start.
		s.log.Warn("scheduler: job no longer claimable, dropping run", "job_id", job.ID, "owner", s.opts.Owner)
		return
	}

	progress := newReporter(s, ctx, cancel, job)
	go progress.supervise()
	res, runErr := s.eng.Run(ctx, merge.RunInput{
		JobID:        job.ID,
		SourceBucket: job.Source.Bucket,
		Segments:     job.Source.Segments,
		DestBucket:   job.Dest.Bucket,
		DestKey:      job.Dest.Key,
		FPS:          job.Source.FPS,
		Codec:        job.Source.Codec,
	}, progress.report)
	progress.stop()

	s.settle(ctx, job, res, runErr, started)
}

// settle records the terminal outcome of one attempt. The stored status is
// re-read first: a canceled row always wins, because Cancel writes the status
// before it aborts the run, and a terminal write must never resurrect a job
// the operator stopped. An attempt aborted through the run context — by
// Cancel or by the drain — is decided by the canceler, never recorded as a job
// failure; an aborted drain leaves the row running so the expiring lease hands
// the job to the next claim.
func (s *Scheduler) settle(runCtx context.Context, job domain.Job, res merge.Result, runErr error, started time.Time) {
	// A run context that ended in a cancel — Cancel(jobID), a lost lease or the
	// drain — means the canceler owns the outcome; whatever error the engine
	// surfaced while unwinding is beside the point. A JobTimeout ends the
	// context with a deadline instead, and that is a real failure.
	aborted := errors.Is(runCtx.Err(), context.Canceled)

	// The merge context is already canceled on the abort paths; terminal
	// writes run on a short detached context so the outcome of work that
	// actually finished still lands.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), terminalWriteTimeout)
	defer cancel()
	duration := time.Since(started)

	status, err := s.store.Jobs().Status(ctx, job.ID)
	if err != nil {
		// Without the status there is no way to tell a canceled job from a
		// failed one, and overwriting a cancel is the one outcome that must
		// never happen. The lease expires and the job is claimed again.
		s.log.Error("scheduler: read job status before settling", "job_id", job.ID, "duration", duration.String(), "error", err)
		return
	}
	if status == domain.StatusCanceled {
		s.log.Info("scheduler: job canceled, keeping its stored status", "job_id", job.ID, "attempt", job.Attempts, "duration", duration.String())
		return
	}
	if runErr == nil {
		s.recordSuccess(ctx, job, res, duration)
		return
	}
	if aborted {
		s.log.Info("scheduler: job attempt aborted, leaving the lease to expire",
			"job_id", job.ID, "attempt", job.Attempts, "duration", duration.String(), "error", runErr)
		return
	}
	s.recordFailure(ctx, job, runErr, duration)
}

// recordSuccess stores the merged object's result.
func (s *Scheduler) recordSuccess(ctx context.Context, job domain.Job, res merge.Result, duration time.Duration) {
	if _, err := s.store.Jobs().Succeed(ctx, job.ID, s.opts.Owner, store.SucceedPatch{
		SizeBytes:  res.SizeBytes,
		SHA256:     res.SHA256,
		Codec:      res.Codec,
		DurationMS: res.DurationMS,
		Frames:     res.Frames,
	}); err != nil {
		s.log.Error("scheduler: record job success", "job_id", job.ID, "duration", duration.String(), "error", err)
		return
	}
	s.log.Info("scheduler: job succeeded",
		"job_id", job.ID,
		"attempt", job.Attempts,
		"duration", duration.String(),
		"dest", res.DestBucket+"/"+res.DestKey,
		"size_bytes", res.SizeBytes,
		"codec", res.Codec,
		"output_duration_ms", res.DurationMS,
		"frames", res.Frames,
	)
}

// recordFailure requeues the job while its retry budget lasts and fails it
// terminally once the budget is spent. The requeue delay grows with the attempt
// number, so a source that is briefly unavailable is not hammered.
func (s *Scheduler) recordFailure(ctx context.Context, job domain.Job, runErr error, duration time.Duration) {
	attempts := job.Attempts
	maxAttempts := s.maxAttempts(job)
	var retryAt time.Time
	if attempts < maxAttempts {
		retryAt = time.Now().Add(s.opts.RetryBackoff * time.Duration(attempts))
	}
	if _, err := s.store.Jobs().Fail(ctx, job.ID, s.opts.Owner, runErr.Error(), retryAt); err != nil {
		s.log.Error("scheduler: record job failure", "job_id", job.ID, "attempt", attempts, "duration", duration.String(), "error", err)
		return
	}
	if retryAt.IsZero() {
		s.log.Error("scheduler: job failed",
			"job_id", job.ID,
			"attempts", attempts,
			"max_attempts", maxAttempts,
			"duration", duration.String(),
			"error", runErr,
		)
		return
	}
	s.log.Warn("scheduler: job requeued after failure",
		"job_id", job.ID,
		"attempt", attempts,
		"max_attempts", maxAttempts,
		"retry_in", time.Until(retryAt).String(),
		"duration", duration.String(),
		"error", runErr,
	)
}
