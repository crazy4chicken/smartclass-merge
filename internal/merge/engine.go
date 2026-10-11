// Package merge runs one merge job end to end: it resolves the codec of the
// recording, streams every segment through SplitFrames into the embedded ffmpeg
// and uploads the merged file to the destination bucket, reporting a full
// progress snapshot per update and verifying what was stored (contract §5.5).
package merge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crazy4chicken/smartclass-merge/internal/domain"
	"github.com/crazy4chicken/smartclass-merge/internal/ffmpeg"
	"github.com/crazy4chicken/smartclass-merge/internal/filehouse"
)

const (
	// sniffWindowBytes is how much of the first segment is read to detect the
	// codec: enough for several frames, whose concatenated parameter sets tell
	// H.264, H.265 and MPEG-4 apart (contract §5.4).
	sniffWindowBytes = 64 << 10
	// progressInterval throttles the progress callbacks of one merge, so the
	// callback rate stays independent of the ffmpeg progress rate and of the
	// read size. The scheduler throttles its database writes further.
	progressInterval = 500 * time.Millisecond
)

// Deps are the collaborators of one Engine.
type Deps struct {
	Filehouse *filehouse.Client
	FFmpeg    *ffmpeg.FFmpeg
	Logger    *slog.Logger
}

// Options are the engine settings of the process (MERGE_*), shared by every job.
type Options struct {
	// FPS is the frame rate applied when a job does not carry one
	// (MERGE_DEFAULT_FPS).
	FPS int
	// Codec forces one codec; empty means the codec is sniffed from the first
	// segment.
	Codec ffmpeg.Codec
	// TranscodeFallback retries a failed copy merge with a libx264 transcode
	// (MERGE_TRANSCODE_FALLBACK).
	TranscodeFallback bool
	// WorkDir is the root of the per-job directories (MERGE_WORK_DIR).
	WorkDir string
	// ObjectMaxBytes is the largest object a single PUT uploads
	// (MERGE_OBJECT_MAX_BYTES); a larger file uses a multipart upload.
	ObjectMaxBytes int64
	// PartBytes is the size of one multipart part
	// (MERGE_MULTIPART_PART_BYTES).
	PartBytes int64
}

// Engine merges one recording per Run. It holds no per-job state and is safe for
// concurrent use.
type Engine struct {
	filehouse *filehouse.Client
	ff        *ffmpeg.FFmpeg
	logger    *slog.Logger
	opts      Options
}

// New builds an engine from its collaborators and settings. A nil logger falls
// back to slog.Default().
func New(d Deps, o Options) *Engine {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Engine{
		filehouse: d.Filehouse,
		ff:        d.FFmpeg,
		logger:    d.Logger,
		opts:      o,
	}
}

// RunInput is one merge job's immutable input: the recording's segments in
// playback order and where the merged object belongs.
type RunInput struct {
	JobID        string
	SourceBucket string
	Segments     []domain.Segment
	DestBucket   string
	DestKey      string
	// FPS is the frame rate the recording was captured with; zero falls back to
	// Options.FPS.
	FPS int
	// Codec is the codec the caller knows; empty means "sniff it".
	Codec string
}

// Result is what one successful merge produced. Codec is the codec of the stored
// object: the recorded codec for a copy merge, h264 when the transcode fallback
// ran.
type Result struct {
	DestBucket string
	DestKey    string
	SizeBytes  int64
	SHA256     string
	DurationMS int64
	Frames     int64
	Codec      string
}

// Progress is one progress snapshot. Every field is cumulative for the job, and
// Stage names the phase that produced the snapshot.
type Progress struct {
	Stage            domain.Stage
	InputBytes       int64
	InputTotal       int64
	OutputBytes      int64
	UploadedBytes    int64
	OutputDurationMS int64
	Frames           int64
}

// ProgressFunc receives progress snapshots. Callbacks of one job never overlap,
// but they run on the goroutine that made the progress (ffmpeg's output reader
// or the upload transfer), so the callback must be safe to call from a
// non-caller goroutine and must not block for long: it backpressures the merge.
type ProgressFunc func(p Progress)

// Run merges in.Segments into one object at DestBucket/DestKey and returns what
// was stored. It reports progress while it works, and it always removes its
// per-job directory, whether or not the merge succeeded.
func (e *Engine) Run(ctx context.Context, in RunInput, report ProgressFunc) (Result, error) {
	if err := e.check(in); err != nil {
		return Result{}, err
	}
	fps := in.FPS
	if fps <= 0 {
		fps = e.opts.FPS
	}
	if fps <= 0 {
		return Result{}, errors.New("merge: the job carries no frame rate and no default is configured")
	}
	dir := filepath.Join(e.opts.WorkDir, "merge-"+in.JobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("merge: create job work dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			e.logger.Warn("remove job work dir failed", "job_id", in.JobID, "dir", dir, "error", err)
		}
	}()

	rep := &reporter{fn: report}
	rep.set(true, func(p *Progress) {
		p.Stage = domain.StageMerging
		p.InputTotal = declaredInput(in.Segments)
	})

	codec, err := e.resolveCodec(ctx, in)
	if err != nil {
		return Result{}, err
	}
	transcoded := false
	merged, outPath, err := e.merge(ctx, in, codec, fps, false, dir, rep)
	if err != nil && e.opts.TranscodeFallback {
		var failed *ffmpegFailure
		if errors.As(err, &failed) {
			e.logger.Warn("copy merge failed, retrying with a transcode",
				"job_id", in.JobID,
				"codec", string(codec),
				"error", err,
			)
			transcoded = true
			merged, outPath, err = e.merge(ctx, in, codec, fps, true, dir, rep)
		}
	}
	if err != nil {
		return Result{}, fmt.Errorf("merge %s: %w", in.JobID, err)
	}
	rep.set(true, func(p *Progress) {
		p.Stage = domain.StageUploading
		p.OutputBytes = merged.OutputBytes
		p.OutputDurationMS = merged.DurationMS
		p.Frames = merged.Frames
	})
	stored, err := e.upload(ctx, in, outPath, merged.OutputBytes, rep)
	if err != nil {
		return Result{}, fmt.Errorf("merge %s: %w", in.JobID, err)
	}
	rep.set(true, func(p *Progress) { p.UploadedBytes = merged.OutputBytes })

	outCodec := codec
	if transcoded {
		outCodec = ffmpeg.CodecH264
	}
	return Result{
		DestBucket: in.DestBucket,
		DestKey:    in.DestKey,
		SizeBytes:  stored.SizeBytes,
		SHA256:     storedDigest(stored),
		DurationMS: merged.DurationMS,
		Frames:     merged.Frames,
		Codec:      string(outCodec),
	}, nil
}

// check rejects an input the engine cannot act on before any work starts.
func (e *Engine) check(in RunInput) error {
	switch {
	case e.filehouse == nil:
		return errors.New("merge: no filehouse client is configured")
	case e.ff == nil:
		return errors.New("merge: no ffmpeg is configured")
	case e.opts.WorkDir == "":
		return errors.New("merge: no work dir is configured (MERGE_WORK_DIR)")
	case in.JobID == "" || strings.ContainsAny(in.JobID, `/\`) || strings.Contains(in.JobID, ".."):
		return fmt.Errorf("merge: unusable job id %q", in.JobID)
	case in.SourceBucket == "":
		return errors.New("merge: the source bucket is required")
	case in.DestBucket == "" || in.DestKey == "":
		return errors.New("merge: the destination bucket and key are required")
	case len(in.Segments) == 0:
		return errors.New("merge: the job has no segments to merge")
	}
	for _, segment := range in.Segments {
		if segment.Key == "" {
			return errors.New("merge: a segment without a key cannot be downloaded")
		}
	}
	return nil
}

// resolveCodec returns the codec of the recording: the codec the job names when
// it names a supported one, otherwise the codec sniffed from the frames of the
// first segment.
func (e *Engine) resolveCodec(ctx context.Context, in RunInput) (ffmpeg.Codec, error) {
	codec := ffmpeg.Codec(strings.ToLower(strings.TrimSpace(in.Codec)))
	if codec == "" {
		codec = e.opts.Codec
	}
	if codec != "" {
		if _, ok := ffmpeg.FormatFor(codec, false); !ok {
			return "", unsupportedCodec(codec)
		}
		return codec, nil
	}
	// Nothing names the codec, so read the head of the first segment and look at
	// the bitstream. The frames of the first segment are read twice in this
	// case: once for this window and once by the merge, which keeps the merge
	// loop free of a special case for the first segment.
	body, err := e.filehouse.GetObject(ctx, in.SourceBucket, in.Segments[0].Key)
	if err != nil {
		return "", fmt.Errorf("read first segment %s: %w", in.Segments[0].Key, err)
	}
	defer body.Close()
	frames, err := collectFrames(body, sniffWindowBytes)
	if err != nil {
		return "", fmt.Errorf("read first segment %s: %w", in.Segments[0].Key, err)
	}
	sniffed, ok := ffmpeg.Sniff(frames)
	if !ok {
		return "", fmt.Errorf("merge: cannot detect the codec of %s from the first segment; pass the codec explicitly", in.JobID)
	}
	e.logger.Debug("sniffed recording codec", "job_id", in.JobID, "codec", string(sniffed))
	return sniffed, nil
}

// ffmpegFailure marks a failure of the ffmpeg pipeline itself, as opposed to a
// segment download or a framing failure. Only such a failure is retried with the
// transcode fallback: re-downloading a recording that failed to copy would
// change nothing.
type ffmpegFailure struct{ err error }

// Error renders the underlying ffmpeg failure.
func (f *ffmpegFailure) Error() string { return f.err.Error() }

// Unwrap exposes the underlying ffmpeg failure.
func (f *ffmpegFailure) Unwrap() error { return f.err }

// merge streams every segment through ffmpeg into dir and returns ffmpeg's
// result together with the path of the merged file.
func (e *Engine) merge(ctx context.Context, in RunInput, codec ffmpeg.Codec, fps int, transcode bool, dir string, rep *reporter) (ffmpeg.Result, string, error) {
	format, ok := ffmpeg.FormatFor(codec, transcode)
	if !ok {
		return ffmpeg.Result{}, "", unsupportedCodec(codec)
	}
	out := filepath.Join(dir, "out"+format.Extension)

	// ffmpeg reads the frames from a pipe the feed goroutine writes, so a
	// segment is downloaded only while it is consumed, and a failing download
	// stops the pipeline by breaking the pipe.
	reader, writer := io.Pipe()
	feedErr := make(chan error, 1)
	go func() {
		err := e.feed(ctx, in, writer, rep)
		if err != nil {
			_ = writer.CloseWithError(err)
		} else {
			_ = writer.Close()
		}
		feedErr <- err
	}()

	options := ffmpeg.MergeOptions{
		Demuxer:   format.Demuxer,
		FPS:       fps,
		Container: format.Container,
		Transcode: transcode,
		Out:       out,
	}
	result, err := e.ff.Run(ctx, reader, options, func(p ffmpeg.Progress) {
		rep.set(false, func(s *Progress) {
			s.OutputBytes = p.TotalSize
			s.OutputDurationMS = p.OutTimeMS
			s.Frames = p.Frame
		})
	})
	// Run returned, so ffmpeg is gone: unblock the feed goroutine if it is
	// still writing, then collect its outcome.
	_ = reader.CloseWithError(err)
	downloaded := <-feedErr
	if downloaded != nil && !errors.Is(downloaded, err) {
		// The download or the framing failed; ffmpeg only saw a broken pipe.
		return ffmpeg.Result{}, "", downloaded
	}
	if err != nil {
		return ffmpeg.Result{}, "", &ffmpegFailure{err: err}
	}
	return result, out, nil
}

// feed streams every segment of the recording, in order, into the merge
// pipeline.
func (e *Engine) feed(ctx context.Context, in RunInput, dst io.Writer, rep *reporter) error {
	var fed int64
	for _, segment := range in.Segments {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, err := e.feedSegment(ctx, in.SourceBucket, segment, fed, dst, rep)
		if err != nil {
			return err
		}
		fed += read
	}
	return nil
}

// feedSegment downloads one segment and copies its frames, without the framing,
// into the pipeline. inputBase is the number of bytes already fed, so the
// reported input progress is cumulative for the job.
func (e *Engine) feedSegment(ctx context.Context, bucket string, segment domain.Segment, inputBase int64, dst io.Writer, rep *reporter) (int64, error) {
	body, err := e.filehouse.GetObject(ctx, bucket, segment.Key)
	if err != nil {
		return 0, fmt.Errorf("download segment %s: %w", segment.Key, err)
	}
	defer body.Close()
	source := &countingReader{
		r: body,
		onRead: func(read int64) {
			rep.set(false, func(p *Progress) { p.InputBytes = inputBase + read })
		},
	}
	if _, _, err := SplitFrames(dst, source); err != nil {
		return source.read, fmt.Errorf("segment %s: %w", segment.Key, err)
	}
	return source.read, nil
}

// declaredInput sums the sizes the job declared for its segments. A segment
// without a size contributes nothing: progress_input_total is a hint for the
// caller, not a promise the engine relies on.
func declaredInput(segments []domain.Segment) int64 {
	var total int64
	for _, segment := range segments {
		if segment.SizeBytes > 0 {
			total += segment.SizeBytes
		}
	}
	return total
}

// unsupportedCodec reports a codec this service cannot merge and names the ones
// it can.
func unsupportedCodec(codec ffmpeg.Codec) error {
	supported := ffmpeg.SupportedCodecs()
	names := make([]string, 0, len(supported))
	for _, name := range supported {
		names = append(names, string(name))
	}
	return fmt.Errorf("merge: unsupported codec %q; supported codecs are %s (vp8 and vp9 have no raw elementary-stream demuxer in ffmpeg)", string(codec), strings.Join(names, ", "))
}

// reporter keeps the running progress snapshot of one job and hands a copy to
// the caller's callback. set is safe for concurrent use — the merge reports from
// ffmpeg's output goroutine and from the segment download at the same time — and
// the callback runs under the same lock, so callbacks never overlap.
type reporter struct {
	mu   sync.Mutex
	fn   ProgressFunc
	p    Progress
	last time.Time
}

// set applies mutate to the snapshot and, unless the throttle window is still
// open or force is set, passes a copy to the callback.
func (r *reporter) set(force bool, mutate func(*Progress)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mutate(&r.p)
	if r.fn == nil {
		return
	}
	now := time.Now()
	if !force && now.Sub(r.last) < progressInterval {
		return
	}
	r.last = now
	r.fn(r.p)
}
