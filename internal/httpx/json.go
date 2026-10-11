// Package httpx holds the transport-level helpers shared by every HTTP handler:
// JSON encoding, size-capped request decoding and RFC 9457 problem documents.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// MaxBodyBytes caps decoded JSON request bodies at 1 MiB.
const MaxBodyBytes int64 = 1 << 20

// WriteJSON marshals v and writes it with the given status code. A value that
// cannot be marshalled (a programming error) produces a problem document
// instead of a truncated body.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		WriteProblem(w, nil, http.StatusInternalServerError, "internal_error")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// DecodeJSON decodes a size-limited JSON request body into v. Unknown fields are
// allowed (DisallowUnknownFields is off); a body over MaxBodyBytes wraps
// http.MaxBytesError so callers can answer 413.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return fmt.Errorf("request body exceeds %d bytes: %w", MaxBodyBytes, err)
		}
		return fmt.Errorf("invalid json body: %w", err)
	}
	return nil
}

// DecodeJSONStrict decodes a size-limited JSON request body into v and rejects
// unknown members, so a typo in a create body is a client error instead of a
// silently ignored field. A body over MaxBodyBytes wraps http.MaxBytesError so
// callers can answer 413.
func DecodeJSONStrict(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return fmt.Errorf("request body exceeds %d bytes: %w", MaxBodyBytes, err)
		}
		return fmt.Errorf("invalid json body: %w", err)
	}
	return nil
}
