# Embedded ffmpeg binaries

This directory holds the ffmpeg executables that are compiled into the `merge`
binary. They are **not** committed (see `.gitignore`); only this file and
`.gitkeep` are tracked, so the directory exists in a fresh checkout.

| File | Embedded by | Platform |
| --- | --- | --- |
| `ffmpeg-windows-amd64.exe` | `internal/ffmpeg/embed_windows_amd64.go` | windows/amd64 |
| `ffmpeg-linux-amd64` | `internal/ffmpeg/embed_linux_amd64.go` | linux/amd64 |
| `ffmpeg-linux-arm64` | `internal/ffmpeg/embed_linux_arm64.go` | linux/arm64 |

## Fetching

```sh
go run ./cmd/fetchffmpeg                 # current GOOS/GOARCH
go run ./cmd/fetchffmpeg -os linux -arch arm64
```

The command is idempotent and prints the SHA-256 and size of the file it wrote.
Fetch only the platform you build for: each platform file embeds this whole
directory, so every payload present here ends up inside the binary.

## Build and startup behavior

`cmd/fetchffmpeg` needs to run for a platform before that platform's build can
**serve merges**, but not before it can be **built**: the platform files embed the
directory (`//go:embed all:bin`), so a checkout whose payload was never fetched
still compiles and `go vet ./...` stays clean.

A build without its payload reports the missing ffmpeg the first time one is
needed — startup validation, `merge doctor` or the first job — with
`ffmpeg: no embedded build for this platform; run go run ./cmd/fetchffmpeg for it
or set MERGE_FFMPEG_PATH to an existing ffmpeg`. A platform this package does not
support at all (anything but windows/amd64, linux/amd64 and linux/arm64) reports
the same message.

`MERGE_FFMPEG_PATH` overrides the embedded build entirely: the configured
executable is used as-is and neither the payload nor this directory is touched.
That is the way to develop and verify against a host ffmpeg without fetching
anything.

The embedded build is extracted on first use to `<MERGE_WORK_DIR>/ffmpeg/<name>`
through a temporary file and a rename, with mode `0755`. An extracted file of the
expected size is reused, which avoids rewriting a ~100 MB file per process and
avoids replacing a running binary on Windows (where an in-use executable cannot
be deleted or renamed over).

## Source and license

The binaries come from [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds)
(`ffmpeg-master-latest-win64-gpl.zip`,
`ffmpeg-master-latest-linux64-gpl.tar.xz`,
`ffmpeg-master-latest-linuxarm64-gpl.tar.xz`). They are **GPL** static builds
that include `libx264`, which the transcode fallback uses. Redistributing a
binary that embeds them must comply with the GPL; the build job that produces
release artifacts is responsible for shipping the corresponding license texts.
