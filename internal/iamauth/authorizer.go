package iamauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	iam "github.com/crazy4chicken/nsc-teamusers/sdk/go"

	"github.com/crazy4chicken/smartclass-merge/internal/httpx"
)

// Scopes of the merge:<action>:<scope> permission keys. Require walks them in
// this order for the resource a route resolves.
const (
	ScopeAny  = "any"
	ScopeTeam = "team"
	ScopeOwn  = "own"
)

// resourceSegment is the resource segment of the merge permission keys.
const resourceSegment = "merge"

// defaultHTTPTimeout bounds teamusers HTTP calls when Options.Timeout is unset.
const defaultHTTPTimeout = 10 * time.Second

// reasonUnavailable is the SDK's denial reason for a failed authorization
// lookup. It is the only reason that answers 503 instead of 403: every other
// reason is a decision, not an outage.
const reasonUnavailable = "authorization service unavailable"

// Dev identity injected by Middleware when Options.Dev is set.
const (
	devSubject      = "dev-user"
	devServiceToken = "dev"
)

// Options configures the teamusers-backed Authorizer. BaseURL is required
// unless Dev is set: dev mode is a fully offline bypass.
type Options struct {
	// BaseURL is the teamusers base URL, for example http://teamusers:8080.
	BaseURL string
	// Issuer is the expected access-token "iss" claim; empty selects the SDK
	// default ("teamusers").
	Issuer string
	// Audience is the expected access-token "aud" claim; empty selects the SDK
	// default ("teamusers").
	Audience string
	// ClientID and ClientSecret enable the client-credentials exchange
	// (POST /auth/client-credentials) used for permission lookups and for the
	// outbound filehouse token. They must be set together.
	ClientID     string
	ClientSecret string
	// Timeout bounds teamusers HTTP calls; a non-positive value selects
	// defaultHTTPTimeout.
	Timeout time.Duration
	// Logger receives authorization warnings. A nil logger discards them.
	Logger *slog.Logger
	// Dev bypasses token verification and permission checks entirely: requests
	// receive synthetic claims and every permission is granted. Never enable it
	// outside local development.
	Dev bool
}

// Authorizer verifies teamusers bearer tokens and authorizes requests through
// the merge:<action>:<scope> ladder.
type Authorizer struct {
	client      *iam.Client
	verifier    *iam.Verifier
	permissions *iam.PermissionsClient
	tokens      *tokenSource
	httpClient  *http.Client
	baseURL     string
	log         *slog.Logger
	dev         bool
}

// New builds the verified-token pipeline: a JWKS verifier, a permission cache
// fed by the client-credentials token source, and the SDK client that combines
// them. It never performs network I/O, so with Dev set it succeeds offline.
func New(opts Options) (*Authorizer, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	clientID := strings.TrimSpace(opts.ClientID)

	if opts.Dev {
		authorizer := &Authorizer{baseURL: base, log: logger, dev: true}
		if base != "" && clientID != "" && opts.ClientSecret != "" {
			httpClient := opts.resolveHTTPClient()
			authorizer.httpClient = httpClient
			authorizer.tokens = newTokenSource(base, clientID, opts.ClientSecret, httpClient)
		}
		return authorizer, nil
	}

	if base == "" {
		return nil, errors.New("iam: teamusers base URL is required")
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("iam: teamusers base URL %q must be an absolute http(s) URL", opts.BaseURL)
	}
	if (clientID == "") != (opts.ClientSecret == "") {
		return nil, errors.New("iam: client id and client secret must be set together")
	}

	httpClient := opts.resolveHTTPClient()
	verifier := iam.NewVerifier(
		base,
		iam.WithIssuer(opts.Issuer),
		iam.WithAudience(opts.Audience),
		iam.WithHTTPClient(httpClient),
	)
	permissionOptions := []any{iam.WithHTTPClient(httpClient)}
	authorizer := &Authorizer{
		verifier:   verifier,
		baseURL:    base,
		httpClient: httpClient,
		log:        logger,
	}
	if clientID != "" {
		authorizer.tokens = newTokenSource(base, clientID, opts.ClientSecret, httpClient)
		permissionOptions = append(permissionOptions, iam.WithTokenSource(authorizer.tokens.TokenFunc()))
	}
	authorizer.permissions = iam.NewPermissionsClient(base, permissionOptions...)
	authorizer.client = iam.NewClient(verifier, authorizer.permissions)
	return authorizer, nil
}

// Close stops the verifier's background JWKS refresh workers. It is safe to
// call more than once.
func (a *Authorizer) Close() {
	if a == nil || a.verifier == nil {
		return
	}
	_ = a.verifier.Close()
}

// Token returns a valid teamusers service access token for outbound filehouse
// calls. It satisfies the filehouse.TokenProvider interface structurally. In
// dev mode without configured credentials it returns a synthetic token so a
// dev filehouse can run its own bypass.
func (a *Authorizer) Token(ctx context.Context) (string, error) {
	if a == nil {
		return "", errors.New("iam: authorizer is not configured")
	}
	if a.tokens != nil {
		return a.tokens.Token(ctx)
	}
	if a.dev {
		return devServiceToken, nil
	}
	return "", errors.New("iam: service credentials are not configured")
}

// Refresh forces a new service token, bypassing the cache. filehouse.Client
// detects it through the filehouse.TokenRefresher interface and calls it once
// after a 401, so a credential rejected upstream (for example after a key
// rotation) is replaced instead of being retried unchanged.
func (a *Authorizer) Refresh(ctx context.Context) (string, error) {
	if a == nil {
		return "", errors.New("iam: authorizer is not configured")
	}
	if a.tokens != nil {
		return a.tokens.Refresh(ctx)
	}
	return a.Token(ctx)
}

// Middleware verifies a bearer token and stores the resulting claims in the
// request context, where handlers read them with ClaimsFromContext. Requests
// without an Authorization header pass through unauthenticated so public
// routes can share the router; Require rejects them. In dev mode it injects
// synthetic claims without touching the network.
func (a *Authorizer) Middleware() func(http.Handler) http.Handler {
	if a == nil {
		return func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeDecision(w, http.StatusUnauthorized, "authorization is not configured")
			})
		}
	}
	if a.dev {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				claims := iam.Claims{Subject: devSubject, Kind: "user", Expiry: time.Now().Add(time.Hour)}
				next.ServeHTTP(w, r.WithContext(iam.WithClaims(r.Context(), claims)))
			})
		}
	}
	verify := a.client.Middleware
	return func(next http.Handler) http.Handler {
		authenticated := verify(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if next != nil && strings.TrimSpace(r.Header.Get("Authorization")) == "" {
				next.ServeHTTP(w, r)
				return
			}
			authenticated.ServeHTTP(w, r)
		})
	}
}

// Require returns middleware that authorizes one any-scoped permission key
// (for example "merge:read:any") through the any → team → own ladder. The
// resolver supplies the resource identity of the route; a nil resolver means
// the route has no resource, so only the any-scoped key can grant access. The
// granting rung is recorded in the request context for handlers that must
// narrow a collection to it (GrantedScopeFromContext).
//
// Responses written here: 401 with the teamusers decision body for a request
// without claims, 403 problem+json detail "insufficient_permissions" for a
// denied decision, and 503 problem+json detail "iam_unavailable" when
// teamusers cannot answer (contract §4).
func (a *Authorizer) Require(permission string, resolve ScopeResolver) func(http.Handler) http.Handler {
	if a == nil {
		return unavailableGuard()
	}
	action, ok := anyScopedAction(permission)
	if !a.dev && (!ok || a.client == nil) {
		return unavailableGuard()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, authenticated := iam.ClaimsFromContext(r.Context())
			if !authenticated {
				writeDecision(w, http.StatusUnauthorized, "authentication is required")
				return
			}
			if a.dev {
				next.ServeHTTP(w, r.WithContext(withGrantedScope(r.Context(), ScopeAny)))
				return
			}
			scope := Scope{}
			if resolve != nil {
				resolved, err := resolve(r)
				if err != nil {
					a.writeResolverError(w, r, err)
					return
				}
				scope = resolved
			}
			keys, resource := permissionLadder(action, scope, claims)
			unavailable := false
			for _, key := range keys {
				allowed, reason := a.client.Allow(r.Context(), claims, key, resource)
				if allowed {
					next.ServeHTTP(w, r.WithContext(withGrantedScope(r.Context(), scopeOfKey(key, action))))
					return
				}
				if reason == reasonUnavailable {
					unavailable = true
				}
			}
			if unavailable {
				httpx.WriteProblemReason(w, r, http.StatusServiceUnavailable, "iam_unavailable", "teamusers could not answer the authorization request")
				return
			}
			httpx.WriteProblemReason(w, r, http.StatusForbidden, "insufficient_permissions", "required one of: "+strings.Join(keys, ", "))
		})
	}
}

// scopeOfKey returns the scope rung of one candidate key of the ladder, so the
// authorized request records exactly the rung that granted it.
func scopeOfKey(key, action string) string {
	prefix := resourceSegment + ":" + action + ":"
	return strings.TrimPrefix(key, prefix)
}

// writeResolverError maps a ScopeResolver failure onto a problem response.
func (a *Authorizer) writeResolverError(w http.ResponseWriter, r *http.Request, err error) {
	var resourceErr *ResourceError
	if errors.As(err, &resourceErr) {
		status := resourceErr.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		detail := resourceErr.Detail
		if detail == "" {
			detail = "internal_error"
		}
		if status >= http.StatusInternalServerError {
			a.log.Warn("resolve authorization resource", "method", r.Method, "path", r.URL.Path, "detail", detail, "error", err)
		}
		httpx.WriteProblem(w, r, status, detail)
		return
	}
	a.log.Warn("resolve authorization resource", "method", r.Method, "path", r.URL.Path, "error", err)
	httpx.WriteProblem(w, r, http.StatusInternalServerError, "internal_error")
}

// resolveHTTPClient returns the client used for every teamusers call. A
// caller-supplied timeout always governs a freshly built client.
func (o Options) resolveHTTPClient() *http.Client {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	return &http.Client{Timeout: timeout}
}

// anyScopedAction validates that permission is a merge:<action>:any key and
// returns its action segment.
func anyScopedAction(permission string) (string, bool) {
	parsed, err := iam.Parse(strings.TrimSpace(permission))
	if err != nil || parsed.Resource != resourceSegment || parsed.Scope != ScopeAny || parsed.Deny {
		return "", false
	}
	return parsed.Action, true
}

// unavailableGuard fails every request closed when the authorizer is unusable;
// it is only reachable through a programming or configuration error.
func unavailableGuard() func(http.Handler) http.Handler {
	return func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "service_unavailable")
		})
	}
}

// decisionResponse mirrors the body the teamusers middleware writes for
// authentication decisions.
type decisionResponse struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason"`
}

// writeDecision writes the teamusers decision body together with the
// WWW-Authenticate challenge.
func writeDecision(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="teamusers"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(decisionResponse{Allow: false, Reason: reason})
}
