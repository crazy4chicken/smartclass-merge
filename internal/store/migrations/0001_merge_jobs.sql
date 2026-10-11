-- +goose Up
-- store.Migrate creates the schema before goose runs, because goose creates its
-- version table first and PostgreSQL cannot put objects in a schema that does
-- not exist yet. This line keeps the file runnable by hand against a fresh
-- database.
CREATE SCHEMA IF NOT EXISTS smartclass_merge;

CREATE TABLE merge_jobs (
  id                      TEXT PRIMARY KEY,                   -- ULID
  owner_id                TEXT NOT NULL,                      -- claims.subject of the requester
  owner_kind              TEXT NOT NULL,                      -- user | service
  team_id                 TEXT NOT NULL DEFAULT '',           -- team of the requester, '' when none
  status                  TEXT NOT NULL,                      -- queued | running | succeeded | failed | canceled
  stage                   TEXT NOT NULL DEFAULT '',           -- '' | merging | uploading
  source_bucket           TEXT NOT NULL,
  source_prefix           TEXT NOT NULL DEFAULT '',           -- device_id/streams/stream_id/ when the service listed
  source_segments         JSONB NOT NULL,                     -- [{key,seq,size_bytes}] in merge order
  source_fps              INT NOT NULL,                       -- declared frame rate, the bitstream carries none
  source_codec            TEXT NOT NULL DEFAULT '',           -- requested or sniffed: h264|h265|mjpeg|mpeg4|av1
  dest_bucket             TEXT NOT NULL,
  dest_key                TEXT NOT NULL,
  dest_size_bytes         BIGINT NOT NULL DEFAULT 0,          -- uploaded object, filled on success
  dest_sha256             TEXT NOT NULL DEFAULT '',
  attempts                INT NOT NULL DEFAULT 0,             -- incremented at claim
  max_attempts            INT NOT NULL,
  next_attempt_at         TIMESTAMPTZ NOT NULL DEFAULT now(), -- due time; claim picks the earliest
  lease_owner             TEXT NOT NULL DEFAULT '',           -- scheduler instance processing the job
  lease_expires_at        TIMESTAMPTZ,                        -- expired leases are claimable again
  progress_input_bytes    BIGINT NOT NULL DEFAULT 0,
  progress_input_total    BIGINT NOT NULL DEFAULT 0,
  progress_output_bytes   BIGINT NOT NULL DEFAULT 0,
  progress_uploaded_bytes BIGINT NOT NULL DEFAULT 0,
  output_duration_ms      BIGINT NOT NULL DEFAULT 0,
  output_frames           BIGINT NOT NULL DEFAULT 0,
  output_codec            TEXT NOT NULL DEFAULT '',           -- codec actually written (copy or transcode)
  last_error              TEXT NOT NULL DEFAULT '',
  idempotency_key         TEXT NOT NULL DEFAULT '',           -- X-Idempotency-Key, '' when absent
  created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at              TIMESTAMPTZ,                        -- first claim
  finished_at             TIMESTAMPTZ                         -- succeeded, failed or canceled
);

-- One job per (owner, idempotency key): a retried create replays the stored job
-- instead of starting a second merge. An absent key never conflicts.
CREATE UNIQUE INDEX merge_jobs_idempotency ON merge_jobs (owner_id, idempotency_key) WHERE idempotency_key <> '';

-- The claim scan: which job is due next.
CREATE INDEX merge_jobs_claim ON merge_jobs (status, next_attempt_at);

-- +goose Down
DROP TABLE IF EXISTS merge_jobs;
