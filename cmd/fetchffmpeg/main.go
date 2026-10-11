// Command fetchffmpeg downloads the FFmpeg build that internal/ffmpeg embeds.
//
// The embedded payload is resolved at compile time, so the file for the target
// platform must exist before `go build` - fetch each platform the release
// targets, for example `go run ./cmd/fetchffmpeg -os linux -arch arm64`. The
// archives come from BtbN's FFmpeg-Builds (GPL static builds that include
// libx264); see the README for the redistribution note. Running the command
// again is a no-op while the payload is present.
package main

import (
	"archive/tar"
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

const (
	// commandName labels the flag set and the error prefixes.
	commandName = "fetchffmpeg"
	// releaseBaseURL is the download root of the FFmpeg-Builds releases.
	releaseBaseURL = "https://github.com/BtbN/FFmpeg-Builds/releases/download"
	// defaultOutDir is the directory internal/ffmpeg embeds, relative to the
	// repository root.
	defaultOutDir = "internal/ffmpeg/bin"
	// defaultBuild tracks the newest master build. Any release tag that carries
	// the same asset names works, for example `autobuild-2026-09-01-12-00`.
	defaultBuild = "latest"
	// binDir is the archive-relative directory holding the executable in every
	// FFmpeg-Builds archive, for example
	// ffmpeg-master-latest-linux64-gpl/bin/ffmpeg.
	binDir = "bin"
	// downloadTimeout bounds one archive download: the GPL builds are roughly
	// 100-200 MiB, so a slow link still finishes.
	downloadTimeout = 30 * time.Minute
)

// release describes one downloadable platform build.
type release struct {
	// asset is the release asset file name; its extension selects the archive
	// reader.
	asset string
	// exe is the executable's name inside the archive's bin/ directory.
	exe string
}

// releases maps a "goos/goarch" target onto its FFmpeg-Builds asset. Only these
// three platforms have a build-tagged embed file in internal/ffmpeg.
var releases = map[string]release{
	"windows/amd64": {asset: "ffmpeg-master-latest-win64-gpl.zip", exe: "ffmpeg.exe"},
	"linux/amd64":   {asset: "ffmpeg-master-latest-linux64-gpl.tar.xz", exe: "ffmpeg"},
	"linux/arm64":   {asset: "ffmpeg-master-latest-linuxarm64-gpl.tar.xz", exe: "ffmpeg"},
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// run parses the flags, resolves the requested platform and downloads its
// payload. It returns the process exit code.
func run(args []string) int {
	flags := flag.NewFlagSet(commandName, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	targetOS := flags.String("os", runtime.GOOS, "target operating system (linux, windows)")
	targetArch := flags.String("arch", runtime.GOARCH, "target architecture (amd64, arm64)")
	outDir := flags.String("out", defaultOutDir, "directory that receives the payload")
	build := flags.String("build", defaultBuild, "FFmpeg-Builds release tag to download from")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s: unexpected argument %q\n", commandName, flags.Arg(0))
		return 2
	}

	target := strings.ToLower(strings.TrimSpace(*targetOS)) + "/" + strings.ToLower(strings.TrimSpace(*targetArch))
	rel, ok := releases[target]
	if !ok {
		fmt.Fprintf(os.Stderr, "%s: unsupported platform %s; supported: %s\n",
			commandName, target, supportedTargets())
		return 2
	}
	// The name is the one internal/ffmpeg embeds, per platform, with the .exe
	// suffix Windows needs to execute it.
	name := "ffmpeg-" + strings.ReplaceAll(target, "/", "-")
	if strings.HasSuffix(rel.exe, ".exe") {
		name += ".exe"
	}

	if err := fetch(*build, rel, filepath.Join(*outDir, name)); err != nil {
		fmt.Fprintln(os.Stderr, commandName+":", err)
		return 1
	}
	return 0
}

// fetch installs rel's executable at dest. An existing payload is reported and
// left untouched, which keeps a second run cheap and makes the command
// idempotent; delete the file to re-fetch it, for example after changing the
// release tag. A new payload is written through a temporary file and renamed
// into place, so an interrupted run never leaves a partial executable behind.
func fetch(build string, rel release, dest string) error {
	if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
		sum, err := fileSHA256(dest)
		if err != nil {
			return err
		}
		fmt.Printf("%s already present (%s, sha256 %s); delete it to re-fetch\n",
			dest, humanBytes(info.Size()), sum)
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	url := releaseBaseURL + "/" + build + "/" + rel.asset
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	archivePath, err := download(ctx, url, dir)
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(archivePath)
	}()

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dest)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	size, sum, err := extract(rel, archivePath, tmp)
	if err != nil {
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Rename is atomic within dir, and on Windows it is the only way to replace
	// an executable another process may hold open.
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return err
	}
	fmt.Printf("%s (%s, sha256 %s)\n", dest, humanBytes(size), sum)
	return nil
}

// download streams url into a fresh temporary file in dir and returns its path.
// The caller removes the file.
func download(ctx context.Context, url, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: unexpected status %s", url, resp.Status)
	}

	f, err := os.CreateTemp(dir, ".ffmpeg-archive-*.tmp")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
	}()
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// extract copies rel's executable out of the archive at archivePath into w and
// returns its size and lowercase hex SHA-256.
func extract(rel release, archivePath string, w io.Writer) (int64, string, error) {
	if strings.HasSuffix(rel.asset, ".zip") {
		return extractZip(rel, archivePath, w)
	}
	return extractTarXZ(rel, archivePath, w)
}

// extractZip reads the payload from a ZIP archive (the Windows build).
func extractZip(rel release, archivePath string, w io.Writer) (int64, string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return 0, "", fmt.Errorf("open %s: %w", rel.asset, err)
	}
	defer func() {
		_ = reader.Close()
	}()
	for _, f := range reader.File {
		if !isExecutableMember(f.Name, rel.exe) {
			continue
		}
		src, err := f.Open()
		if err != nil {
			return 0, "", fmt.Errorf("open %s in %s: %w", f.Name, rel.asset, err)
		}
		defer func() {
			_ = src.Close()
		}()
		return copyExecutable(w, src)
	}
	return 0, "", fmt.Errorf("%s: archive has no %s/%s", rel.asset, binDir, rel.exe)
}

// extractTarXZ reads the payload from a tar.xz archive (the Linux builds).
func extractTarXZ(rel release, archivePath string, w io.Writer) (int64, string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return 0, "", err
	}
	defer func() {
		_ = f.Close()
	}()
	uncompressed, err := xz.NewReader(f)
	if err != nil {
		return 0, "", fmt.Errorf("open %s: %w", rel.asset, err)
	}
	reader := tar.NewReader(uncompressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, "", fmt.Errorf("read %s: %w", rel.asset, err)
		}
		if header.Typeflag != tar.TypeReg || !isExecutableMember(header.Name, rel.exe) {
			continue
		}
		return copyExecutable(w, reader)
	}
	return 0, "", fmt.Errorf("%s: archive has no %s/%s", rel.asset, binDir, rel.exe)
}

// isExecutableMember reports whether an archive entry is the ffmpeg executable:
// the builds nest everything under one top-level directory, so the entry is
// <release>/bin/<exe>.
func isExecutableMember(name, exe string) bool {
	cleaned := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	return path.Base(cleaned) == exe && path.Base(path.Dir(cleaned)) == binDir
}

// copyExecutable streams src into dst and returns the byte count and hex digest
// of what was written.
func copyExecutable(dst io.Writer, src io.Reader) (int64, string, error) {
	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(dst, digest), src)
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(digest.Sum(nil)), nil
}

// fileSHA256 hashes an existing file.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
	}()
	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// supportedTargets renders the table's keys for an error message.
func supportedTargets() string {
	targets := make([]string, 0, len(releases))
	for target := range releases {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return strings.Join(targets, ", ")
}

// humanBytes renders a byte count the way an operator reads a file size.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f TiB", value/unit)
}
