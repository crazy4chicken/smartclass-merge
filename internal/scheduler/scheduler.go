// Package scheduler runs the merge queue. It owns the tick loop that claims
// queued jobs, the concurrency bound around the merge engine, the progress and
// lease heartbeat every running attempt writes to the store, the detection of
// cancels issued outside this process and the terminal outcome of each
// attempt.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/crazy4chicken/smartclass-merge/internal/domain"
	"github.com/crazy4chicken/smartclass-merge/internal/merge"
	"github.com/crazy4chicken/smartclass-merge/internal/store"
)

// Options tunes the tick loop. Every zero field falls back to a safe default,
// so New can never build a scheduler that spins on a zero ticker or runs an
// unbounded number of merges.
type Options struct {
	Tick          time.Duration // cadence of the claim pass
	Lease         time.Duration // how long a claim stays valid without a progress write
	JobTimeout    time.Duration // wall-clock budget of one attempt
	MaxAttempts   int           // retry budget for rows that carry none
	MaxConcurrent int           // merges this process runs at once
	RetryBackoff  time.Duration // base of the per-attempt retry delay
	Owner         string        // lease owner recorded by Claim; identifies this process
}

// Scheduler claims jobs, runs them and records their outcome.
type Scheduler struct {
	opts  Options
	store *store.Store
	eng   *merge.Engine
	log   *slog.Logger

	slots chan struct{} // bounds the concurrent merges to MaxConcurrent

	mu      sync.Mutex
	running map[string]context.CancelFunc // in-flight merges by job id, for Cancel
	wg      sync.WaitGroup                // the in-flight merge goroutines
}

// Pacing, drain and fallback constants.
const (
	progressInterval     = 2 * time.Second  // minimum gap between progress writes to the store
	terminalWriteTimeout = 30 * time.Second // budget of one terminal Succeed or Fail write
	drainGrace           = 30 * time.Second // time in-flight merges get to settle after cancellation

	defaultTick          = 5 * time.Second
	defaultLease         = 2 * time.Minute
	defaultJobTimeout    = 6 * time.Hour
	defaultRetryBackoff  = time.Minute
	defaultMaxAttempts   = 3
	defaultMaxConcurrent = 1
)

// New builds a Scheduler. It never reaches the network; the store and the
// engine must already be constructed.
func New(o Options, st *store.Store, eng *merge.Engine, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	if o.Tick <= 0 {
		o.Tick = defaultTick
	}
	if o.Lease <= 0 {
		o.Lease = defaultLease
	}
	if o.JobTimeout <= 0 {
		o.JobTimeout = defaultJobTimeout
	}
	if o.MaxAttempts < 1 {
		o.MaxAttempts = defaultMaxAttempts
	}
	if o.MaxConcurrent < 1 {
		o.MaxConcurrent = defaultMaxConcurrent
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = defaultRetryBackoff
	}
	if o.Owner == "" {
		o.Owner = fallbackOwner()
	}
	return &Scheduler{
		opts:    o,
		store:   st,
		eng:     eng,
		log:     log,
		slots:   make(chan struct{}, o.MaxConcurrent),
		running: make(map[string]context.CancelFunc),
	}
}

// fallbackOwner names this process when the caller supplies no owner. A lease
// owner must be unique across replicas and stable for the process lifetime,
// which host plus pid give.
func fallbackOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "merge"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// Run blocks until ctx is cancelled. It claims immediately, then on every tick;
// on cancellation it stops claiming and drains the in-flight merges.
func (s *Scheduler) Run(ctx context.Context) {
	s.log.Info("scheduler: started",
		"owner", s.opts.Owner,
		"tick", s.opts.Tick.String(),
		"lease", s.opts.Lease.String(),
		"job_timeout", s.opts.JobTimeout.String(),
		"max_concurrent_jobs", s.opts.MaxConcurrent,
		"max_attempts", s.opts.MaxAttempts,
		"retry_backoff", s.opts.RetryBackoff.String(),
	)
	s.tick(ctx)
	ticker := time.NewTicker(s.opts.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.drain()
			s.log.Info("scheduler: stopped")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick claims one batch of due jobs — never more than the free capacity — and
// starts every claimed job.
func (s *Scheduler) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	free := s.freeSlots()
	if free <= 0 {
		return
	}
	jobs, err := s.store.Jobs().Claim(ctx, s.opts.Owner, free, s.opts.Lease, time.Now())
	if err != nil {
		s.log.Error("scheduler: claim jobs", "owner", s.opts.Owner, "limit", free, "error", err)
		return
	}
	for _, job := range jobs {
		s.start(ctx, job)
	}
}

// Cancel aborts an in-flight merge started by this process. It reports false
// when the job is not running here: the caller then relies on the stored
// status, which its own cancel transition already wrote — the lease guard of
// Succeed and Fail keeps that status safe either way.
func (s *Scheduler) Cancel(jobID string) bool {
	s.mu.Lock()
	cancel, ok := s.running[jobID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	s.log.Info("scheduler: canceling job", "job_id", jobID)
	cancel()
	return true
}

// start registers one claimed job as in-flight and runs it in its own
// goroutine. The job context keeps the caller's values but owns its
// cancellation: Cancel(jobID) aborts the merge at once and the drain grants it
// a grace period before Run cancels it.
func (s *Scheduler) start(ctx context.Context, job domain.Job) {
	if ctx.Err() != nil {
		// Claimed while shutting down: the row keeps our lease and is
		// reclaimed once it expires, the same recovery a crashed process
		// relies on.
		s.log.Warn("scheduler: job claimed while shutting down", "job_id", job.ID)
		return
	}
	// The batch was sized to the free capacity, so a slot is always there; the
	// select keeps a pathological race from ever blocking the tick loop.
	select {
	case s.slots <- struct{}{}:
	default:
		s.log.Warn("scheduler: no free merge slot", "job_id", job.ID)
		return
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.JobTimeout)
	s.mu.Lock()
	s.running[job.ID] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.slots }()
		defer s.forget(job.ID)
		defer cancel()
		s.runJob(runCtx, cancel, job)
	}()
}

// forget drops a settled job from the in-flight table, after which Cancel
// reports false for it.
func (s *Scheduler) forget(jobID string) {
	s.mu.Lock()
	delete(s.running, jobID)
	s.mu.Unlock()
}

// drain waits for the in-flight merges to settle and then cancels the ones
// still running. Their rows are left untouched on purpose: an attempt aborted
// by a restart is not a job failure, so it must not spend a retry. The lease
// expires and the next claim — this process or a peer — picks the job up again.
func (s *Scheduler) drain() {
	inflight := s.inFlight()
	if inflight == 0 {
		return
	}
	s.log.Info("scheduler: draining in-flight jobs", "jobs", inflight, "grace", drainGrace.String())
	settled := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(settled)
	}()
	timer := time.NewTimer(drainGrace)
	defer timer.Stop()
	select {
	case <-settled:
		s.log.Info("scheduler: in-flight jobs settled")
		return
	case <-timer.C:
	}
	cancels := s.cancelers()
	s.log.Warn("scheduler: drain grace elapsed, canceling jobs", "jobs", len(cancels))
	for _, cancel := range cancels {
		cancel()
	}
}

// freeSlots reports how many merges this process may still start.
func (s *Scheduler) freeSlots() int { return cap(s.slots) - len(s.slots) }

// inFlight reports how many merges this process is running.
func (s *Scheduler) inFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}

// cancelers snapshots the cancel functions of the in-flight merges.
func (s *Scheduler) cancelers() []context.CancelFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	cancels := make([]context.CancelFunc, 0, len(s.running))
	for _, cancel := range s.running {
		cancels = append(cancels, cancel)
	}
	return cancels
}

// maxAttempts is a job's retry budget. The row stores its own; the scheduler
// default only covers rows that carry none.
func (s *Scheduler) maxAttempts(job domain.Job) int {
	if job.MaxAttempts > 0 {
		return job.MaxAttempts
	}
	return s.opts.MaxAttempts
}
