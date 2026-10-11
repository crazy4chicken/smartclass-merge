package httpx

import (
	"encoding/json"
	"net/http"
)

// ProblemContentType is the media type of RFC 9457 problem documents.
const ProblemContentType = "application/problem+json; charset=utf-8"

// problem is the wire form of an RFC 9457 problem document. detail carries the
// stable error code from the API contract, instance the request path and reason
// the extra member used by authorization failures.
type problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
	Reason   string `json:"reason,omitempty"`
}

// WriteProblem writes an RFC 9457 problem document: type "about:blank", the
// status text as title, detail as the stable error code and instance as the
// request path.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	writeProblem(w, r, status, detail, "")
}

// WriteProblemReason writes a problem document with an extra "reason" member.
// Handlers use it for authorization failures: detail stays the stable error
// code while reason names the attempted permission keys. It never carries
// credentials.
func WriteProblemReason(w http.ResponseWriter, r *http.Request, status int, detail, reason string) {
	writeProblem(w, r, status, detail, reason)
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail, reason string) {
	body, err := json.Marshal(problem{
		Type:     "about:blank",
		Title:    http.StatusText(status),
		Status:   status,
		Detail:   detail,
		Instance: requestPath(r),
		Reason:   reason,
	})
	if err != nil {
		// Marshalling strings and an int cannot fail; keep the response
		// well-formed if that ever changes.
		status = http.StatusInternalServerError
		body = []byte(`{"type":"about:blank","title":"Internal Server Error","status":500,"detail":"internal_error","instance":""}`)
	}
	header := w.Header()
	header.Set("Content-Type", ProblemContentType)
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r != nil && r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// requestPath returns the request path used as the problem instance, or "" when
// there is no request (internal failures and write-after-marshal-error paths).
func requestPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return r.URL.Path
}
