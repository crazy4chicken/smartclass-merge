---
title: API Overview
---

# API Overview

smartclass-merge exposes one HTTP API: create a merge job, follow it, cancel it, plus two
unauthenticated health probes. The generated reference is produced from the same route table the
server serves, so it can never drift from the running code.

## Base URL

All paths in this reference are relative to the server's listen address, `MERGE_LISTEN_ADDR`
(default `:8083`), and live under `/api/v1`:

```text
http://merge.example.com:8083/api/v1/merges
```

In the fleet, the svchost `route.prefix: /merge` is stripped before the request reaches the
process, so the public URL is `https://<host>/merge/api/v1/merges`. See the
[deployment guide](/guide/deploy) for how the service is exposed.

## Authentication

Every `/api/v1` call needs a teamusers-issued bearer token:

```http
Authorization: Bearer <access token>
```

The token is verified locally against the issuer's JWKS, then the caller's `merge:*` permissions are
resolved remotely - permissions are never carried inside the token. Failures are never silent:

- a request without verified claims answers `401` with the teamusers decision body
  (`{"allow": false, "reason": "..."}`) and `WWW-Authenticate: Bearer realm="teamusers"`; a token
  that fails verification is rejected by the teamusers middleware, which answers the same decision
  body with the code family the SDK reports (`invalid_token*`);
- a verified caller without a grant covering the target answers `403` with the problem detail
  `insufficient_permissions`, whose `reason` names the keys that were tried;
- when the authorization service cannot be reached the request answers `503` - merge fails closed,
  never open.

Every route under `/api/v1` is guarded this way - only `/healthz` and `/readyz` are public - and a
job route resolves its resource before the permission check. The keys and how they are granted are
documented in [API → Permissions](/guide/api#permissions).

## Permissions

The key grammar is `merge:<action>:<scope>` with `action ∈ {read, write}` and
`scope ∈ {any, team, own}`, resolved through the `any > team > own` ladder:

| Key | Grants |
| --- | --- |
| `merge:read:any` | Read every job, whatever its owner: list jobs across teams and fetch any job. |
| `merge:read:team` | Read jobs belonging to the caller's team. |
| `merge:read:own` | Read jobs created by the caller. |
| `merge:write:any` | Create jobs in any team, and cancel any job. |
| `merge:write:team` | Create jobs for the caller's team, and cancel its jobs. |
| `merge:write:own` | Cancel jobs created by the caller. |

A single-job route resolves the job first, then tries the caller's grants against the job's scope;
a list request returns exactly the rows the granting rung allows - the caller's own jobs, the
caller's team, or everything for an `any` grant - so a list is never a window into another team's
jobs.

Creating a job is the one route without an `own` rung: the job does not exist yet, so there is no
owner to compare against. `POST /api/v1/merges` therefore accepts `merge:write:any` always and
`merge:write:team` when the token carries a team claim (the created job belongs to that team);
`merge:write:own` alone can cancel but not create. Register the six keys once with
`merge register-permissions -token <admin token>`.

## Errors

Failures are reported as RFC 9457 problem details with the content type
`application/problem+json`:

```json
{
  "type": "about:blank",
  "title": "Not Found",
  "status": 404,
  "detail": "job_not_found",
  "instance": "/api/v1/merges/01J8Z0Q7RD3F9K1V0P4S8M2W6X"
}
```

| Field | JSON type | Present | Meaning |
| --- | --- | --- | --- |
| `type` | string | always | Always `about:blank`; the service defines no per-error type URIs of its own. |
| `title` | string | always | The HTTP status text of `status`. |
| `status` | integer | always | The HTTP status code, repeated in the body. |
| `detail` | string | always | A **stable code** from the table below - switch on it, not on `title`. |
| `instance` | string | always | Path of the failing request. |
| `reason` | string | when relevant | Extra human-readable context, e.g. the attempted permission keys on a `403`. Never carries credentials. |

| HTTP | `detail` | Returned when |
| --- | --- | --- |
| 400 | `invalid_request` | Malformed body, query or path: an unknown or conflicting member, an unsupported codec, an out-of-range fps, a repeated segment key, a non-positive `limit` or an unknown `status`. |
| 401 | `invalid_token*` | The bearer token is missing, expired or fails verification. The response is the teamusers decision body, not a problem document; `*` marks the code family the teamusers middleware reports. |
| 403 | `insufficient_permissions` | Verified caller, no candidate `merge:*` key allowed. |
| 404 | `job_not_found` | No job with that id (or it was not created as a merge job). |
| 409 | `status_not_cancelable` | The job already ended in `succeeded` or `failed`. |
| 500 | `internal_error` | An unexpected failure; the cause is logged, never returned. |
| 503 | `source_unavailable` | The segments of the recording could not be listed from filehouse; nothing was enqueued. |
| 503 | `iam_unavailable` | The permission service or the JWKS endpoint could not be reached; the request was not authorized. |
| 503 | `service_unavailable` | The API is not usable in this process - its authorization layer could not be built - so every guarded request fails closed. |

Responses carry `X-Request-Id`, which is also logged; include it in a bug report. A missing-claims
`401` is the one response that is not a problem document: it uses the teamusers decision body.

## Collections

List endpoints wrap their results in an `items` envelope, so a response is always a JSON object and
never a bare array:

```json
{"items": [ /* job views */ ], "next_cursor": "01J8Z0Q7RD3F9K1V0P4S8M2W6X"}
```

An empty collection returns `{"items": []}`, and `next_cursor` is empty on the last page: pass it
back unchanged as `cursor` to continue where the previous page stopped.

```http
GET /api/v1/merges?limit=50&status=running
```

- `limit` - page size. The default is `100` when omitted, the maximum is `1000`, and larger values
  are clamped, not rejected. A non-numeric or non-positive value answers `400 invalid_request`.
- `cursor` - opaque continuation token from the previous page's `next_cursor`.
- `status` - filter by job state: `queued`, `running`, `succeeded`, `failed` or `canceled`. Any
  other value answers `400 invalid_request`.

Rows are returned newest first, and the visible set is the caller's permission scope.

## Idempotency

`POST /api/v1/merges` accepts an optional `X-Idempotency-Key` header. Replaying a request with the
same key returns the job the first call created - again `201`, this time with
`Idempotency-Replayed: true` - instead of starting a second merge, so a retried client call is
safe. Keys are scoped per owner: two callers may use the same key without colliding.

```http
POST /api/v1/merges
X-Idempotency-Key: lecture-2026-10-11-room-3
```

## Reference

One page per API tag, generated from the OpenAPI document:

- [Merges](/api/reference/merges) - create, list, fetch and cancel merge jobs.
- [Health](/api/reference/health) - liveness and readiness probes.

The machine-readable document these pages are built from is available for download:
[openapi.yaml](/openapi.yaml).
