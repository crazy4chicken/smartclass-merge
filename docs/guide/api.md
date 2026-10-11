---
title: API
outline: 2
---

# API

The merge API is a small control plane: create a job, follow it, cancel it. Everything is
authenticated by a teamusers JWT and authorized by the permission keys below; the merged video
itself is never proxied - it lands in filehouse and is fetched from there.

Base URL: the service root, for example `http://merge:8083` or `https://host.example.com/merge`
behind the svchost route from the [deployment guide](/guide/deploy).

## Authentication

Every route except `/healthz` and `/readyz` requires `Authorization: Bearer <JWT>`, a token issued
by teamusers with the audience the service is configured with (`MERGE_TEAMUSERS_AUD`). The token is
verified locally against the issuer's JWKS; there is no session state. The caller's subject, teams
and permission grants are read from the token and the permission service.

## Permissions

| Route | Permission |
| --- | --- |
| `GET /api/v1/merges` | `merge:read:any` or `merge:read:team` or `merge:read:own` |
| `GET /api/v1/merges/{id}` | `merge:read:any` or `merge:read:team` or `merge:read:own`, scoped to the job |
| `POST /api/v1/merges` | `merge:write:any`, or `merge:write:team` when the token carries a team |
| `POST /api/v1/merges/{id}/cancel` | `merge:write:any` or `merge:write:team` or `merge:write:own`, scoped to the job |

The ladder is `any > team > own`:

- **`any`** is the school-wide scope. It covers every job, whatever its owner.
- **`team`** covers jobs whose `team_id` the caller holds.
- **`own`** covers jobs whose `owner_id` is the caller's subject.

A single-job route resolves the job first, then checks the caller's grants against the job's scope;
a caller without any covering grant gets `403 insufficient_permissions` (the problem document's
`reason` names the keys that were tried). A list request returns exactly the rows the caller's
narrowest covering scope allows: `own` for an `own`-only caller, the caller's teams for a `team`
grant, everything for `any`.

Register the six keys once with `merge register-permissions -token <admin token>`; they are also the
keys a directory service or role in teamusers must carry to hand out access.

Creation is the one route without an `own` rung: a job that does not exist yet has no owner to
compare against, so `POST /api/v1/merges` accepts `merge:write:any` always and `merge:write:team`
when the token carries a team claim - the created job belongs to that team. `merge:write:own` alone
grants cancels of the caller's own jobs, not creation.

## Endpoints

| Method | Path | Permission | Purpose |
| --- | --- | --- | --- |
| `GET` | `/healthz` | public | Liveness: `{"status":"ok"}` while the process runs. |
| `GET` | `/readyz` | public | Readiness: Postgres ping, work dir writable, ffmpeg resolvable. `503 {"status":"unavailable"}` when one fails. |
| `POST` | `/api/v1/merges` | `merge:write:any` / `merge:write:team` | Create a merge job. |
| `GET` | `/api/v1/merges` | `merge:read:*` | List jobs (`?limit=&cursor=&status=`). |
| `GET` | `/api/v1/merges/{id}` | `merge:read:*` | One job. |
| `POST` | `/api/v1/merges/{id}/cancel` | `merge:write:*` | Cancel a `queued` or `running` job. |

## Create a job

```http
POST /api/v1/merges
Authorization: Bearer <JWT>
X-Idempotency-Key: lecture-2026-10-11-room-3
Content-Type: application/json

{
  "bucket": "webcam-segments",
  "device_id": "01J8Z0M4Q1",
  "stream_id": "01J8Z0M4Q2",
  "fps": 30,
  "codec": "h264",
  "dest": {"bucket": "recordings", "key": "merged/2026/10/11/lecture.mp4"}
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `bucket` | string | No | Source bucket; defaults to `MERGE_SOURCE_BUCKET` (`webcam-segments`). |
| `device_id` | string | with `stream_id` | The recorder's device id; merge lists the stream's segments itself. |
| `stream_id` | string | with `device_id` | The stream whose segments to merge (`{device_id}/streams/{stream_id}/`). |
| `segments` | array | with `bucket` | Explicit selection; mutually exclusive with `device_id`/`stream_id`. |
| `segments[].key` | string | yes | Object key inside the source bucket. |
| `segments[].seq` | int | no | The recorder's `segment_seq`; when every entry carries one, the array is sorted by it, otherwise the given order is kept. |
| `segments[].size_bytes` | int | no | Declared size, used as the input total of the progress report. |
| `fps` | int | No | Frame rate of the recording, `1`–`240`; defaults to `MERGE_DEFAULT_FPS` (`25`). The raw bitstream carries no timing, so this is the only source of playback speed. |
| `codec` | string | No | One of `h264`, `h265`, `mjpeg`, `mpeg4`, `av1`. Omit it and the codec is sniffed from the first segment. |
| `dest.bucket` | string | No | Destination bucket; defaults to `MERGE_DEST_BUCKET` (which must be configured). |
| `dest.key` | string | No | Destination key; defaults to `merged/{YYYY}/{MM}/{DD}/{id}{ext}`, where `ext` comes from the codec. |

Rules enforced while decoding (the body is a strict JSON document capped at 1 MiB: an unknown
member is rejected, not ignored):

- Give **either** `segments` **or** `device_id` + `stream_id`. Both, neither, or only one of the
  pair is `400 invalid_request`; an empty `segments` array counts as "not given".
- When `device_id` + `stream_id` are used, merge lists the prefix and orders the objects by flush
  timestamp (parsed from the key) and then by the `first_seq` suffix. A prefix without any segment
  is `400 invalid_request`; a filehouse failure (or an object whose key does not carry the
  recorder's `{YYYYMMDDTHHMMSS}_{first_seq}.bin` name) is `503 source_unavailable` - nothing is
  enqueued either way.
- An explicit `segments` array must carry a non-empty `key` on every entry, must not repeat a key,
  and neither `seq` nor `size_bytes` may be negative.
- `fps` outside `1`–`240` is `400 invalid_request`, as is a `codec` outside the supported set
  (`vp8` and `vp9` are part of the recorder's vocabulary but have no raw-bitstream demuxer, so they
  are rejected here).
- The destination bucket must be resolvable - the request's `dest.bucket` or `MERGE_DEST_BUCKET` -
  otherwise the request is `400 invalid_request` (and `run` refuses to start without one).
- `bucket` must be a bucket the caller's filehouse credential can read: the filehouse client is
  authenticated with merge's service token, and filehouse decides what that account may access.

The answer is `201 Created` with the [job view](#the-job-view) and a `Location` header pointing at
`/api/v1/merges/{id}`. An `X-Idempotency-Key` header makes the request replay-safe: repeating it
returns the original job - again `201`, like filehouse's own upload replay - with
`Idempotency-Replayed: true` instead of creating a second one.

```http
HTTP/1.1 201 Created
Content-Type: application/json
Idempotency-Replayed: true

{"id":"01J8Z0Q7RD3F9K1V0P4S8M2W6X","status":"queued", ...}
```

Keys are scoped per owner: two callers may use the same key without colliding.

## The job view

```json
{
  "id": "01J8Z0Q7RD3F9K1V0P4S8M2W6X",
  "status": "running",
  "stage": "merging",
  "owner_id": "01J8Z0M4Q1",
  "owner_kind": "service",
  "team_id": "",
  "source": {
    "bucket": "webcam-segments",
    "prefix": "01J8Z0M4Q1/streams/01J8Z0M4Q2/",
    "segments": [{"key": "01J8Z0M4Q1/streams/01J8Z0M4Q2/20261011T080001_42.bin", "seq": 42, "size_bytes": 18345984}],
    "fps": 30,
    "codec": "h264"
  },
  "dest": {"bucket": "recordings", "key": "merged/2026/10/11/lecture.mp4"},
  "attempts": 1,
  "max_attempts": 3,
  "progress": {
    "input_bytes": 73400320,
    "input_total": 110075904,
    "output_bytes": 0,
    "uploaded_bytes": 0,
    "output_duration_ms": 0,
    "frames": 0
  },
  "error": "",
  "created_at": "2026-10-11T08:05:00Z",
  "updated_at": "2026-10-11T08:05:04Z",
  "started_at": "2026-10-11T08:05:01Z",
  "finished_at": null
}
```

| Field | Notes |
| --- | --- |
| `status` | `queued`, `running`, `succeeded`, `failed` or `canceled`. |
| `stage` | `""`, `merging` or `uploading` - the phase of a `running` job. |
| `owner_id` / `owner_kind` / `team_id` | Who created the job and which team it belongs to; these drive the permission scope. |
| `source.segments` | The resolved segment list, in playback order. |
| `dest.size_bytes` / `dest.sha256` | Filled in on success: the stored size and the digest filehouse reported. |
| `progress` | Cumulative counters; see the [architecture guide](/guide/architecture#progress-reporting). |
| `error` | `last_error`: the failure of the last attempt, empty otherwise. |
| `attempts` / `max_attempts` | Attempts used and the retry budget; see [retries](/guide/architecture#retries-leases-and-timeouts). |

## List jobs

```http
GET /api/v1/merges?limit=50&status=running
Authorization: Bearer <JWT>
```

| Query | Default | Notes |
| --- | --- | --- |
| `limit` | `100` | Clamped to 1000. |
| `cursor` | - | Opaque cursor from the previous page; pass it back unchanged. |
| `status` | - | Filter by status; an unknown value is `400 invalid_request`. |

```json
{"items": [ /* job views */ ], "next_cursor": "01J8Z0Q7RD3F9K1V0P4S8M2W6X"}
```

`next_cursor` is empty on the last page. Rows are returned newest first, and the visible set is the
caller's permission scope - the list is never a window into another team's jobs.

## Cancel a job

```http
POST /api/v1/merges/01J8Z0Q7RD3F9K1V0P4S8M2W6X/cancel
Authorization: Bearer <JWT>
```

- `queued` or `running` → the job becomes `canceled` and the running merge is stopped as soon as it
  notices (immediately when this process runs it).
- Already `canceled` → `200` with the unchanged job.
- `succeeded` or `failed` → `409 status_not_cancelable`.
- Unknown id → `404 job_not_found`.

## Errors

Every error is an [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem document; `detail` is
the stable machine-readable code and must be parsed, not the `title`.

```json
{
  "type": "about:blank",
  "title": "Bad Request",
  "status": 400,
  "detail": "invalid_request",
  "instance": "/api/v1/merges"
}
```

| HTTP | `detail` | Meaning |
| --- | --- | --- |
| 400 | `invalid_request` | Malformed JSON, an unknown member, a missing or conflicting field, an unsupported codec or an out-of-range fps. |
| 401 | `invalid_token*` | The bearer token is missing, expired or fails verification (the exact code comes from the teamusers SDK). |
| 403 | `insufficient_permissions` | The token is valid but carries no grant covering the job's scope; `reason` names the tried keys. |
| 404 | `job_not_found` | No job with that id (or it was not created as a merge job). |
| 409 | `status_not_cancelable` | The job already ended in `succeeded` or `failed`. |
| 503 | `source_unavailable` | filehouse could not be listed while resolving `device_id`/`stream_id`; nothing was enqueued. |
| 503 | `iam_unavailable` | The permission service or the JWKS endpoint could not be reached; the request was not authorized. |
| 503 | `service_unavailable` | The service cannot serve the request: database unreachable, destination bucket unconfigured, work dir unusable. |

Responses carry `X-Request-ID`, which is also logged; include it in a bug report. Failures are
never silent: a merge that cannot finish ends in `failed` with the cause in `error`, and every
attempt is retried per the job's `max_attempts` before that.

## A complete run

```sh
# 1. Create the job (device + stream: let merge list the segments)
curl -sS -X POST "$BASE/api/v1/merges" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"device_id":"01J8Z0M4Q1","stream_id":"01J8Z0M4Q2","fps":25,"codec":"h264",
       "dest":{"bucket":"recordings","key":"merged/2026/10/11/lecture.mp4"}}'

# 2. Follow it
curl -sS "$BASE/api/v1/merges/$ID" -H "Authorization: Bearer $TOKEN"

# 3. When status is succeeded, the object is in filehouse
#    dest.bucket/dest.key, with dest.sha256 for verification.
```
