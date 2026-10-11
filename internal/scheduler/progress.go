package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/crazy4chicken/smartclass-merge/internal/domain"
	"github.com/crazy4chicken/smartclass-merge/internal/merge"
)

// reporter bridges the engine's in-process progress callbacks to the job store.
// The callback sits on the engine's hot path, so it only updates an in-memory
// snapshot; one supervisor goroutine owns every database write. Each heartbeat
// polls the stored status — so a cancel issued by another replica aborts this
// run — and writes the latest snapshot through Progress where there is
// something new to say. A silent engine, such as a stalled download, still gets
// its lease refreshed once a third of the lease has passed, so the claim can
// never expire under a merge that is still alive.
type reporter struct {
	s      *Scheduler
	jobID  string
	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	snap         domain.Progress
	stage        domain.Stage
	dirty        bool          // a fresh engine sample waits to be written
	writtenStage domain.Stage  // the stage the store already holds
	writtenAt    time.Time     // when the last write landed
	refresh      time.Duration // silent-stretch limit before the lease is renewed

	quit chan struct{} // closed to end the heartbeat
	done chan struct{} // closed once the heartbeat has returned
	once sync.Once     // makes stop idempotent
}

// newReporter builds the reporter of one attempt. The snapshot starts from the
// progress stored on the row, so the heartbeat never reports numbers the store
// does not already hold. The zero write state means the first heartbeat always
// writes: it repairs a mark-stage write that failed and picks up wherever the
// engine got to.
func newReporter(s *Scheduler, ctx context.Context, cancel context.CancelFunc, job domain.Job) *reporter {
	refresh := s.opts.Lease / 3
	if refresh < progressInterval {
		refresh = progressInterval
	}
	return &reporter{
		s:       s,
		jobID:   job.ID,
		ctx:     ctx,
		cancel:  cancel,
		snap:    job.Progress,
		stage:   domain.StageMerging,
		refresh: refresh,
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// report stores one engine progress sample. A stage change is logged at the
// moment the engine reports it; every other sample stays silent until the
// supervisor writes it.
func (r *reporter) report(p merge.Progress) {
	r.mu.Lock()
	stageChanged := p.Stage != "" && p.Stage != r.stage
	if stageChanged {
		r.stage = p.Stage
	}
	r.snap = domain.Progress{
		InputBytes:       p.InputBytes,
		InputTotal:       p.InputTotal,
		OutputBytes:      p.OutputBytes,
		UploadedBytes:    p.UploadedBytes,
		OutputDurationMS: p.OutputDurationMS,
		Frames:           p.Frames,
	}
	r.dirty = true
	r.mu.Unlock()
	if stageChanged {
		r.s.log.Info("scheduler: job stage changed", "job_id", r.jobID, "stage", string(p.Stage))
	}
}

// supervise writes the heartbeat until the run ends or the job context is
// done.
func (r *reporter) supervise() {
	defer close(r.done)
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.quit:
			return
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.tick()
		}
	}
}

// stop ends the heartbeat and flushes the final snapshot, so the terminal
// outcome is recorded on top of a store state that already reflects the whole
// run: the ticker can be up to one interval behind the engine, and without this
// write the row would keep an early snapshot — missing the input counters and
// the final output numbers — next to its terminal result.
func (r *reporter) stop() {
	r.once.Do(func() { close(r.quit) })
	<-r.done
	r.flush()
}

// flush writes the latest snapshot once, ignoring the heartbeat interval, and
// only while the job context is still live: a canceled or timed-out run must
// not write progress over a row another owner may already hold.
func (r *reporter) flush() {
	if r.ctx.Err() != nil {
		return
	}
	snap, stage, owed := r.pending()
	if !owed {
		return
	}
	ok, err := r.s.store.Jobs().Progress(r.ctx, r.jobID, r.s.opts.Owner, snap, stage)
	switch {
	case err != nil:
		r.s.log.Warn("scheduler: write final job progress", "job_id", r.jobID, "stage", string(stage), "error", err)
	case !ok:
		r.s.log.Warn("scheduler: job lease lost before the final progress write", "job_id", r.jobID, "owner", r.s.opts.Owner)
	default:
		r.markWritten(stage)
	}
}

// tick runs one heartbeat: poll the stored status first — that poll is the
// cancel detector and never throttled — then write the snapshot when the store
// is owed one.
func (r *reporter) tick() {
	status, err := r.s.store.Jobs().Status(r.ctx, r.jobID)
	switch {
	case err != nil:
		r.s.log.Warn("scheduler: poll job status", "job_id", r.jobID, "error", err)
	case status == domain.StatusCanceled:
		r.s.log.Info("scheduler: job canceled out of process, aborting run", "job_id", r.jobID)
		r.cancel()
		return
	}

	snap, stage, owed := r.pending()
	if !owed {
		return
	}
	ok, err := r.s.store.Jobs().Progress(r.ctx, r.jobID, r.s.opts.Owner, snap, stage)
	switch {
	case err != nil:
		// Keep the sample marked for the next beat: a transient store error
		// must not stall progress until the lease refresh comes around.
		r.markDirty()
		r.s.log.Warn("scheduler: write job progress", "job_id", r.jobID, "stage", string(stage), "error", err)
	case !ok:
		// The row is no longer running under our lease: it was canceled or a
		// peer reclaimed it after the lease expired. Another owner is
		// responsible for its outcome, so this run stops before writing one.
		r.s.log.Warn("scheduler: job lease lost, aborting run", "job_id", r.jobID, "owner", r.s.opts.Owner)
		r.cancel()
	default:
		r.markWritten(stage)
	}
}

// pending reports whether the store is owed a write — a fresh engine sample, a
// stage it has not seen yet, or a lease refresh after a silent stretch long
// enough for the claim to be at risk — and returns the snapshot to write. The
// sample is marked as taken, so a sample the engine reports afterwards is never
// mistaken for one already on its way to the store.
func (r *reporter) pending() (domain.Progress, domain.Stage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.dirty && r.stage == r.writtenStage && time.Since(r.writtenAt) < r.refresh {
		return r.snap, r.stage, false
	}
	r.dirty = false
	return r.snap, r.stage, true
}

// markDirty queues the current snapshot for the next heartbeat again.
func (r *reporter) markDirty() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dirty = true
}

// markWritten records a landed write, so the next heartbeat repeats it only
// when it has something new to say or the lease needs refreshing.
func (r *reporter) markWritten(stage domain.Stage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writtenStage = stage
	r.writtenAt = time.Now()
}
