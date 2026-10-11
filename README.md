# SmartClass Merge

SmartClass Merge turns one recording's raw segments into a single playable video. Recorders write
each stream to [nsc-filehouse](https://github.com/crazy4chicken) as many small segment objects;
merge takes a job naming those segments, combines them with its bundled ffmpeg, and uploads one MP4
to the bucket you choose - with status, progress and retries tracked in PostgreSQL.

It is the media companion to
[SmartClass Webcam Server](https://github.com/crazy4chicken/smartclass-webcam-server): webcam-server
owns the devices and writes the segments, merge assembles them into the video a person can watch.

## What it does

- **Segments in, one video out.** A job names a recording - either its segments explicitly, or the
  recorder's `device_id` + `stream_id`, which merge lists and orders itself. The result is one
  object in your destination bucket, at a key you choose.
- **Nothing to install for video.** The release carries its own ffmpeg, so the host needs no system
  ffmpeg and no codec packages. `MERGE_FFMPEG_PATH` points merge at a host ffmpeg instead.
- **Recordings stay playable.** The video is normally copied rather than re-encoded, keeping the
  recorded quality; when a copy cannot be produced, merge falls back to an H.264 re-encode, so a
  slightly damaged recording still yields a file that plays.
- **Uploads are verified.** The merged video is hashed while it is written, and the object that
  filehouse stored is checked against that digest; on success the job reports `dest.size_bytes` and
  `dest.sha256`.
- **Every job is visible.** Jobs survive restarts: you see what is queued, whether a job is merging
  or uploading, how far it got, and failed attempts are retried before the job ends in a visible
  `failed` state.
- **Scoped access control.** Sign-in and permissions come from your existing `teamusers` IAM:
  `merge:read:*` and `merge:write:*`, each resolved through the `any > team > own` ladder.

## How it works

1. **Create the job.** `POST /api/v1/merges` with the recording's segments (or `device_id` +
   `stream_id`), the frame rate it was captured with, and where the merged video belongs. The job
   starts as `queued` and is answered as a `201` with its id.
2. **The service assembles it.** The segments are streamed from filehouse, in playback order,
   through the bundled ffmpeg into a temp file, which is then uploaded to the destination bucket -
   hashed while it is written, so the job is only marked `succeeded` once the stored object is
   verified.
3. **Play it.** `GET /api/v1/merges/{id}` reports the job's `stage` (`merging`, `uploading`),
   cumulative progress (bytes read, frames encoded, bytes uploaded) and, when it is done, the
   destination key, size and hash. The video is served from filehouse; merge never proxies it.

## API

| Method | Path | What it does |
| --- | --- | --- |
| `POST` | `/api/v1/merges` | Queue a merge job; answers `201` with the job. |
| `GET` | `/api/v1/merges` | List jobs, newest first (`?limit=`, `?cursor=`, `?status=`). |
| `GET` | `/api/v1/merges/{id}` | Follow one job: status, stage, progress, destination, attempts. |
| `POST` | `/api/v1/merges/{id}/cancel` | Cancel a job that is still `queued` or `running`. |
| `GET` | `/healthz` / `/readyz` | Public probes: liveness, and readiness (database, work dir, ffmpeg). |

Jobs run through five states: `queued` → `running` (with `stage` `merging`, then `uploading`) →
`succeeded`, or `failed` once its attempts are used up, or `canceled`. Failures are never silent:
the last error is reported on the job.

## Requirements

- **PostgreSQL 16 or newer** - merge keeps its own tables in the `smartclass_merge` schema and
  applies its migrations on startup; no external migration tool is needed.
- **nsc-filehouse** with the source bucket your recorders write to (`webcam-segments` by default)
  and a destination bucket for the merged videos (`MERGE_DEST_BUCKET`). Both must exist before the
  first job.
- **teamusers** with the six `merge:*` permission keys registered and a service credential for
  merge.
- **A Linux host** running [svchost](https://github.com/crazy4chicken/nekostick-svchost) (or any
  supervisor that can pass environment variables to a binary) and a writable work directory with
  room for one recording per concurrent job.

## Deployment

The full walkthrough - prerequisites, the complete `MERGE_*` configuration reference, the release
artifact layout and the fleet compose file - is in **[docs/guide/deploy.md](docs/guide/deploy.md)**.

The short version, starting from `merge_<version>_linux_x64.zip`:

```sh
# 1. Apply the database migrations
MERGE_DSN='postgres://merge:…@db:5432/merge?sslmode=require' \
  ./merge migrate

# 2. Register the six merge:* permission keys (once per teamusers instance)
./merge register-permissions -token <teamusers admin token>

# 3. Run the API and the merge queue
MERGE_DSN='postgres://merge:…@db:5432/merge?sslmode=require' \
MERGE_TEAMUSERS_URL='https://iam.example.com' \
MERGE_TEAMUSERS_CLIENT_ID='merge-svc' \
MERGE_TEAMUSERS_CLIENT_SECRET='…' \
MERGE_FILEHOUSE_URL='http://filewarehouse:8080' \
MERGE_DEST_BUCKET='recordings' \
  ./merge run

# 4. Check it - /healthz is liveness, /readyz also probes the database, the work dir and ffmpeg
curl -sS http://127.0.0.1:8083/readyz
```

Releases are published for `v*` tags as `merge_<version>_<platform>_<arch>.zip` - `linux` on `x64`
and `arm64`, `windows` on `x64` - with the entry binary `merge` (`merge.exe` on Windows) at the ZIP
root. That naming and layout is exactly what svchost expects, and those three platforms are the
ones the bundled ffmpeg ships for.

`merge doctor` checks a deployment (configuration, database, work directory, ffmpeg and endpoints)
and exits non-zero on failure, so it can gate a rollout; `merge status` runs the same checks and
always exits 0 once the configuration loaded.

## Permissions

| Key | Grants |
| --- | --- |
| `merge:read:any` | Read every job, whatever its owner. |
| `merge:read:team` | Read jobs belonging to the caller's team. |
| `merge:read:own` | Read the caller's own jobs. |
| `merge:write:any` | Create jobs in any team, and cancel any job. |
| `merge:write:team` | Create jobs for the caller's team, and cancel its jobs. |
| `merge:write:own` | Cancel the caller's own jobs. |

A request is authorized at the narrowest scope that covers it, and a list only ever shows the jobs
that scope allows. Creating a job needs `merge:write:any` or `merge:write:team`: the job does not
exist yet, so there is no owner to compare against, and `merge:write:own` alone can cancel but not
create. Register all six keys once with `./merge register-permissions -token <admin token>`; see the
[API guide](docs/guide/api.md#permissions) for how the scope is applied to each route.

## Bundled ffmpeg

The release embeds a static FFmpeg build from
[BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds) - the **GPL** flavour, because the
transcode fallback needs `libx264` - so a host needs nothing installed. Because FFmpeg is
GPL-licensed, **redistributing a binary that embeds it is subject to the GPL**: publish the
corresponding source (FFmpeg plus the FFmpeg-Builds scripts) alongside the release, or point
recipients at the FFmpeg-Builds repository. Set `MERGE_FFMPEG_PATH` to use a host ffmpeg if your
deployment cannot ship a GPL binary.

## Documentation

| Document | What it covers |
| --- | --- |
| [Deployment guide](docs/guide/deploy.md) | Installing, configuring and supervising merge, including the fleet compose file. |
| [API guide](docs/guide/api.md) | Every endpoint, the create body, the job view, error codes and idempotency. |
| [Architecture](docs/guide/architecture.md) | The merge pipeline, the job state machine, retries and the codec table. |
| [Development](docs/guide/development.md) | Running merge locally against a dev database. |
| [API overview](docs/api/overview.md) | Base URL, authentication, permissions, errors and the generated reference. |

The same pages, rendered: <https://crazy4chicken.github.io/smartclass-merge/>.

## License

Released under the GNU Affero General Public License v3.0, the same license as the rest of the
SmartClass fleet; see [LICENSE](LICENSE). The bundled ffmpeg binaries keep their own (GPL) license;
see above.
