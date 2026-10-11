// Package filehouse is the typed client for nsc-filehouse, the object store that
// holds the recording segments this service merges and receives the merged
// videos. It covers the routes frozen in contract §5.3 — object listing,
// downloads, single PUT uploads and multipart uploads — and turns every failure
// into either an *Error carrying the RFC 9457 detail or one of the package
// sentinels.
package filehouse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// listPageLimit is the page size ListObjects asks for: the server's maximum,
	// so a recording needs as few round trips as possible.
	listPageLimit = 1000
	// maxResponseBytes caps a JSON response body. A page of 1000 object rows is
	// the largest response this client reads, and it stays well below the cap.
	maxResponseBytes = 8 << 20
	// maxErrorBodyBytes caps how much of a failed response is kept on
	// Error.Body; filehouse problems are small, the cap only guards against a
	// proxy answering with an HTML page.
	maxErrorBodyBytes = 4 << 10
	// retryDelay is the pause before a repeated attempt, so a flapping
	// filehouse is not hit twice in the same instant.
	retryDelay = 200 * time.Millisecond
)

// TokenProvider supplies the Bearer credential of every outbound call. The
// production implementation is the iamauth client-credentials source, which
// caches a token and refreshes it before expiry.
type TokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// TokenRefresher is an optional TokenProvider extension. A source that caches a
// credential implements it, so the client can force a refresh after a 401 and
// retry the call once; a source without it is asked again, which may hand back
// the same credential.
type TokenRefresher interface {
	Refresh(ctx context.Context) (string, error)
}

// Client is a typed client for one filehouse base URL. It is safe for concurrent
// use.
type Client struct {
	baseURL string
	timeout time.Duration
	http    *http.Client
	tokens  TokenProvider
	logger  *slog.Logger
}

// missingTokenProvider rejects every call of a client built without a provider,
// so a misconfigured service fails loudly instead of sending anonymous calls.
type missingTokenProvider struct{}

// Token implements TokenProvider.
func (missingTokenProvider) Token(context.Context) (string, error) {
	return "", errors.New("filehouse: no token provider configured")
}

// New builds a client for baseURL. timeout bounds one metadata call
// (MERGE_FILEHOUSE_TIMEOUT); transfers are exempt and run under the caller's job
// context instead, because they stream bodies that take as long as the job
// takes. The token provider supplies the Bearer credential of every request.
func New(baseURL string, tp TokenProvider, timeout time.Duration, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	if tp == nil {
		tp = missingTokenProvider{}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		timeout: timeout,
		// No http.Client.Timeout: it would abort a multi-gigabyte transfer
		// mid-stream, so each call applies its own deadline.
		http:   &http.Client{},
		tokens: tp,
		logger: log,
	}
}

// retryPolicy bounds the attempts of one upstream call. Only calls whose
// outbound payload can be repeated are ever retried: a body-less GET on a
// transport failure or a 5xx response uses policyGET, while every write and
// every transfer uses policySingle, because a streamed body cannot be replayed.
type retryPolicy struct {
	attempts int // total attempts, the first included
}

var (
	policyGET    = retryPolicy{attempts: 3} // two retries
	policySingle = retryPolicy{attempts: 1}
)

// transportError marks a failure before a response was received, or while
// reading one; only these, and a 5xx response, are repeated.
type transportError struct{ err error }

// Error renders the wrapped transport failure.
func (e *transportError) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped transport failure.
func (e *transportError) Unwrap() error { return e.err }

// errResponseTooLarge marks a body above the response cap.
var errResponseTooLarge = errors.New("response exceeds the JSON response cap")

// apiRequest describes one filehouse call. At most one of jsonBody and stream is
// set:
//
//   - jsonBody is marshalled and sent as application/json; the encoded bytes are
//     replayed on every attempt, so only a jsonBody call may retry;
//   - stream is sent as the raw request body and can never be replayed, so a
//     stream call uses policySingle and surfaces a 401 instead of retrying it.
type apiRequest struct {
	action   string // log label
	method   string
	path     string
	query    url.Values
	jsonBody any
	stream   io.Reader
	// contentType is the Content-Type of a streamed body. An encoded JSON body
	// always wins over it.
	contentType string
	out         any // JSON decode target for a 2xx body; nil to discard the body
	policy      retryPolicy
}

// decodeJSON reads the capped body of a response into out and converts a non-2xx
// response into an *Error. Closing the body stays with the caller, which applies
// the same handling on the error paths.
func decodeJSON(req apiRequest, resp *http.Response, out any) error {
	payload, err := readCapped(resp.Body)
	if err != nil {
		if errors.Is(err, errResponseTooLarge) {
			return fmt.Errorf("filehouse: %s: %w", req.action, err)
		}
		return &transportError{err: fmt.Errorf("filehouse: %s: read response: %w", req.action, err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newError(resp.StatusCode, payload)
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("filehouse: %s: decode response: %w", req.action, err)
	}
	return nil
}

// call performs one upstream call under its retry policy and decodes the 2xx
// JSON body into req.out.
func (c *Client) call(ctx context.Context, req apiRequest) error {
	ctx, cancel := c.metadataContext(ctx)
	defer cancel()
	encoded, err := encode(req)
	if err != nil {
		return err
	}
	return c.retry(ctx, req, encoded, func(resp *http.Response) (bool, error) {
		return false, decodeJSON(req, resp, req.out)
	})
}

// download performs one upstream call under its retry policy and returns the
// open 2xx body for the caller to stream and close.
func (c *Client) download(ctx context.Context, req apiRequest) (io.ReadCloser, error) {
	encoded, err := encode(req)
	if err != nil {
		return nil, err
	}
	var body io.ReadCloser
	err = c.retry(ctx, req, encoded, func(resp *http.Response) (bool, error) {
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			payload, readErr := readCapped(resp.Body)
			if readErr != nil && !errors.Is(readErr, errResponseTooLarge) {
				return false, &transportError{err: fmt.Errorf("filehouse: %s: read response: %w", req.action, readErr)}
			}
			return false, newError(resp.StatusCode, payload)
		}
		body = resp.Body
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// writeJSON performs exactly one attempt of a call whose request carries a
// streamed or JSON body and whose success response is a JSON object. It returns
// the decoded body together with the response headers, which carry the digest of
// a write. A write is never retried: its body cannot be replayed, so a transport
// failure or a 401 is surfaced instead.
func writeJSON[T any](ctx context.Context, c *Client, req apiRequest) (T, http.Header, error) {
	var out T
	encoded, err := encode(req)
	if err != nil {
		return out, nil, err
	}
	var header http.Header
	err = c.retry(ctx, req, encoded, func(resp *http.Response) (bool, error) {
		header = resp.Header
		payload, readErr := readCapped(resp.Body)
		if readErr != nil {
			if errors.Is(readErr, errResponseTooLarge) {
				return false, fmt.Errorf("filehouse: %s: %w", req.action, readErr)
			}
			return false, &transportError{err: fmt.Errorf("filehouse: %s: read response: %w", req.action, readErr)}
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return false, newError(resp.StatusCode, payload)
		}
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &out); err != nil {
				return false, fmt.Errorf("filehouse: %s: decode response: %w", req.action, err)
			}
		}
		return false, nil
	})
	if err != nil {
		return out, nil, err
	}
	return out, header, nil
}

// retry runs the attempt loop of one call: it performs the exchange and hands a
// successful response to consume, which reports whether it kept the response
// body. consume must not keep the body of a failed exchange.
func (c *Client) retry(ctx context.Context, req apiRequest, encoded []byte, consume func(*http.Response) (bool, error)) error {
	var lastErr error
	for attempt := 1; attempt <= req.policy.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastErr
			}
			return err
		}
		started := time.Now()
		status, err := c.attempt(ctx, req, encoded, consume)
		elapsed := time.Since(started)
		if err == nil {
			c.logger.Debug("filehouse call",
				"action", req.action,
				"http_status", status,
				"duration_ms", elapsed.Milliseconds(),
				"attempt", attempt,
			)
			return nil
		}
		lastErr = err
		if attempt >= req.policy.attempts || !shouldRetry(ctx, err) {
			attrs := []any{
				"action", req.action,
				"http_status", status,
				"duration_ms", elapsed.Milliseconds(),
				"attempt", attempt,
				"upstream_detail", DetailOf(err),
			}
			if status == 0 {
				attrs = append(attrs, "error", err)
			}
			c.logger.Warn("filehouse call failed", attrs...)
			return err
		}
		c.logger.Debug("filehouse call failed, retrying",
			"action", req.action,
			"http_status", status,
			"attempt", attempt,
			"upstream_detail", DetailOf(err),
		)
		if err := sleepCtx(ctx, retryDelay); err != nil {
			return lastErr
		}
	}
	return lastErr
}

// attempt performs one request/response exchange, including the single forced
// token refresh that follows a 401. It reports the HTTP status, or 0 when the
// exchange failed before a response was read.
func (c *Client) attempt(ctx context.Context, req apiRequest, encoded []byte, consume func(*http.Response) (bool, error)) (int, error) {
	token, err := c.obtainToken(ctx, false)
	if err != nil {
		return 0, fmt.Errorf("filehouse: %s: obtain token: %w", req.action, err)
	}
	resp, err := c.exchange(ctx, req, encoded, token)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode == http.StatusUnauthorized && req.stream == nil {
		drainAndClose(resp.Body)
		token, err = c.obtainToken(ctx, true)
		if err != nil {
			return resp.StatusCode, fmt.Errorf("filehouse: %s: refresh token: %w", req.action, err)
		}
		resp, err = c.exchange(ctx, req, encoded, token)
		if err != nil {
			return 0, err
		}
	}
	status := resp.StatusCode
	kept, err := consume(resp)
	if !kept || err != nil {
		drainAndClose(resp.Body)
	}
	return status, err
}

// exchange performs one HTTP round trip. The response body stays open; the
// caller closes it.
func (c *Client) exchange(ctx context.Context, req apiRequest, encoded []byte, token string) (*http.Response, error) {
	var body io.Reader
	switch {
	case req.stream != nil:
		body = req.stream
	case encoded != nil:
		body = bytes.NewReader(encoded)
	}
	target := c.baseURL + req.path
	if len(req.query) > 0 {
		target += "?" + req.query.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, target, body)
	if err != nil {
		return nil, fmt.Errorf("filehouse: %s: build request: %w", req.action, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if req.out != nil || req.jsonBody != nil {
		// Metadata calls are JSON on both sides; a download is not, and
		// filehouse does not negotiate, so the header is only a hint.
		httpReq.Header.Set("Accept", "application/json")
	}
	switch {
	case encoded != nil:
		httpReq.Header.Set("Content-Type", "application/json")
	case req.contentType != "":
		httpReq.Header.Set("Content-Type", req.contentType)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &transportError{err: fmt.Errorf("filehouse: %s: %w", req.action, err)}
	}
	return resp, nil
}

// metadataContext bounds one metadata call with MERGE_FILEHOUSE_TIMEOUT.
// Transfers skip it: they stream a job-sized body and run under the job context
// alone.
func (c *Client) metadataContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

// obtainToken mints the Bearer credential, forcing a refresh when the previous
// one was rejected and the source supports it.
func (c *Client) obtainToken(ctx context.Context, force bool) (string, error) {
	if force {
		if refresher, ok := c.tokens.(TokenRefresher); ok {
			return refresher.Refresh(ctx)
		}
	}
	return c.tokens.Token(ctx)
}

// shouldRetry reports whether err is unknown-safe to repeat: a transport failure
// or a 5xx response. A 4xx is a decision, not an accident, and is never
// repeated.
func shouldRetry(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var transport *transportError
	if errors.As(err, &transport) {
		return true
	}
	var failure *Error
	return errors.As(err, &failure) && failure.Status >= http.StatusInternalServerError
}

// encode marshals the JSON body of req, if it has one.
func encode(req apiRequest) ([]byte, error) {
	if req.jsonBody == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(req.jsonBody)
	if err != nil {
		return nil, fmt.Errorf("filehouse: %s: encode request: %w", req.action, err)
	}
	return encoded, nil
}

// readCapped reads at most maxResponseBytes+1 bytes, so an over-sized body is
// detected instead of silently truncated.
func readCapped(r io.Reader) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxResponseBytes {
		return nil, errResponseTooLarge
	}
	return payload, nil
}

// drainAndClose consumes what is left of a response body and closes it. The
// drain is bounded: it keeps the connection reusable for the small bodies this
// client fails on, without reading a large aborted transfer to its end.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}

// sleepCtx waits for d and reports ctx.Err() when the context ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
