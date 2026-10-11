---
layout: home

hero:
  name: SmartClass Merge
  text: One recording's segments, one playable video
  tagline: Merges the segment objects a recorder wrote to nsc-filehouse into a single MP4 in the bucket you choose, with ffmpeg bundled into the release and every job's status, progress and retries tracked in PostgreSQL.
  actions:
    - theme: brand
      text: Get Started
      link: /guide/deploy
    - theme: alt
      text: API Reference
      link: /api/overview

features:
  - title: One job, one video
    details: A job names a recording - its segments, or the recorder's device_id and stream_id - and produces exactly one object in the destination bucket, at a key you choose.
  - title: Segments straight from filehouse
    details: The segments stay where the recorder wrote them. Merge lists a device/stream prefix itself, orders the objects by flush time and streams each one through, so nothing has to be re-uploaded first.
  - title: ffmpeg included
    details: The release carries its own ffmpeg build, so the host needs no system ffmpeg and no codec packages. MERGE_FFMPEG_PATH points merge at a host ffmpeg instead.
  - title: Durable jobs with visible status
    details: A job survives restarts and reports whether it is merging or uploading, how far it got, and how many attempts it used; a failed attempt is retried before the job ends in a visible failed state.
  - title: Scoped teamusers permissions
    details: Sign-in and grants stay in teamusers. merge:read and merge:write are resolved on every request through the any > team > own ladder, and a list only ever shows the jobs the caller's scope covers.
  - title: Verified uploads
    details: The merged video is hashed while it is written and the stored object is checked against that digest, so a job is only marked succeeded once filehouse confirms the upload.
---

## What it is

smartclass-merge is the media step between a finished recording and the video a person watches:

- **Recording.** [smartclass-webcam-server](https://github.com/crazy4chicken/smartclass-webcam-server)
  writes a lesson's stream into the source bucket as many small segment objects.
- **Merging.** A merge job names that recording (or its `device_id`/`stream_id`, which merge lists
  itself); the segments are assembled, in playback order, through the bundled ffmpeg into a single
  file, which is uploaded to the destination bucket and verified against its own digest.
- **Playback.** The merged video lives in filehouse with an ordinary object key; players and
  archive jobs fetch it from there, and merge never proxies the bytes.

Authorization is delegated the same way across the fleet: tokens come from
[teamusers](https://github.com/crazy4chicken/nsc-teamusers), and merge keeps no local accounts.

## Quick links

- [Deployment](/guide/deploy) - prerequisites, the full `MERGE_*` reference and the fleet compose file.
- [API Usage](/guide/api) - create a job, follow it, cancel it; permissions and errors.
- [Architecture](/guide/architecture) - the merge pipeline, the job state machine and the codec table.
- [Development](/guide/development) - running merge locally against a dev database.
- [API Overview](/api/overview) - base URL, authentication, the permission ladder and the error codes.
- [Merges reference](/api/reference/merges) - the generated reference for the merge-job endpoints.
