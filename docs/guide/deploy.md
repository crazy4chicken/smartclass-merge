---
title: Deployment
outline: 2
---

# Deployment

This guide covers the Nekostick/svchost path used by the SmartClass fleet: one compose document
declares the services (teamusers, filewarehouse, webcam-server, dispatchub, merge), the Host injects
addresses, ports and secrets, and merge runs as a queue worker beside the rest.

## Prerequisites

- **PostgreSQL 16 or newer**, reachable from the merge launch. The service applies its embedded
  goose migrations on startup; no external migration tool is needed. Every object it creates lives
  in the schema `smartclass_merge`, which it creates on the first migrate, so the DSN role needs no
  rights on the database's `public` schema. A role that does not own the database cannot create a
  schema: a DBA creates it once and grants it, and merge then uses it as-is.
- **nsc-filehouse** with two buckets: the source bucket your recorders write to
  (`MERGE_SOURCE_BUCKET`, `webcam-segments` by default) and the destination bucket the merged
  videos go to (`MERGE_DEST_BUCKET`). Both must exist; create them through the filehouse API or its
  console before the first job.
- **A teamusers instance** with the six `merge:*` keys registered and a service credential for
  merge. See [API → Permissions](/guide/api#permissions).
- **The ffmpeg payload for the target platform**, fetched with `go run ./cmd/fetchffmpeg` before
  `go build`. The release workflow does this for every platform it publishes.
- **A writable work directory** with room for at least one merged recording per concurrent job -
  `MERGE_WORK_DIR`, `/var/lib/merge/work` in the compose below. It holds the temp video while it is
  encoded and uploaded, so size it at `MERGE_MAX_CONCURRENT_JOBS × (largest expected recording)`.
- **svchost** or any other supervisor that can run a Linux binary with environment variables.

## Release artifact

The release workflow publishes `merge_<version>_<platform>_<arch>.zip` for `linux` on `amd64`
(`x64`) and `arm64`, and for `windows` on `amd64`, with the entry binary `merge` (`merge.exe` on
Windows) at the ZIP root. Those three are exactly the platforms the embedded ffmpeg ships for, so a
release for any other platform would have no ffmpeg to run unless `MERGE_FFMPEG_PATH` is set. The
layout is the svchost contract: the asset name carries the service id, the platform and the
architecture, and the entry binary must carry the exact service name.

The embedded ffmpeg makes the artifact platform-specific **and** architecture-specific: each ZIP
carries the binary for its target (linux/amd64, linux/arm64 or windows/amd64), which the build
embeds. Fetch before building:

```sh
# one payload per platform you publish; the embed is compile-time
go run ./cmd/fetchffmpeg -os linux -arch amd64
go run ./cmd/fetchffmpeg -os linux -arch arm64

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags '-s -w -X main.version=v0.1.0' -o dist/merge ./cmd/merge
```

Everything present in `internal/ffmpeg/bin/` is embedded into the binary, so fetch **only** the
platform you are building for: a release job that publishes several platforms builds each one in its
own checkout, or removes the other payloads first - otherwise every artifact carries all of them.
The `cmd/fetchffmpeg` tool is a no-op while a payload is present - delete it to re-fetch.

## Configuration

Every value comes from the environment. A `.env` file is loaded only when `MERGE_ENV_FILE` is set,
and real environment variables always win over it:

> precedence: environment > `MERGE_ENV_FILE` file > default

### Configuration reference

| Variable | Default | Required | Purpose |
| --- | --- | --- | --- |
| `MERGE_LISTEN_ADDR` | `:8083` | No | HTTP listen address. |
| `MERGE_DSN` | - | `run`, `migrate` | PostgreSQL DSN; the pool sets `application_name=smartclass-merge` and pins `search_path` to the `smartclass_merge` schema (a DSN that sets `search_path` itself wins). |
| `MERGE_TEAMUSERS_URL` | `http://127.0.0.1:8082` | `run` (unless `MERGE_DEV=true`), `register-permissions` | teamusers base URL; JWKS is discovered under it. |
| `MERGE_TEAMUSERS_ISSUER` | `teamusers` | No | Expected `iss` claim. |
| `MERGE_TEAMUSERS_AUD` | `teamusers` | No | Expected `aud` claim - set it to the fleet value (`nekostick`). |
| `MERGE_TEAMUSERS_CLIENT_ID` | - | `run` (unless `MERGE_DEV=true`) | client-credentials credential id for outbound calls (permission lookups and the filehouse token). |
| `MERGE_TEAMUSERS_CLIENT_SECRET` | - | `run` (unless `MERGE_DEV=true`) | client-credentials secret. Treat as a secret. |
| `MERGE_TEAMUSERS_ADMIN_TOKEN` | - | `register-permissions` | teamusers admin bearer token; the `-token` flag wins. A secret. |
| `MERGE_TEAMUSERS_TIMEOUT` | `5s` | No | IAM HTTP timeout. |
| `MERGE_FILEHOUSE_URL` | - | `run` (unless `MERGE_DEV=true`) | nsc-filehouse base URL, e.g. `http://filewarehouse:8080`; the internal address, without a route prefix. |
| `MERGE_FILEHOUSE_TIMEOUT` | `30s` | No | Metadata HTTP timeout. Segment downloads and uploads run on the job's context instead, because they can take minutes. |
| `MERGE_SOURCE_BUCKET` | `webcam-segments` | No | Default source bucket; the bucket webcam-server writes segments to. |
| `MERGE_DEST_BUCKET` | - | `run` | Bucket the merged videos go to. |
| `MERGE_OBJECT_MAX_BYTES` | `5368709120` (5 GiB) | No | Largest merged file uploaded with a single PUT; above it, the upload switches to multipart. |
| `MERGE_MULTIPART_PART_BYTES` | `67108864` (64 MiB) | No | Size of one multipart part; at most `268435456` (256 MiB), which is what filehouse accepts. |
| `MERGE_DEFAULT_FPS` | `25` | No | Frame rate applied when a request omits `fps`; `1`–`240`. |
| `MERGE_WORK_DIR` | `<os temp dir>/smartclass-merge` | No | Root of the per-job temp directories and of the extracted ffmpeg. Persistent and writable, sized for the recordings. |
| `MERGE_FFMPEG_PATH` | - | No | An ffmpeg executable to use instead of the embedded build. |
| `MERGE_MAX_CONCURRENT_JOBS` | `1` | No | Merges this process runs at once. |
| `MERGE_SCHED_TICK` | `5s` | No | Scheduler scan interval. |
| `MERGE_JOB_LEASE` | `2m` | No | How long a claim stays valid without a heartbeat; expired claims are re-claimed. |
| `MERGE_MAX_ATTEMPTS` | `3` | No | Attempts per job before it is marked `failed`. |
| `MERGE_RETRY_BACKOFF` | `1m` | No | Base of the retry delay: attempt *n* waits `backoff × n`. |
| `MERGE_JOB_TIMEOUT` | `6h` | No | Wall-clock budget of one attempt. |
| `MERGE_TRANSCODE_FALLBACK` | `true` | No | Re-encode with libx264 when a copy merge fails. |
| `MERGE_DEV` | `false` | No | Dev bypass: synthetic claims, teamusers disabled. Local development only. |
| `MERGE_LOG_LEVEL` | `info` | No | `slog` level: `debug`, `info`, `warn` or `error`. |
| `MERGE_ENV_FILE` | - | No | Path to a `.env`-style file; loaded at startup only when set. |

`MERGE_TEAMUSERS_TIMEOUT`, `MERGE_FILEHOUSE_TIMEOUT`, `MERGE_SCHED_TICK`, `MERGE_JOB_LEASE`,
`MERGE_RETRY_BACKOFF` and `MERGE_JOB_TIMEOUT` accept Go duration syntax (`300ms`, `2m`, `1h`).
Loading fails fast on an unparsable duration or an unknown log level, and each subcommand then
enforces only the variables it uses - `run` the DSN, the destination bucket and (outside dev) the
filehouse endpoint and the teamusers credential, `migrate` the DSN, `register-permissions` the
teamusers URL and its admin token - so a bad compose edit surfaces at startup, not at the first
recording, and a one-off `migrate` is never blocked by the runtime endpoints.

## Fleet deployment (svchost)

The document below is the complete `svchost.compose.yaml` for the fleet; the same file is committed
at the repository root. Field semantics are defined in the
[svchost compose reference](https://github.com/crazy4chicken/nekostick-svchost/blob/main/docs/compose.md).

```yaml
# svchost compose for the SmartClass dispatch fleet.
# Field semantics: https://github.com/crazy4chicken/nekostick-svchost/blob/main/docs/compose.md
#
# strictSources is intentionally omitted (it defaults to false): a single
# source.sha256 cannot cover every architecture at once, because the per-arch
# ZIP assets have different digests. Pin sha256 only for a single-architecture
# deployment, or set strictSources: true together with a matching digest.
serviceScope: global

services:
  teamusers:
    source:
      release: "github:crazy4chicken/nsc-teamusers@v0.2.1" # asset teamusers_<version>_<arch>.zip, entry binary `teamusers`
      # sha256: "..." # optional single-arch pin; otherwise svchost verifies against the GitHub asset digest
    args: ["run"]
    env:
      TEAMUSERS_CONNECTION_STRING: "${HOST:TEAMUSERS_CONNECTION_STRING}"
      TEAMUSERS_KEY_DIR: /var/lib/teamusers/keys # persistent and writable; never point it at artifacts/ or tmp/
      TEAMUSERS_NODE_ID: teamusers
      TEAMUSERS_TOKEN_AUDIENCE: nekostick # MUST equal MERGE_TEAMUSERS_AUD and WEBCAM_TEAMUSERS_AUD
      TEAMUSERS_LOG_LEVEL: info
      # The listen address comes from the Host launch values; override with
      # TEAMUSERS_LISTEN_ADDRESS / TEAMUSERS_LISTEN_PORT if this deployment needs fixed values.
    start: eager # authentication is on the fleet's critical path, start with the host
    restart: on-failure
    health:
      type: http
      path: /healthz
      timeout: 5s
    route:
      prefix: /iam
      strip: true # /iam/auth/login reaches the child process as /auth/login

  filewarehouse:
    source:
      release: "github:crazy4chicken/nsc-filewarehouse@v0.1.0" # module nsc-filehouse; entry binary `filewarehouse`
      # sha256: "..." # optional single-arch pin
    args: ["run"]
    env:
      FILEHOUSE_DSN: "${HOST:FILEHOUSE_DSN}"
      FILEHOUSE_ADDR: "${HOST}"
      FILEHOUSE_PORT: "${PORT}"
      FILEHOUSE_BLOB_DIR: /var/lib/filehouse/blobs # persistent and writable; the segments and photos live here
      FILEHOUSE_KEY_DIR: /var/lib/filehouse/keys
      FILEHOUSE_NODE_ID: filewarehouse
      FILEHOUSE_PUBLIC_BASE_URL: "https://files.example.com/files" # public origin, route prefix included
      FILEHOUSE_TEAMUSERS_BASE_URL: "http://${HOST@teamusers}:${PORT@teamusers}"
      FILEHOUSE_TEAMUSERS_ISSUER: teamusers
      FILEHOUSE_TEAMUSERS_AUDIENCE: nekostick
      FILEHOUSE_TEAMUSERS_CLIENT_ID: filehouse-svc
      FILEHOUSE_TEAMUSERS_CLIENT_SECRET: "${HOST:FILEHOUSE_TEAMUSERS_CLIENT_SECRET}"
    start: eager # applies migrations at startup, before the first upload
    restart: on-failure
    health:
      type: http
      path: /healthz
      timeout: 5s
    route:
      prefix: /files
      strip: true

  webcam-server:
    source:
      release: "github:crazy4chicken/smartclass-webcam-server@v0.2.0"
    env:
      WEBCAM_LISTEN_ADDR: "${HOST}:${PORT}" # the Host assigns the port for this launch
      WEBCAM_DB_URL: "${HOST:WEBCAM_DB_URL}"
      WEBCAM_TEAMUSERS_URL: "http://${HOST@teamusers}:${PORT@teamusers}"
      WEBCAM_TEAMUSERS_AUD: nekostick
      WEBCAM_TEAMUSERS_CLIENT_ID: webcam-server-svc
      WEBCAM_TEAMUSERS_CLIENT_SECRET: "${HOST:WEBCAM_TEAMUSERS_CLIENT_SECRET}"
      WEBCAM_FILEHOUSE_URL: "http://${HOST@filewarehouse}:${PORT@filewarehouse}" # internal address, no route prefix; the media sink — omit it and webcam-server falls back to S3, then to noop (nothing is stored)
      WEBCAM_FILEHOUSE_BUCKET: webcam-segments
      WEBCAM_WS_TICKET_TTL: 60s
    start: eager
    restart: on-failure
    health:
      type: http
      path: /readyz # also covers the Postgres connection
      timeout: 5s
    route:
      prefix: /webcam
      strip: true # see the device-plane note below before publishing this prefix

  dispatchub:
    source:
      release: "github:crazy4chicken/smartclass-dispatchub@v0.1.0"
      # Before the first release exists, bootstrap from a local binary instead:
      # path: /opt/dispatchub/dispatchub
    env:
      DISPATCH_LISTEN_ADDR: "${HOST}:${PORT}"
      DISPATCH_DSN: "${HOST:DISPATCH_DSN}"
      DISPATCH_TIMEZONE: Asia/Shanghai
      DISPATCH_TEAMUSERS_URL: "http://${HOST@teamusers}:${PORT@teamusers}"
      DISPATCH_TEAMUSERS_ISSUER: teamusers
      DISPATCH_TEAMUSERS_AUD: nekostick
      DISPATCH_TEAMUSERS_CLIENT_ID: dispatchub-svc
      DISPATCH_TEAMUSERS_CLIENT_SECRET: "${HOST:DISPATCH_TEAMUSERS_CLIENT_SECRET}"
      DISPATCH_WEBCAM_URL: "http://${HOST@webcam-server}:${PORT@webcam-server}" # same-namespace service reference
      DISPATCH_PRESTART: 2m
      DISPATCH_POSTSTOP_GRACE: 15s
      DISPATCH_LOG_LEVEL: info
    start: eager # the scheduler must be up before the first lesson window
    restart: on-failure # fatal errors exit non-zero; use `always` if a clean exit should also be restarted
    health:
      type: http
      path: /healthz
      timeout: 5s
    route:
      prefix: /dispatch
      strip: true # /dispatch/api/v1/... reaches the child process as /api/v1/...

  merge:
    source:
      release: "github:crazy4chicken/smartclass-merge@v0.1.0"
      # Before the first release exists, bootstrap from a local binary instead:
      # path: /opt/merge/merge
    args: ["run"]
    env:
      MERGE_LISTEN_ADDR: "${HOST}:${PORT}"
      MERGE_DSN: "${HOST:MERGE_DSN}"
      MERGE_TEAMUSERS_URL: "http://${HOST@teamusers}:${PORT@teamusers}"
      MERGE_TEAMUSERS_ISSUER: teamusers
      MERGE_TEAMUSERS_AUD: nekostick # MUST equal TEAMUSERS_TOKEN_AUDIENCE
      MERGE_TEAMUSERS_CLIENT_ID: merge-svc
      MERGE_TEAMUSERS_CLIENT_SECRET: "${HOST:MERGE_TEAMUSERS_CLIENT_SECRET}"
      MERGE_FILEHOUSE_URL: "http://${HOST@filewarehouse}:${PORT@filewarehouse}" # internal address, no route prefix
      MERGE_SOURCE_BUCKET: webcam-segments
      MERGE_DEST_BUCKET: recordings # must exist in filewarehouse; the merged video is written here
      MERGE_WORK_DIR: /var/lib/merge/work # persistent and writable; holds each job's temp video, size it for one recording per concurrent job
      MERGE_MAX_CONCURRENT_JOBS: "1" # one merge at a time; raise it only with CPU and disk headroom
      MERGE_LOG_LEVEL: info
      # MERGE_FFMPEG_PATH: /usr/bin/ffmpeg # uncomment to use a host ffmpeg instead of the embedded build
    start: eager # the queue should drain as soon as a recording stops producing segments
    restart: on-failure # fatal errors exit non-zero; use `always` if a clean exit should also be restarted
    health:
      type: http
      path: /readyz # covers Postgres, the work dir and the ffmpeg extraction; the first probe extracts the payload
      timeout: 15s
    route:
      prefix: /merge
      strip: true # /merge/api/v1/... reaches the child process as /api/v1/...
```

Before applying it to a real host, replace the example filehouse public origin
(`https://files.example.com/files`) and the release versions if you pin a different one - the merge
release tag in particular, because it carries the ffmpeg payload for that platform.

### Template walkthrough

svchost resolves three template forms inside `env` values:

| Template | Meaning |
| --- | --- |
| `${HOST:NAME}` | A **host value** stored on the svchost Host: every secret and every DSN is passed through this form. Nothing is stored in the compose file. |
| `${HOST}` / `${PORT}` | The address and port the Host leased to **this** launch. `MERGE_LISTEN_ADDR` binds to them. |
| `${HOST@service}` / `${PORT@service}` | The address and port assigned to **another service in the same namespace**, e.g. `http://${HOST@teamusers}:${PORT@teamusers}`. This is why no static port appears anywhere in the fleet. |

### Notes

- **No secret ever lives in the YAML.** Every credential is a `${HOST:...}` passthrough and every
  DSN comes from the host environment, so the file is committed as-is and the values stay in the
  Host.
- **The service name is the release contract.** `services.merge` is the `serviceId`, so the release
  workflow must publish `merge_<version>_<platform>_<arch>.zip` with the entry binary named exactly
  `merge` - `merge.exe` on Windows - at the ZIP root. The platform segment is mandatory, and the
  process CWD will be the service root rather than the extraction directory.
- **The work directory is real state.** `MERGE_WORK_DIR` holds the extracted ffmpeg and each job's
  temp video; it must survive a restart (the ffmpeg extraction is cached there) and have room for
  the largest recording. Never point it at `artifacts/` or `tmp/`, and never share one work
  directory between two merge launches that run concurrently.
- **The health probe pays for the extraction.** `GET /readyz` resolves ffmpeg, so the **first**
  probe extracts the embedded payload into `MERGE_WORK_DIR/ffmpeg/` (a few hundred milliseconds
  for ~100 MiB). That is why the compose uses `timeout: 15s`; afterwards the probe is cheap.
- **Merge runs one job at a time by default.** `MERGE_MAX_CONCURRENT_JOBS: "1"` keeps CPU and disk
  predictable; raise it only with headroom for one more encoding and one more temp file.
- **The queue is shared through PostgreSQL, not the network.** Every merge process watches the same
  `merge_jobs` table and claims with `FOR UPDATE SKIP LOCKED`, so a second replica does not double
  work - but it does need its own work directory.
- **One digest, one architecture.** `source.sha256` holds a single value, so it cannot match both
  the x64 and arm64 ZIPs; pin it for single-architecture deployments, or set `strictSources: true`
  together with the matching digest. Each merge asset also carries a different ffmpeg payload, so
  its digest differs per architecture by construction.
- **A dangling `${...@svc}` reference fails that service's sync.** Merge references `teamusers` and
  `filewarehouse`; dropping either block means dropping the corresponding variable.
- **Health paths differ in meaning.** `/healthz` is the liveness check the supervisor should use;
  `/readyz` additionally probes Postgres, the work dir and the ffmpeg extraction, so it belongs on
  the operator dashboard.
- **Scope choice.** `serviceScope: global` (used above) shares one service root and one service-name
  namespace across configs - a duplicate service name makes the whole reconciliation round fail -
  while `serviceScope: document` isolates roots per config name.

### Field notes

`merge`

| Field | Note |
| --- | --- |
| `source.release` | `merge_<version>_<platform>_<arch>.zip`, binary `merge`; the commented `path:` is the bootstrap route before the first release exists. |
| `MERGE_LISTEN_ADDR` | `${HOST}:${PORT}` - bind to the launch lease. |
| `MERGE_DSN` | The merge database DSN; migrations are applied from the embedded FS at startup. |
| `MERGE_TEAMUSERS_URL` | Internal teamusers address; JWKS is discovered under it. |
| `MERGE_TEAMUSERS_ISSUER` / `..._AUD` | `teamusers` / `nekostick`; the audience must match `TEAMUSERS_TOKEN_AUDIENCE`. |
| `MERGE_TEAMUSERS_CLIENT_ID` / `..._SECRET` | merge's service credential; the secret comes from the Host. It is the token merge presents to filehouse, so filehouse must accept this teamusers issuer. |
| `MERGE_FILEHOUSE_URL` | Same-namespace service reference to filewarehouse; both buckets must exist there. |
| `MERGE_SOURCE_BUCKET` / `MERGE_DEST_BUCKET` | Read segments from the first, write merged videos to the second. |
| `MERGE_WORK_DIR` | Persistent and writable, sized for one recording per concurrent job. |
| `MERGE_MAX_CONCURRENT_JOBS` | `1` unless the host has CPU and disk headroom. |
| `MERGE_FFMPEG_PATH` | Commented out: the embedded build is the default. A host ffmpeg is the escape hatch when shipping a GPL binary is not acceptable. |
| `start` / `restart` | `eager` (the queue should drain as soon as a recording stops) / `on-failure` (fatal errors exit non-zero; use `always` to also restart clean exits). |
| `health` | `GET /readyz` with a 15 s timeout, because the first probe extracts the ffmpeg payload. |
| `route` | `prefix: /merge`, `strip: true`: `/merge/api/v1/...` reaches the child as `/api/v1/...`. |

The other services are unchanged from the dispatch fleet; see the
[dispatchub deployment guide](https://github.com/crazy4chicken/smartclass-dispatchub/blob/main/docs/guide/deploy.md)
for their field notes.

## First-time setup

1. **Store the host values** referenced by the compose file: `TEAMUSERS_CONNECTION_STRING`,
   `FILEHOUSE_DSN`, `WEBCAM_DB_URL`, `DISPATCH_DSN`, `MERGE_DSN` and the three service-client
   secrets (`FILEHOUSE_TEAMUSERS_CLIENT_SECRET`, `WEBCAM_TEAMUSERS_CLIENT_SECRET`,
   `MERGE_TEAMUSERS_CLIENT_SECRET`).
2. **Create the database schema** (or let merge do it): `merge migrate` creates the
   `smartclass_merge` schema and applies the embedded migrations.
3. **Create the buckets** in filehouse: the source bucket webcam-server writes to
   (`webcam-segments`) and the destination bucket (`recordings`).
4. **Register the six `merge:*` keys** in teamusers and bind them to roles: either run
   `merge register-permissions -token <admin token>` against the instance or use the raw
   registration calls in the teamusers API.
5. **Create the service credentials** (`merge-svc`, plus `filehouse-svc`, `webcam-server-svc` and
   `dispatchub-svc` if you deploy the whole fleet) in teamusers and place their secrets in the Host
   values.
6. **Give the merge credential access to the buckets**: filehouse authorizes the merge service
   token by its own policy, so the account behind `merge-svc` must be allowed to read the source
   bucket and write the destination bucket.
7. **Grant the two audiences their keys**: human callers need `merge:read:*` and `merge:write:*`
   (at whatever scope their role covers), while merge's own service credential needs no `merge:*`
   key at all - it is used for outbound calls, and it authenticates those against the issuer.
8. **Apply the compose document** and verify: `GET /merge/readyz` answers 200 through the host
   route, and `merge doctor` reports a reachable database, a writable work dir, a resolvable ffmpeg
   (with its version and the required demuxers) and reachable endpoints.

## Operations

| Command | Use |
| --- | --- |
| `merge migrate` | Apply the embedded migrations and exit. Safe to run on every deploy; goose records what is applied. |
| `merge register-permissions -token <t>` | Upsert the six `merge:*` keys in teamusers. Idempotent. |
| `merge status` | Print the redacted configuration and the dependency reachability; always exits 0 once the configuration loaded. |
| `merge doctor` | Same checks with a 30 s timeout, and exit non-zero on failure. Suitable as a deployment gate. |
| `merge run` | Serve the API and run the queue. |

`doctor` checks, in order: the effective configuration (secrets masked), the PostgreSQL connection
and server version, that `MERGE_WORK_DIR` is writable, that ffmpeg resolves (`MERGE_FFMPEG_PATH` or
the extracted embedded build) and reports its version, that the build provides every demuxer the
[codec table](/guide/architecture#codec-table) needs (`h264`, `hevc`, `mjpeg`, `m4v`, `av1`) -
listing the missing ones - and that the filehouse and teamusers endpoints answer. A dependency that
is not configured at all is reported as `SKIP`, not `FAIL`; a missing required demuxer is a `FAIL`
and exits non-zero, because that build cannot merge the recordings it advertises.

## Health and diagnostics

| Probe | Meaning |
| --- | --- |
| `GET /healthz` | Liveness: 200 while the process lives. This is the supervisor's probe. |
| `GET /readyz` | Readiness: Postgres ping plus the work dir and ffmpeg resolution. Use it on the operator dashboard. |
| `merge status` | Prints the redacted configuration and the dependency reachability, then exits. |
| `merge doctor` | Runs the same checks as `status` plus the ffmpeg demuxer set, with a 30 s timeout, and exits non-zero on failure. |

Security notes:

- `MERGE_DSN`, `MERGE_TEAMUSERS_CLIENT_SECRET` and `MERGE_TEAMUSERS_ADMIN_TOKEN` are secrets. Never
  commit them, paste them into an issue, or print them; `status` replaces secret values with
  `<redacted>`.
- `MERGE_DEV=true` disables teamusers entirely. Never set it on a host that is reachable from a
  network you do not control.
- The embedded ffmpeg is a GPL binary; redistribution of the artifact is subject to the GPL. See
  the licensing note in the [README](https://github.com/crazy4chicken/smartclass-merge#readme).
