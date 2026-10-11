package iamauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrServiceAuth reports that the client-credentials exchange with teamusers
// failed: the endpoint answered a non-200 status or a payload that cannot be
// used. Callers can test it with errors.Is. The error chain never carries the
// client secret or the issued access token.
var ErrServiceAuth = errors.New("iam: service authentication failed")

const (
	// tokenRefreshMargin is how long before expiry a cached token is renewed,
	// so an in-flight request never carries a nearly-expired credential.
	tokenRefreshMargin = 60 * time.Second
	// maxTokenLifetime rejects absurd expires_in values.
	maxTokenLifetime = 24 * time.Hour
	// maxTokenResponseBytes bounds the decoded exchange response.
	maxTokenResponseBytes = 64 << 10
	// maxErrorBodyBytes bounds how much of an error response is inspected.
	maxErrorBodyBytes = 4 << 10
	// maxErrorDetailChars bounds a reported response detail.
	maxErrorDetailChars = 200
)

// serviceTokenError is the typed failure of one exchange. It unwraps to
// ErrServiceAuth and deliberately contains no credential material.
type serviceTokenError struct {
	status int
	detail string
}

// Error implements error.
func (e *serviceTokenError) Error() string {
	detail := e.detail
	if detail == "" {
		detail = "no response detail"
	}
	if e.status == 0 {
		return "iam: client credentials exchange failed: " + detail
	}
	return fmt.Sprintf("iam: client credentials exchange failed: HTTP %d: %s", e.status, detail)
}

// Unwrap exposes the sentinel to errors.Is.
func (e *serviceTokenError) Unwrap() error { return ErrServiceAuth }

// cachedToken is one successfully exchanged access token.
type cachedToken struct {
	value     string
	expiresAt time.Time
}

// tokenCall is one in-flight exchange shared by every waiting caller.
type tokenCall struct {
	done  chan struct{}
	token string
	err   error
}

// tokenSource exchanges the configured client id/secret for short-lived
// service access tokens through POST /auth/client-credentials. A fetched token
// is cached until expires_in minus tokenRefreshMargin has elapsed; concurrent
// callers share a single in-flight exchange, so a cold cache under load issues
// one request.
type tokenSource struct {
	baseURL      string
	clientID     string
	clientSecret string
	httpClient   *http.Client
	now          func() time.Time

	mu       sync.Mutex
	cached   *cachedToken
	inflight *tokenCall
}

// newTokenSource builds the credential source. hc defaults to
// http.DefaultClient.
func newTokenSource(baseURL, clientID, clientSecret string, hc *http.Client) *tokenSource {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &tokenSource{
		baseURL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		clientID:     strings.TrimSpace(clientID),
		clientSecret: clientSecret,
		httpClient:   hc,
		now:          time.Now,
	}
}

// Token returns a valid service access token, exchanging a new one when the
// cached token is missing, about to expire or was invalidated by an error.
func (s *tokenSource) Token(ctx context.Context) (string, error) {
	if s == nil {
		return "", errors.New("iam: no client credentials are configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := s.now()

	s.mu.Lock()
	if s.cached != nil && now.Before(s.cached.expiresAt) {
		token := s.cached.value
		s.mu.Unlock()
		return token, nil
	}
	if call := s.inflight; call != nil {
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.token, call.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	call := &tokenCall{done: make(chan struct{})}
	s.inflight = call
	s.mu.Unlock()

	token, ttl, err := s.exchange(ctx)

	s.mu.Lock()
	if s.inflight == call {
		s.inflight = nil
	}
	if err == nil {
		s.cached = &cachedToken{value: token, expiresAt: s.now().Add(ttl)}
	}
	call.token, call.err = token, err
	close(call.done)
	s.mu.Unlock()

	return token, err
}

// Refresh forces one client-credentials exchange even when a token is cached.
// It exchanges directly rather than clearing the cache and calling Token, so a
// concurrent in-flight refresh cannot hand back the rejected credential.
func (s *tokenSource) Refresh(ctx context.Context) (string, error) {
	if s == nil {
		return "", errors.New("iam: no client credentials are configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	token, ttl, err := s.exchange(ctx)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.cached = &cachedToken{value: token, expiresAt: s.now().Add(ttl)}
	s.mu.Unlock()
	return token, nil
}

// TokenFunc adapts Token to the func() (string, error) shape expected by
// iam.WithTokenSource. The SDK callback has no context, so it uses the
// background context and relies on the HTTP client timeout.
func (s *tokenSource) TokenFunc() func() (string, error) {
	return func() (string, error) {
		return s.Token(context.Background())
	}
}

// exchange performs one client-credentials request and returns the access
// token together with the lifetime it should be cached for.
func (s *tokenSource) exchange(ctx context.Context) (string, time.Duration, error) {
	if s.baseURL == "" {
		return "", 0, errors.New("iam: base URL is required for the client credentials exchange")
	}
	if s.clientID == "" || s.clientSecret == "" {
		return "", 0, errors.New("iam: client id and client secret are both required")
	}

	requestBody, err := json.Marshal(struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}{ClientID: s.clientID, ClientSecret: s.clientSecret})
	if err != nil {
		return "", 0, fmt.Errorf("iam: encode client credentials request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/auth/client-credentials", bytes.NewReader(requestBody))
	if err != nil {
		return "", 0, fmt.Errorf("iam: create client credentials request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := s.httpClient.Do(request)
	if err != nil {
		return "", 0, fmt.Errorf("iam: client credentials request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", 0, &serviceTokenError{status: response.StatusCode, detail: responseDetail(response.Body)}
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxTokenResponseBytes)).Decode(&payload); err != nil {
		return "", 0, &serviceTokenError{status: response.StatusCode, detail: "malformed response body"}
	}
	token := strings.TrimSpace(payload.AccessToken)
	if token == "" {
		return "", 0, &serviceTokenError{status: response.StatusCode, detail: "response is missing access_token"}
	}
	lifetime := time.Duration(payload.ExpiresIn) * time.Second
	if lifetime <= 0 || lifetime > maxTokenLifetime {
		return "", 0, &serviceTokenError{status: response.StatusCode, detail: "response has an invalid expires_in"}
	}

	// Cache until 60 seconds before expiry. A lifetime shorter than the margin
	// would otherwise never cache, so fall back to its first half.
	ttl := lifetime - tokenRefreshMargin
	if ttl <= 0 {
		ttl = lifetime / 2
	}
	return token, ttl, nil
}

// responseDetail extracts a short, single-line description from an error
// response body without ever exposing request credentials.
func responseDetail(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
	if err != nil || len(raw) == 0 {
		return "no response detail"
	}
	var problem struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &problem) == nil {
		if detail := strings.TrimSpace(problem.Detail); detail != "" {
			return truncateDetail(detail)
		}
	}
	return truncateDetail(string(raw))
}

// truncateDetail collapses whitespace and caps the detail length so a remote
// body cannot flood logs or problem documents.
func truncateDetail(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	if detail == "" {
		return "no response detail"
	}
	if len(detail) > maxErrorDetailChars {
		return detail[:maxErrorDetailChars]
	}
	return detail
}
