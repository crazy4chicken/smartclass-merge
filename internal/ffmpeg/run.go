package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// stderrTailLines is how many ffmpeg standard-error lines an error message
// carries; ffmpeg prints the real cause within a few lines of the end.
const stderrTailLines = 20

// MergeOptions describe one merge pipeline (contract §5.4).
type MergeOptions struct {
	// Demuxer is the raw input demuxer the frames are read with ("h264",
	// "hevc", "mjpeg", "m4v" or "av1").
	Demuxer string
	// FPS is the frame rate of the recorded stream. A raw bitstream carries no
	// timing, so the caller supplies it (the request's fps or
	// MERGE_DEFAULT_FPS).
	FPS int
	// Container is the output format name ("mp4" or "mov").
	Container string
	// Transcode re-encodes the video with libx264 instead of copying it.
	Transcode bool
	// Out is the file ffmpeg writes; the caller owns it.
	Out string
}

// Progress is one ffmpeg progress block, published about twice per second.
type Progress struct {
	// Frame is the number of frames written so far.
	Frame int64
	// OutTimeMS is the duration written so far, in milliseconds.
	OutTimeMS int64
	// TotalSize is the number of bytes written so far.
	TotalSize int64
}

// Result is what one successful merge produced.
type Result struct {
	DurationMS  int64
	Frames      int64
	OutputBytes int64
}

// Run merges the raw frames read from in into o.Out and reports ffmpeg's
// progress to progress (which may be nil). The caller owns in and must keep it
// open until Run returns; ffmpeg reads it as pipe:0. A failing merge leaves a
// partial output file behind, which the engine discards with its job directory.
func (f *FFmpeg) Run(ctx context.Context, in io.Reader, o MergeOptions, progress func(Progress)) (Result, error) {
	switch {
	case in == nil:
		return Result{}, errors.New("ffmpeg: an input reader is required")
	case o.Out == "":
		return Result{}, errors.New("ffmpeg: an output path is required")
	case o.Demuxer == "":
		return Result{}, errors.New("ffmpeg: an input demuxer is required")
	case o.Container == "":
		return Result{}, errors.New("ffmpeg: an output container is required")
	case o.FPS <= 0:
		return Result{}, fmt.Errorf("ffmpeg: a positive frame rate is required, got %d", o.FPS)
	}
	path, err := f.Path(ctx)
	if err != nil {
		return Result{}, err
	}
	stdout := &progressWriter{report: progress}
	var stderr lineWriter
	cmd := exec.CommandContext(ctx, path, o.args()...)
	cmd.Stdin = in
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = execWaitDelay
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("ffmpeg: merging into %s failed: %w%s", filepath.Base(o.Out), err, stderr.tailSuffix())
	}
	// cmd.Run waits for the copying goroutines of both writers, so the last
	// progress block is complete before it is read here.
	info, err := os.Stat(o.Out)
	if err != nil {
		return Result{}, fmt.Errorf("ffmpeg: merged output %s: %w", o.Out, err)
	}
	if info.Size() == 0 {
		return Result{}, fmt.Errorf("ffmpeg: merging into %s produced an empty file%s", filepath.Base(o.Out), stderr.tailSuffix())
	}
	result := Result{DurationMS: stdout.last.OutTimeMS, Frames: stdout.last.Frame, OutputBytes: info.Size()}
	if result.DurationMS <= 0 && result.Frames > 0 {
		// A stream-copy remux decodes nothing, so ffmpeg reports
		// out_time_us/out_time_ms=N/A in its final progress block. The pipeline
		// feeds a raw elementary stream at a constant rate, so the frame count
		// and that rate give the exact duration.
		result.DurationMS = result.Frames * 1000 / int64(o.FPS)
	}
	f.opts.Logger.Debug("ffmpeg merge finished",
		"path", filepath.Base(o.Out),
		"frames", result.Frames,
		"duration_ms", result.DurationMS,
		"bytes", result.OutputBytes,
		"transcode", o.Transcode,
	)
	return result, nil
}

// args renders the ffmpeg command line of one merge (contract §5.4).
func (o MergeOptions) args() []string {
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "warning",
		"-progress", "pipe:1", "-nostats",
		"-f", o.Demuxer, "-r", strconv.Itoa(o.FPS), "-i", "pipe:0",
	}
	if o.Transcode {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p")
	} else {
		args = append(args, "-c:v", "copy")
	}
	if o.Container == "mp4" || o.Container == "mov" {
		// faststart moves the moov atom in front of the media data, so a player
		// can start from the first bytes of the uploaded object.
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, "-f", o.Container, "-y", o.Out)
	return args
}

// progressWriter parses the `key=value` lines of `-progress pipe:1` and invokes
// the callback once per completed block, which ffmpeg terminates with a
// `progress=` line. Output arrives in arbitrary chunks, so a partial line is
// buffered until its newline arrives. The callback runs on the process's output
// goroutine, never concurrently.
type progressWriter struct {
	report func(Progress)
	last   Progress
	buf    []byte
}

// Write consumes a chunk of ffmpeg's standard output.
func (w *progressWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
}

// line applies one `key=value` line to the current block. Unknown keys are
// ignored, and so are the `N/A` values ffmpeg publishes before it knows a
// number (for example total_size while the muxer header is still open).
func (w *progressWriter) line(raw string) {
	key, value, ok := strings.Cut(strings.TrimRight(raw, "\r"), "=")
	if !ok {
		return
	}
	switch key {
	case "frame":
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			w.last.Frame = n
		}
	case "out_time_us", "out_time_ms":
		// ffmpeg publishes both keys in microseconds despite the name of the
		// second one; Progress.OutTimeMS is milliseconds.
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			w.last.OutTimeMS = n / 1000
		}
	case "total_size":
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			w.last.TotalSize = n
		}
	case "progress":
		if w.report != nil {
			w.report(w.last)
		}
	}
}

// lineRing keeps the last stderrTailLines lines written through lineWriter.
type lineRing struct {
	lines []string
	next  int
}

// add appends one line, dropping the oldest once the ring is full.
func (r *lineRing) add(line string) {
	if len(r.lines) < stderrTailLines {
		r.lines = append(r.lines, line)
		return
	}
	r.lines[r.next] = line
	r.next = (r.next + 1) % stderrTailLines
}

// tail renders the buffered lines oldest first, or "" when nothing was written.
func (r *lineRing) tail() string {
	switch {
	case len(r.lines) == 0:
		return ""
	case len(r.lines) < stderrTailLines:
		return strings.Join(r.lines, "\n")
	}
	ordered := make([]string, 0, stderrTailLines)
	ordered = append(ordered, r.lines[r.next:]...)
	ordered = append(ordered, r.lines[:r.next]...)
	return strings.Join(ordered, "\n")
}

// lineWriter feeds the complete lines of one command's output into a ring; a
// partial line is kept until its newline arrives. It never fails a write: the
// process must not be stopped by diagnostics.
type lineWriter struct {
	ring    lineRing
	partial []byte
}

// Write consumes a chunk of the command's output.
func (w *lineWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.ring.add(strings.TrimRight(string(w.partial[:i]), "\r"))
		w.partial = w.partial[i+1:]
	}
}

// tailSuffix renders the captured stderr tail for an error message, or "" when
// the command wrote nothing.
func (w *lineWriter) tailSuffix() string {
	tail := w.ring.tail()
	if tail == "" {
		return ""
	}
	return ", ffmpeg stderr:\n" + tail
}
