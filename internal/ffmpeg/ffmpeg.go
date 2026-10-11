// Package ffmpeg runs the embedded FFmpeg build that turns a recording's raw,
// length-prefixed frames into one video file. The binary is embedded per
// platform (contract §6) and extracted on first use; Path, Version and Demuxers
// report what is available, Run executes the merge pipeline, and Sniff and
// FormatFor map a recorded codec onto the ffmpeg demuxer, container and
// extension the merge engine needs.
package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// extractDirName is the subdirectory of the work dir that holds the
	// extracted binary: <WorkDir>/ffmpeg/<name>.
	extractDirName = "ffmpeg"
	// execWaitDelay bounds how long a killed ffmpeg's I/O pipes are waited for,
	// so a stuck stdin or a blocking progress callback cannot hang a merge
	// forever after its context ended.
	execWaitDelay = 10 * time.Second
	// probeTimeout bounds Version and Demuxers: their output is tiny, but a
	// broken binary must not block the doctor subcommand indefinitely.
	probeTimeout = 30 * time.Second
)

// Options configure one FFmpeg instance.
type Options struct {
	// BinaryPath is an ffmpeg executable used instead of the embedded build
	// (MERGE_FFMPEG_PATH). It is executed as-is, without extraction.
	BinaryPath string
	// WorkDir is the root of the extraction directory <WorkDir>/ffmpeg
	// (MERGE_WORK_DIR). It is required unless BinaryPath is set.
	WorkDir string
	// Logger receives the ffmpeg diagnostics; a nil logger falls back to
	// slog.Default().
	Logger *slog.Logger
}

// FFmpeg executes one ffmpeg build. It is safe for concurrent use.
type FFmpeg struct {
	opts Options

	mu   sync.Mutex
	path string
}

// errNoEmbedded reports a build without an ffmpeg payload for its platform: the
// platform is not one of the three this package embeds, or the payload was not
// fetched before the build.
var errNoEmbedded = errors.New("ffmpeg: no embedded build for this platform; run `go run ./cmd/fetchffmpeg` for it or set MERGE_FFMPEG_PATH to an existing ffmpeg")

// embeddedBinary returns the payload embedded for this platform, or nil when
// this build carries none. The lookup goes through the embedded directory rather
// than a fixed embed pattern, so a checkout whose payload was not fetched still
// compiles: the missing build is reported by New and Path, where an operator can
// act on the message, instead of failing the whole build.
func embeddedBinary() []byte {
	if platformBinaryName == "" {
		return nil
	}
	payload, err := binDir.ReadFile("bin/" + platformBinaryName)
	if err != nil {
		return nil
	}
	return payload
}

// New validates the options. The executable itself is resolved on first use, so
// a process that never merges never extracts the embedded build. A platform
// without an embedded payload is rejected here, because nothing could ever
// resolve it.
func New(o Options) (*FFmpeg, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.BinaryPath == "" {
		if o.WorkDir == "" {
			return nil, errors.New("ffmpeg: a work dir is required when no binary path is configured")
		}
		if embeddedBinary() == nil {
			return nil, errNoEmbedded
		}
	}
	return &FFmpeg{opts: o}, nil
}

// Path returns the ffmpeg executable to run: BinaryPath when configured,
// otherwise the embedded build extracted to <WorkDir>/ffmpeg/<name>. The result
// is cached, so the extraction happens at most once per process.
func (f *FFmpeg) Path(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.path != "" {
		return f.path, nil
	}
	if f.opts.BinaryPath != "" {
		f.path = f.opts.BinaryPath
		return f.path, nil
	}
	payload := embeddedBinary()
	if payload == nil {
		return "", errNoEmbedded
	}
	path, err := extract(ctx, f.opts.WorkDir, platformBinaryName, payload)
	if err != nil {
		return "", err
	}
	f.path = path
	return path, nil
}

// Version returns the version token of the resolved build, for example "7.1" or
// "N-117637-g4f2cf3c3a8": the third field of ffmpeg's `-version` banner.
func (f *FFmpeg) Version(ctx context.Context) (string, error) {
	path, err := f.Path(ctx)
	if err != nil {
		return "", err
	}
	out, err := f.output(ctx, path, "-version")
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(string(out), "\n")
	if fields := strings.Fields(first); len(fields) >= 3 && fields[0] == "ffmpeg" && fields[1] == "version" {
		return fields[2], nil
	}
	if banner := strings.TrimSpace(first); banner != "" {
		return banner, nil
	}
	return "", fmt.Errorf("ffmpeg: %s -version answered with no output", path)
}

// Demuxers reports the demuxers the resolved build provides, keyed by demuxer
// name. The doctor subcommand uses it to check that the embedded build can read
// every codec this service accepts.
func (f *FFmpeg) Demuxers(ctx context.Context) (map[string]bool, error) {
	path, err := f.Path(ctx)
	if err != nil {
		return nil, err
	}
	out, err := f.output(ctx, path, "-demuxers")
	if err != nil {
		return nil, err
	}
	demuxers := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		// Every listing row is " <flag>  <name>  <description>"; the header row
		// has a single field and no flag column.
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[0]) != 1 {
			continue
		}
		demuxers[fields[1]] = true
	}
	return demuxers, nil
}

// extract installs payload as <workDir>/ffmpeg/<name> with mode 0755, through a
// temporary file and a rename, and returns its path. A file of the expected size
// is reused: the payload cannot change for a given build, and on Windows a
// running ffmpeg cannot be replaced.
func extract(ctx context.Context, workDir, name string, payload []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir := filepath.Join(workDir, extractDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("ffmpeg: create extraction dir: %w", err)
	}
	target := filepath.Join(dir, name)
	if info, err := os.Stat(target); err == nil && info.Size() == int64(len(payload)) {
		return target, nil
	}
	tmp, err := os.CreateTemp(dir, name+".tmp-")
	if err != nil {
		return "", fmt.Errorf("ffmpeg: create extraction temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("ffmpeg: write embedded binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("ffmpeg: write embedded binary: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("ffmpeg: mark embedded binary executable: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		// Windows cannot rename over an existing file, so a stale build of a
		// different size is removed first.
		if removeErr := os.Remove(target); removeErr != nil {
			_ = os.Remove(tmpName)
			return "", fmt.Errorf("ffmpeg: install embedded binary: %w", err)
		}
		if err := os.Rename(tmpName, target); err != nil {
			_ = os.Remove(tmpName)
			return "", fmt.Errorf("ffmpeg: install embedded binary: %w", err)
		}
	}
	return target, nil
}

// output runs a short ffmpeg probe and returns its standard output; standard
// error is kept so a failing probe can report why.
func (f *FFmpeg) output(ctx context.Context, path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = execWaitDelay
	var stdout bytes.Buffer
	var stderr lineWriter
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %s failed: %w%s", args[0], err, stderr.tailSuffix())
	}
	return stdout.Bytes(), nil
}
