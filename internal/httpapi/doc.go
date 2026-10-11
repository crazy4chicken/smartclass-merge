// Package httpapi exposes smartclass-merge's HTTP API over the chi router
// built by New.
//
// # Routes
//
// Public endpoints:
//
//	GET /healthz  liveness probe
//	GET /readyz   readiness probe (PostgreSQL ping + work dir + ffmpeg)
//
// Endpoints under /api/v1 require a teamusers Bearer token carrying a
// merge:read or merge:write permission scoped to own, team or any (401 without
// verified claims, 403 insufficient_permissions when denied, 503
// iam_unavailable when teamusers cannot answer). Job and collection routes
// resolve their resource first and try the any, team and own rungs in that
// order:
//
//	POST /api/v1/merges             create a merge job
//	GET  /api/v1/merges             list merge jobs
//	GET  /api/v1/merges/{id}        fetch one merge job
//	POST /api/v1/merges/{id}/cancel cancel a queued or running job
//
// # Responses
//
// Collection endpoints return {"items": [...], "next_cursor": "..."}.
// Single-resource endpoints return the job object itself. Errors use RFC 9457
// problem details (application/problem+json) whose detail member is one of the
// stable error codes; the missing-claims 401 emitted by teamusers uses the
// SDK's own {"allow": false, "reason": ...} shape instead.
//
// DocOperations lists every route together with its request and response
// shapes. cmd/genspec walks the router and reflects the table into
// docs/public/openapi.yaml, failing when a route and its metadata drift apart.
package httpapi

import (
	"net/http"

	apidocs "github.com/crazy4chicken/nsc-teamusers/apidocs/go"

	"github.com/crazy4chicken/smartclass-merge/internal/domain"
)

// DocPermissionDeriver returns the merge:<action>:any and merge:<action>:team
// keys that guard method and path for the generated reference. The own-scoped
// rung depends on the resolved job's owner_id, so it cannot be derived
// statically and is documented in the guide and permission notes only.
func DocPermissionDeriver(method, path string) (anyKey, teamKey string) {
	var action string
	switch path {
	case "/api/v1/merges":
		if method == http.MethodPost {
			action = "write"
		} else {
			action = "read"
		}
	case "/api/v1/merges/{id}":
		action = "read"
	case "/api/v1/merges/{id}/cancel":
		action = "write"
	default:
		// The public health probes carry no permission.
		return "", ""
	}
	return "merge:" + action + ":any", "merge:" + action + ":team"
}

// docError builds one documented problem response. The code is rendered as the
// detail of the response example, matching the problem body handlers emit.
func docError(status int, code, title string) apidocs.ErrorDoc {
	return apidocs.ErrorDoc{Status: status, Code: code, Title: title}
}

var (
	docUnauthorized        = docError(401, "authentication is required", "Unauthorized")
	docForbidden           = docError(403, "insufficient_permissions", "Forbidden")
	docInvalidRequest      = docError(400, "invalid_request", "Invalid Request")
	docJobNotFound         = docError(404, "job_not_found", "Not Found")
	docStatusNotCancelable = docError(409, "status_not_cancelable", "Conflict")
	docSourceUnavailable   = docError(503, "source_unavailable", "Service Unavailable")
	docIAMUnavailable      = docError(503, "iam_unavailable", "Service Unavailable")
	docInternalError       = docError(500, "internal_error", "Internal Server Error")
)

// permissionNoteJob documents the dynamic own rung of the job routes.
const permissionNoteJob = "own-scoped access additionally requires the corresponding merge:*:own key and the job's owner_id equal to the token subject."

// permissionNoteCreate documents the create-versus-existing split of the write
// permissions: a job that does not exist yet has no owner to match.
const permissionNoteCreate = "creating a job requires merge:write:any or merge:write:team; the own rung applies to existing jobs, so merge:write:own alone can cancel but not create."

// permissionNoteCollection documents how the collection route narrows rows to
// the rung that granted it.
const permissionNoteCollection = "merge:read:any lists every job; a caller granted only merge:read:team or merge:read:own also sees the list, narrowed to its team's jobs or to the jobs it created."

var (
	mergeSegmentExample = map[string]any{
		"key":        "01J8Z4W3K5M7Q9R1T3V5X7Z9B1/streams/01J8ZF4STR5Q7T9V1X3Z5B7D9F1/20261004T100105_42.bin",
		"seq":        42,
		"size_bytes": 1048576,
	}
	mergeJobExample = map[string]any{
		"id":         "01J8ZM3MRG0A2B4C6D8E0F2G4H",
		"status":     "running",
		"stage":      "merging",
		"owner_id":   "01J8Z1USER3Y5W7A9C1E3G5J7L9",
		"owner_kind": "user",
		"team_id":    "01J8Z2TEAM5A7C9E1G3J5L7N9P1",
		"source": map[string]any{
			"bucket":   "webcam-segments",
			"prefix":   "01J8Z4W3K5M7Q9R1T3V5X7Z9B1/streams/01J8ZF4STR5Q7T9V1X3Z5B7D9F1/",
			"segments": []any{mergeSegmentExample},
			"fps":      30,
			"codec":    "h264",
		},
		"dest": map[string]any{
			"bucket":     "recordings",
			"key":        "merged/2026/10/11/01J8ZM3MRG0A2B4C6D8E0F2G4H.mp4",
			"size_bytes": 0,
			"sha256":     "",
		},
		"attempts":     1,
		"max_attempts": 3,
		"progress": map[string]any{
			"input_bytes":        52428800,
			"input_total":        104857600,
			"output_bytes":       41943040,
			"uploaded_bytes":     0,
			"output_duration_ms": 240000,
			"frames":             7200,
		},
		"error":       "",
		"created_at":  "2026-10-11T09:00:00Z",
		"updated_at":  "2026-10-11T09:00:05Z",
		"started_at":  "2026-10-11T09:00:02Z",
		"finished_at": nil,
	}
	mergeListExample = map[string]any{
		"items":       []any{mergeJobExample},
		"next_cursor": "MjAyNi0xMC0xMVQwOTowMDowMFoAMDFKOFpNM01SRzBBMkI0QzZEOEUwRjJHNEg",
	}
	mergeCreateDeviceExample = map[string]any{
		"device_id": "01J8Z4W3K5M7Q9R1T3V5X7Z9B1",
		"stream_id": "01J8ZF4STR5Q7T9V1X3Z5B7D9F1",
		"fps":       30,
		"codec":     "h264",
		"dest": map[string]any{
			"bucket": "recordings",
			"key":    "merged/2026/10/11/lecture.mp4",
		},
	}
)

// DocOperations is the route contract used by the OpenAPI generator. Every
// route served by New must appear here exactly once, with its chi pattern, so
// cmd/genspec fails instead of publishing a stale reference. The emitter cannot
// express query parameters, the 201/503 statuses or the X-Idempotency-Key
// header, so those live in the operation descriptions.
var DocOperations = []apidocs.Operation{
	{
		Method:          "GET",
		Path:            "/healthz",
		Tag:             "Health",
		Summary:         "Check liveness",
		Description:     "Use this unauthenticated probe to decide whether the HTTP process is alive. It never touches PostgreSQL, filehouse or the embedded ffmpeg, so it keeps answering ok during an outage and is safe to call on a short interval.",
		Response:        statusResponse{},
		ResponseExample: map[string]string{"status": "ok"},
		Errors:          []apidocs.ErrorDoc{},
	},
	{
		Method:          "GET",
		Path:            "/readyz",
		Tag:             "Health",
		Summary:         "Check readiness",
		Description:     "Use this unauthenticated probe on the operator dashboard. It pings PostgreSQL, creates and removes a scratch file in MERGE_WORK_DIR and resolves the embedded ffmpeg binary, all within 5 seconds; the probe answers 200 with {\"status\":\"ready\"} when every check passes and 503 with {\"status\":\"unavailable\"} otherwise, logging the failing dependencies instead of returning them.",
		Response:        statusResponse{},
		ResponseExample: map[string]string{"status": "ready"},
		Errors:          []apidocs.ErrorDoc{},
	},

	{
		Method:          "POST",
		Path:            "/api/v1/merges",
		Tag:             "Merges",
		Summary:         "Create a merge job",
		Description:     "Use to merge the segments of one recording into a single video. The body selects the source in exactly one of two ways: an explicit segments array, each entry carrying the filehouse key and optionally seq and size_bytes, or device_id together with stream_id, in which case the service lists {device_id}/streams/{stream_id}/ from filehouse and orders the objects by the flush timestamp and first frame sequence number parsed from their key names. When every explicit segment carries seq the array is sorted by it, otherwise the given order is kept; a repeated key is rejected. bucket defaults to MERGE_SOURCE_BUCKET, dest.bucket to MERGE_DEST_BUCKET and dest.key to `merged/YYYY/MM/DD/<id><ext>`, where the extension comes from the codec and is .mp4 when the body names none. fps defaults to MERGE_DEFAULT_FPS (25) and must be 1..240; codec is optional and must be one of h264, h265, mjpeg, mpeg4, av1 — the recorder's vp8 and vp9 streams are raw elementary streams without an ffmpeg demuxer and are rejected. Unknown body members answer 400 invalid_request. The request answers 201 Created with the stored job, a Location header with its id and, when the job's stream cannot be listed, 503 source_unavailable. Send X-Idempotency-Key to make the call replayable: a replayed key answers the same 201 with Idempotency-Replayed: true and returns the job created first instead of starting a second merge.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteCreate,
		Request:         createMergeRequest{},
		RequestExample:  mergeCreateDeviceExample,
		Response:        domain.Job{},
		ResponseExample: mergeJobExample,
		Errors: []apidocs.ErrorDoc{
			docInvalidRequest,
			docUnauthorized,
			docForbidden,
			docSourceUnavailable,
			docIAMUnavailable,
			docInternalError,
		},
	},
	{
		Method:          "GET",
		Path:            "/api/v1/merges",
		Tag:             "Merges",
		Summary:         "List merge jobs",
		Description:     "Use to browse merge jobs, newest first. Query parameters: limit (default 100, clamped to 1000; a non-positive or non-numeric value answers 400 invalid_request), cursor (the next_cursor of the previous page, which keeps paging from repeating or skipping a job) and status (one of queued, running, succeeded, failed, canceled). The response is `{\"items\": [<job>], \"next_cursor\": \"<cursor>\"}`, with an empty next_cursor on the last page. " + permissionNoteCollection,
		Security:        "bearerAuth",
		Response:        listResponse[domain.Job]{},
		ResponseExample: mergeListExample,
		Errors:          []apidocs.ErrorDoc{docInvalidRequest, docUnauthorized, docForbidden, docInternalError},
	},
	{
		Method:          "GET",
		Path:            "/api/v1/merges/{id}",
		Tag:             "Merges",
		Summary:         "Get a merge job",
		Description:     "Use to follow one merge job: the frozen source segments, the destination, the attempt counter (attempts of max_attempts) and the progress counters a running worker reports — input_bytes of input_total, output_bytes, uploaded_bytes, output_duration_ms and frames. error carries the last failure of a retried job and finished_at is set once the job is terminal. An unknown id answers 404 job_not_found.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteJob,
		Response:        domain.Job{},
		ResponseExample: mergeJobExample,
		Errors:          []apidocs.ErrorDoc{docUnauthorized, docForbidden, docJobNotFound, docInternalError},
	},
	{
		Method:          "POST",
		Path:            "/api/v1/merges/{id}/cancel",
		Tag:             "Merges",
		Summary:         "Cancel a merge job",
		Description:     "Use to stop a queued or running merge. The store moves the job to canceled first, which also aborts the attempt the scheduler is running in this process, and an already canceled job is returned unchanged so the call is idempotent. A job that already succeeded or failed answers 409 status_not_cancelable and an unknown id answers 404 job_not_found. The response is 200 with the stored job, whose finished_at is now set.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteJob,
		Response:        domain.Job{},
		ResponseExample: mergeJobExample,
		Errors:          []apidocs.ErrorDoc{docUnauthorized, docForbidden, docJobNotFound, docStatusNotCancelable, docInternalError},
	},
}
