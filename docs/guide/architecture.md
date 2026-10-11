---
title: Architecture
outline: 2
---

# Architecture

smartclass-merge is a small control plane in front of one worker loop. The HTTP API turns a request
into a `merge_jobs` row; the scheduler claims rows, the merge engine does the work, and filehouse
holds both the input segments and the merged object. Nothing about a running merge lives in memory:
if the process dies mid-job, the lease expires and another attempt picks the row up.

```text
client ──POST /api/v1/merges──▶ HTTP API ──queued row──▶ PostgreSQL (smartclass_merge.merge_jobs)
                                                                │
                                                        claim (tick, lease)
                                                                ▼
scheduler ──▶ merge engine ──▶ filehouse GET segment objects ──▶ strip [len][payload] framing
                    │                                                     │
                    │                                              stdin pipe:0
                    ▼                                                     ▼
              temp file in MERGE_WORK_DIR ◀── ffmpeg (embedded, -c:v copy) ──┘
                    │
                    └──▶ filehouse PUT / multipart upload ──▶ dest bucket
```

## Components

| Piece | Responsibility |
| --- | --- |
| `internal/httpapi` | chi router, problem+json errors, permission guards; creates, lists, reads and cancels jobs. |
| `internal/scheduler` | Tick loop: claims jobs, bounds concurrency, writes progress and lease heartbeats, records the outcome. |
| `internal/merge` | One job end to end: codec resolution, segment streaming, ffmpeg invocation, verified upload. |
| `internal/ffmpeg` | The embedded build: extraction, `Version`/`Demuxers` probes, `Run`, codec sniffing and the codec→format table. |
| `internal/filehouse` | The nsc-filehouse client: object listing/download, chunked PUT and multipart uploads. |
| `internal/store` | PostgreSQL: migrations, CRUD, the claim query, idempotency. |
| `internal/iamauth` | teamusers token verification, the `any > team > own` permission ladder and the outbound client-credentials token. |

## The merge pipeline

1. **Resolve the codec.** The job may name one (`codec`); when it does not, the engine reads up to
   64 KiB of frames from the first segment, concatenates their payloads and sniffs the codec
   (H.264/H.265 parameter sets, MPEG-4 VOP, AV1 OBU, JPEG SOI). The sniff is a heuristic: H.265
   evidence (a VPS/SPS/PPS NAL, 32..40) outvotes H.264 evidence (1..23), and a stream that shows
   neither is rejected with `invalid_request` rather than guessed.
2. **Pick the format.** `ffmpeg.FormatFor(codec, transcode)` maps the codec onto the demuxer that
   reads the raw bitstream, the output container and the file extension (see the
   [codec table](#codec-table)).
3. **Merge into a temp file.** `MERGE_WORK_DIR/merge-<jobID>/out<ext>` is the destination. Each
   segment in playback order is fetched with `GetObject`, its `[uint32 BE len][encoded frame]`
   framing is stripped, and the payload is written to ffmpeg's stdin - the recording is never held
   in memory. The frame rate comes from the request (raw bitstreams carry no timing), and each
   segment's bytes are counted for progress.
4. **Copy, then transcode if the copy fails.** The first pass is `-c:v copy`, which keeps the
   recorded codec bit-exact. If *ffmpeg itself* fails (a mid-stream parameter change, a damaged
   frame that breaks muxing) and `MERGE_TRANSCODE_FALLBACK` is on, the pipeline runs again with
   `-c:v libx264 -preset veryfast -crf 23 -pix_fmt yuv420p`: slower and lossy, but playable. A
   framing or download error is *not* retried with a transcode - re-downloading the same bytes
   would fail the same way; those surface as a failed attempt and go through the job retry. Both
   paths write `-movflags +faststart` for the `mp4`/`mov` containers, so a player can start from the
   first bytes of the uploaded object.
5. **Upload, verified.** The merged file goes to `dest.bucket`/`dest.key`:
   - at or below `MERGE_OBJECT_MAX_BYTES` (default 5 GiB): one chunked PUT, hashed while it is
     streamed; the digest and size filehouse reports must match what was sent, otherwise the
     attempt fails (`filehouse` stores the SHA-256 it computed, so the object is verified against a
     digest the service saw itself).
   - above it: a sequential multipart upload of `MERGE_MULTIPART_PART_BYTES` parts (at most 10 000
     parts), each staged part checked against its own digest, the completion body carrying the part
     list. Multipart exists to bypass the direct-PUT size cap; a failure before completion aborts
     the upload, so no staged parts are left behind.
6. **Clean up.** The job directory is removed on every exit path, success or failure.

## Progress reporting

Progress is written in two throttled layers, so a fast merge does not hammer PostgreSQL:

| Layer | Cadence | Writes |
| --- | --- | --- |
| Engine → scheduler | 500 ms | One full `Progress` snapshot in memory. |
| Scheduler → store | ≥ 2 s | `store.JobStore.Progress`, which also renews the lease. |

Every snapshot is cumulative - `input_bytes`/`input_total` while merging, `output_bytes`,
`output_duration_ms` and `frames` once ffmpeg has produced output, `uploaded_bytes` while
uploading - and `stage` names the phase (`merging` or `uploading`) that produced it.

## Job state machine

```text
                 create                    claim                 succeed
   (none) ─────────────▶ queued ─────────────▶ running ─────────────▶ succeeded
                           ▲   │                 │  │
              retry with   │   │ cancel          │  │ fail, attempts    fail, attempts
              backoff      └───┼─────────────────┘  └─── exhausted ────▶ failed
                               ▼                   
                           canceled ◀── cancel (queued or running)
```

| Transition | Guard | Effect |
| --- | --- | --- |
| → `queued` | `POST /api/v1/merges` | Row created with `max_attempts`, `next_attempt_at = now`. |
| `queued` → `running` | a worker claims it | `attempts = attempts + 1`, lease granted to the claiming process, `stage` set. |
| `running` → `queued` | attempt failed, `attempts < max_attempts` | `last_error` set, `next_attempt_at = now + backoff × attempts`. |
| `running` → `failed` | attempt failed, budget exhausted, or `MERGE_JOB_TIMEOUT` hit | `finished_at` set; the job is never picked up again. |
| `running` → `succeeded` | upload verified | `dest.size_bytes`, `dest.sha256`, `output_duration_ms`, `output_frames`, `output_codec` recorded, `finished_at` set. |
| `queued`/`running` → `canceled` | `POST /api/v1/merges/{id}/cancel` | Terminal. A canceled job is never retried, and a late terminal write from the attempt cannot overwrite `canceled`. |

A cancel is honored two ways: an in-process cancel signals the running merge's context immediately,
and a worker that does not own the job (or an operator using SQL) still stops within one progress
write, because the scheduler polls the row's status between heartbeats.

## Retries, leases and timeouts

| Knob | Default | Meaning |
| --- | --- | --- |
| `MERGE_SCHED_TICK` | `5s` | How often the scheduler looks for claimable jobs. |
| `MERGE_JOB_LEASE` | `2m` | How long a claim stays valid without a heartbeat. Claims use `FOR UPDATE SKIP LOCKED`, and a row whose lease has expired is claimed again - that is how a job survives a worker crash. |
| `MERGE_MAX_ATTEMPTS` | `3` | Total attempts per job before it is failed. |
| `MERGE_RETRY_BACKOFF` | `1m` | Base of the retry delay: attempt *n* waits `backoff × n`. |
| `MERGE_JOB_TIMEOUT` | `6h` | Wall-clock budget of one attempt; on expiry the attempt is failed (and retried per the policy). |
| `MERGE_MAX_CONCURRENT_JOBS` | `1` | Merges this process runs at once. |

The retry delay grows with the attempt number, so a broken source bucket or a full disk does not
burn the whole attempt budget in seconds. Lease renewal happens on every progress write, which is
why the scheduler's heartbeat keeps a long merge from looking dead.

## Codec table

| Codec | Demuxer | Container | Extension | Note |
| --- | --- | --- | --- | --- |
| `h264` | `h264` | `mp4` | `.mp4` | The recorder default; `-c:v copy` keeps the bitstream. |
| `h265` | `hevc` | `mp4` | `.mp4` | |
| `mjpeg` | `mjpeg` | `mov` | `.mov` | MJPEG has no `mp4` muxer profile; the output is QuickTime. |
| `mpeg4` | `m4v` | `mp4` | `.mp4` | Raw MPEG-4 part 2 elementary stream. |
| `av1` | `av1` | `mp4` | `.mp4` | |
| *transcode fallback* | recorded codec's demuxer | `mp4` | `.mp4` | Output is always H.264. |

`vp8` and `vp9` are part of the recorder's vocabulary but have no raw elementary-stream demuxer in
ffmpeg, so a request naming them is rejected with `invalid_request`. The same applies to a
recording whose codec cannot be determined: without a `codec` field and without a sniffable first
frame (parameter sets, an AV1 OBU or JPEG data), the job is rejected instead of guessed at.

## What filehouse holds

| Object | Bucket | Key |
| --- | --- | --- |
| Source segments | `MERGE_SOURCE_BUCKET` (`webcam-segments`) | `{device_id}/streams/{stream_id}/{YYYYMMDDTHHMMSS}_{first_seq}.bin`, written by webcam-server. |
| Merged video | `MERGE_DEST_BUCKET` | The request's `dest.key`, defaulting to `merged/{YYYY}/{MM}/{DD}/{id}{ext}`. |

Listing a `{device_id}/streams/{stream_id}/` prefix returns the recording's segments; the service
orders them by flush timestamp (parsed from the key) and then by the `first_seq` suffix, which is
also the order a recorder's `segment_seq` produces. An explicit `segments` array is sorted by `seq`
when every entry carries one, and otherwise kept in the order given.
