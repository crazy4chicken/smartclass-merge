---
title: Development
outline: 2
---

# Development

This guide runs merge on one machine: the embedded ffmpeg payload, a local PostgreSQL, the dev
bypass, and an end-to-end outline that merges a synthetic recording into a locally started
filehouse.

## Prerequisites

- **Go 1.26 or newer** (the module targets `go 1.26.0`).
- **PostgreSQL 16 or newer**, or Docker to run one.
- **The ffmpeg payload** for the platform you are on - `go run ./cmd/fetchffmpeg` (see below). A
  host ffmpeg works instead; point `MERGE_FFMPEG_PATH` at it.
- Optional, for the end-to-end run: **nsc-filewarehouse** and one of its buckets.

## 1. Fetch the embedded ffmpeg

The ffmpeg build is embedded at compile time, so the file must exist before `go build` - even for
`go test` and `go run`:

```sh
go run ./cmd/fetchffmpeg                       # the host platform (windows/amd64 on a dev box)
go run ./cmd/fetchffmpeg -os linux -arch arm64 # another platform you intend to build for
go run ./cmd/fetchffmpeg -build autobuild-2026-09-01-12-00  # a pinned FFmpeg-Builds tag
```

The command downloads the BtbN GPL build into `internal/ffmpeg/bin/ffmpeg-<os>-<arch>[.exe]`,
prints its size and SHA-256, and does nothing while the payload is already there (delete the file to
re-fetch). The payloads are gitignored; `internal/ffmpeg/bin/README.md` explains the layout. Note
that **everything** in `internal/ffmpeg/bin/` is embedded into any binary you build, so keep only
the platform you are building for and remove the others before a release build.

During development an already installed ffmpeg is usually more convenient than the ~100 MiB
payload:

```sh
MERGE_FFMPEG_PATH=$(command -v ffmpeg) ./merge doctor
```

`MERGE_FFMPEG_PATH` wins over the embedded build everywhere, and `doctor` reports which one is in
use. When it is set, the embedded payload is never extracted - useful while testing the API without
a full checkout.

## 2. Local PostgreSQL

One database is enough; merge creates its own `smartclass_merge` schema inside it:

```sh
createdb smartclass
psql -d smartclass -c "CREATE ROLE merge LOGIN PASSWORD 'change-me'"
psql -d smartclass -c "GRANT CONNECT ON DATABASE smartclass TO merge"
```

Then apply the migrations and check the connection:

```sh
export MERGE_DSN='postgres://merge:change-me@127.0.0.1:5432/smartclass?sslmode=disable'
go run ./cmd/merge migrate
go run ./cmd/merge status
```

`status` prints the redacted configuration and reports `PASS postgres …` or `SKIP` when
`MERGE_DSN` is unset. `doctor` is the same check with a non-zero exit code; it is what a deployment
gate should call.

## 3. Configuration

Every value is an environment variable; `.env.example` documents all of them with the defaults.
Point `MERGE_ENV_FILE` at a copy and real environment variables keep winning over it:

```sh
cp .env.example .env.local
MERGE_ENV_FILE=$PWD/.env.local go run ./cmd/merge run
```

## 4. Run it with the dev bypass

`MERGE_DEV=true` turns off teamusers entirely: requests are authenticated as a synthetic
`dev-user`, every permission check passes, and outbound filehouse calls carry the literal service
token `dev` (a dev filehouse must accept it, or be started with its own auth bypass). Never enable
it on a reachable host.

```sh
MERGE_DEV=true \
MERGE_DSN='postgres://merge:change-me@127.0.0.1:5432/smartclass?sslmode=disable' \
MERGE_FILEHOUSE_URL='http://127.0.0.1:8080' \
MERGE_DEST_BUCKET=recordings \
  go run ./cmd/merge run
```

Without the dev bypass, run a teamusers instance (or point `MERGE_TEAMUSERS_URL` at the shared one)
and register the permission catalog once:

```sh
go run ./cmd/merge register-permissions -token "$TEAMUSERS_ADMIN_TOKEN"
```

The API is served on `MERGE_LISTEN_ADDR` (`:8083`):

```sh
curl -sS -X POST http://127.0.0.1:8083/api/v1/merges \
  -H 'Content-Type: application/json' \
  -d '{
        "segments": [
          {"key": "dev/streams/s1/20261011T080001_1.bin", "seq": 1},
          {"key": "dev/streams/s1/20261011T080002_2.bin", "seq": 2}
        ],
        "fps": 25,
        "codec": "h264",
        "dest": {"bucket": "recordings", "key": "merged/dev-lecture.mp4"}
      }'
curl -sS http://127.0.0.1:8083/api/v1/merges/<id>
```

## 5. End-to-end against a local filehouse

The recorder's input format is repeatable offline, so the whole pipeline can be exercised on one
machine:

1. **Start nsc-filewarehouse** with a local blob directory and DSN, and create the two buckets
   (`webcam-segments`, `recordings`) through its API.
2. **Produce a synthetic recording.** Encode a test pattern to a raw H.264 elementary stream and
   split it into length-prefixed segments - the recorder's `[uint32 BE len][encoded frame]` framing,
   with no container and no timing:

   ```sh
   ffmpeg -hide_banner -f lavfi -i testsrc=size=640x360:rate=25:duration=10 \
     -c:v libx264 -preset veryfast -f h264 raw.h264
   ```

   Then split `raw.h264` into frames (one access unit per frame is what the recorder stores) and
   write each run of frames as `[len][payload]` into `seg-1.bin`, `seg-2.bin`, … - a throwaway
   script is the right tool; nothing in the repository needs to keep it.
3. **Upload the segments** to `webcam-segments` with the filehouse API (`PUT
   /api/v1/buckets/webcam-segments/objects/dev/streams/s1/20261011T080001_1.bin`).
   Because the job can name them explicitly, the listing path (and the recorder's key scheme) is
   not needed for this test.
4. **Create the merge job** with those keys, `fps` matching the encode (25) and `codec: "h264"`.
5. **Follow it** to `succeeded`; `progress.frames` should reach 250 for a 10 s clip at 25 fps and
   `dest.sha256` should match the downloaded object.
6. **Verify the output**: download `dest.key` from the destination bucket and inspect it -
   `ffmpeg -i merged.mp4` should report the same duration and frame count, and the codec should
   still be H.264 (the copy path did not re-encode).

Watching `merge run`'s log lines is the fastest way to see each step (codec resolution, the ffmpeg
command, progress blocks summed, the upload result); set `MERGE_LOG_LEVEL=debug` for the sniff
decision and per-segment reads.

## 6. Working on the code

| Area | Where |
| --- | --- |
| API routes and handlers | `internal/httpapi` |
| Job store, claims, migrations | `internal/store` |
| Scheduler and retry policy | `internal/scheduler` |
| Merge pipeline and upload | `internal/merge` |
| ffmpeg invocation, sniffing, codec table | `internal/ffmpeg` |
| filehouse client | `internal/filehouse` |
| Configuration and validation | `internal/config` |

Before pushing:

```sh
go build ./...
go vet ./...
gofmt -l .
```

The API reference under `docs/public/openapi.yaml` is generated from the router by
`go run ./cmd/genspec`; regenerate it when a route or a request/response shape changes, and commit
the result.
