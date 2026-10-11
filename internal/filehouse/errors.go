package filehouse

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Sentinels of the failure classes this service reacts to. A call that filehouse
// answered with the matching RFC 9457 detail unwraps to one of them, so callers
// classify with errors.Is instead of comparing detail strings.
var (
	// ErrObjectNotFound reports a missing object (404 object_not_found).
	ErrObjectNotFound = errors.New("filehouse: object not found")
	// ErrUploadExpired reports a multipart upload past its lifetime (410
	// upload_expired); nothing can be staged or completed for it any more.
	ErrUploadExpired = errors.New("filehouse: upload expired")
	// ErrQuotaExceeded reports a write rejected by a bucket, owner or team
	// quota (413 quota_exceeded).
	ErrQuotaExceeded = errors.New("filehouse: quota exceeded")
	// ErrPayloadTooLarge reports a write above a size cap (413
	// payload_too_large): the object cap, a part cap or a declared cap.
	ErrPayloadTooLarge = errors.New("filehouse: payload too large")
)

// problem is the wire form of an RFC 9457 problem document, the shape of every
// filehouse failure.
type problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
	Reason   string `json:"reason"`
}

// Error is a non-2xx response from filehouse. Status is the HTTP status, Detail
// the stable RFC 9457 error code, Reason the extra member of authorization
// failures and Body a bounded copy of the response body for logs.
//
// Error unwraps to the package sentinel of its detail code; codes without one
// (invalid_token*, insufficient_permissions, upload_not_found, part_mismatch,
// ...) stay typed as *Error for errors.As.
type Error struct {
	Status int
	Detail string
	Reason string
	Body   string
}

// Error renders the upstream failure: the status and, when the body was not a
// problem document, an excerpt of it.
func (e *Error) Error() string {
	detail := e.Detail
	if detail == "" {
		detail = e.Body
	}
	if detail == "" {
		detail = http.StatusText(e.Status)
	}
	return fmt.Sprintf("filehouse: HTTP %d: %s", e.Status, detail)
}

// Unwrap maps the detail code onto the package sentinel, or nil for a code
// without one.
func (e *Error) Unwrap() error {
	switch e.Detail {
	case "object_not_found":
		return ErrObjectNotFound
	case "upload_expired":
		return ErrUploadExpired
	case "quota_exceeded":
		return ErrQuotaExceeded
	case "payload_too_large":
		return ErrPayloadTooLarge
	}
	return nil
}

// IsStatus reports whether err carries a filehouse response with the given HTTP
// status. It unwraps through caller-defined error types, so an engine error
// wrapping an *Error still answers.
func IsStatus(err error, status int) bool {
	var failure *Error
	return errors.As(err, &failure) && failure.Status == status
}

// DetailOf returns the RFC 9457 detail of err, or "" when err carries no
// filehouse problem document.
func DetailOf(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Detail
	}
	return ""
}

// newError builds the typed error of a non-2xx response, decoding the problem
// document when the body is one and keeping a bounded excerpt either way.
func newError(status int, payload []byte) *Error {
	failure := &Error{Status: status, Body: truncate(string(payload), maxErrorBodyBytes)}
	var doc problem
	if err := json.Unmarshal(payload, &doc); err == nil {
		failure.Detail = doc.Detail
		failure.Reason = doc.Reason
	}
	return failure
}

// truncate caps text at limit bytes, marking a cut so a clipped body is not
// mistaken for the whole one.
func truncate(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + " [truncated]"
}
