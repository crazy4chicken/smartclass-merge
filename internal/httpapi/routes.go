package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/crazy4chicken/smartclass-merge/internal/domain"
	"github.com/crazy4chicken/smartclass-merge/internal/httpx"
	"github.com/crazy4chicken/smartclass-merge/internal/iamauth"
	"github.com/crazy4chicken/smartclass-merge/internal/store"
)

// Any-scoped permission keys of the routes. Require expands each into the
// any → team → own ladder for the resolved resource.
const (
	permissionReadAny  = "merge:read:any"
	permissionWriteAny = "merge:write:any"
)

// guard describes the authorization of one route: the any-scoped permission
// key and, for routes with a resource identity, the resolver that loads it.
// An empty permission marks a public route.
type guard struct {
	permission string
	resolve    iamauth.ScopeResolver
}

// registerRoutes is the service's only route table: the runtime router and the
// OpenAPI generator both replay it, so adding a route here is the single step
// that exposes it (and cmd/genspec immediately demands its documentation).
func registerRoutes(s *Server, r chi.Router) {
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	r.Route("/api/v1", func(api chi.Router) {
		api.Use(s.deps.Authorizer.Middleware())

		s.register(api, http.MethodPost, "/merges", s.handleMergeCreate, guard{permission: permissionWriteAny, resolve: s.createScope})
		s.register(api, http.MethodGet, "/merges", s.handleMergeList, guard{permission: permissionReadAny, resolve: s.collectionScope})
		s.register(api, http.MethodGet, "/merges/{id}", s.handleMergeGet, guard{permission: permissionReadAny, resolve: s.jobScope})
		s.register(api, http.MethodPost, "/merges/{id}/cancel", s.handleMergeCancel, guard{permission: permissionWriteAny, resolve: s.jobScope})
	})
}

// register mounts one route together with its permission guard.
func (s *Server) register(r chi.Router, method, pattern string, handler http.HandlerFunc, g guard) {
	if g.permission == "" {
		r.Method(method, pattern, handler)
		return
	}
	r.With(s.deps.Authorizer.Require(g.permission, g.resolve)).Method(method, pattern, handler)
}

// jobScope resolves the ownership snapshot of the job named by the {id} path
// parameter, so the ladder can try the team and own rungs against it.
func (s *Server) jobScope(r *http.Request) (iamauth.Scope, error) {
	job, err := s.deps.Store.Jobs().Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		return iamauth.Scope{}, resolveResourceError(err, "job_not_found")
	}
	return scopeOfJob(*job), nil
}

// createScope resolves the permission identity of a job that does not exist
// yet, mirroring dispatchub's create pattern: a resource without a stored
// ownership snapshot cannot match the own rung, so only the any-scoped key
// would grant the route. The job will belong to the caller's team, so the team
// rung is added from the claims; the own rung stays out, because there is no
// owner to compare against before the job exists.
func (s *Server) createScope(r *http.Request) (iamauth.Scope, error) {
	claims, _ := iamauth.ClaimsFromContext(r.Context())
	return iamauth.Scope{TeamID: claims.Team}, nil
}

// collectionScope resolves the acting identity of a collection route: the same
// any → team → own ladder decides whether the caller may list at all, and the
// rung that granted it decides which rows the handler returns
// (GrantedScopeFromContext).
func (s *Server) collectionScope(r *http.Request) (iamauth.Scope, error) {
	claims, _ := iamauth.ClaimsFromContext(r.Context())
	return iamauth.Scope{TeamID: claims.Team, OwnerID: claims.Subject}, nil
}

// scopeOfJob maps a job's snapshotted ownership onto the permission ladder.
func scopeOfJob(job domain.Job) iamauth.Scope {
	return iamauth.Scope{TeamID: job.TeamID, OwnerID: job.OwnerID}
}

// resolveResourceError maps a failed resource lookup onto the status and
// stable detail the authorization guard answers with.
func resolveResourceError(err error, notFoundDetail string) error {
	if errors.Is(err, store.ErrNotFound) {
		return &iamauth.ResourceError{Status: http.StatusNotFound, Detail: notFoundDetail, Err: err}
	}
	return &iamauth.ResourceError{Status: http.StatusInternalServerError, Detail: "internal_error", Err: err}
}

// writeJobError maps a job store error onto the stable API error codes,
// mirroring dispatchub's writeCommandError: the store sentinels are
// authoritative and anything else is a logged 500.
func (s *Server) writeJobError(w http.ResponseWriter, r *http.Request, err error, notFoundDetail string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, notFoundDetail)
	case errors.Is(err, store.ErrNotCancelable):
		httpx.WriteProblem(w, r, http.StatusConflict, "status_not_cancelable")
	case errors.Is(err, store.ErrValidation):
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, store.ErrConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "invalid_request")
	default:
		s.writeInternalError(w, r, err)
	}
}

// idempotencyKey reads the optional X-Idempotency-Key header.
func idempotencyKey(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Idempotency-Key"))
}
