package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/crazy4chicken/smartclass-merge/internal/domain"
	"github.com/crazy4chicken/smartclass-merge/internal/ffmpeg"
	"github.com/crazy4chicken/smartclass-merge/internal/filehouse"
	"github.com/crazy4chicken/smartclass-merge/internal/httpx"
	"github.com/crazy4chicken/smartclass-merge/internal/iamauth"
	"github.com/crazy4chicken/smartclass-merge/internal/scheduler"
	"github.com/crazy4chicken/smartclass-merge/internal/store"
)

const (
	// readyTimeout bounds the dependency checks behind GET /readyz.
	readyTimeout = 5 * time.Second
	// defaultListLimit is the page size of the merge list when limit is
	// omitted, matching the store's default.
	defaultListLimit = 100
	// maxListLimit caps a caller-supplied limit, matching the store's clamp.
	maxListLimit = 1000
	// defaultJobFPS is the frame rate applied when neither the create body nor
	// the configuration carries one (MERGE_DEFAULT_FPS).
	defaultJobFPS = 25
	// defaultJobMaxAttempts is the attempt budget applied when the
	// configuration was not validated (inert routers).
	defaultJobMaxAttempts = 3
	// defaultDestExtension is the container extension of a merge job whose
	// create body names no codec.
	defaultDestExtension = ".mp4"
)

// Deps carries the HTTP layer's dependencies. New only stores them: it opens
// no connection and performs no network call, so cmd/genspec constructs the
// router from zero-valued dependencies and walks it for the OpenAPI document.
type Deps struct {
	// Store is the persistence layer. It may be nil when the router is built
	// only to enumerate routes.
	Store *store.Store
	// Authorizer verifies bearer tokens and enforces the merge:* permissions.
	Authorizer *iamauth.Authorizer
	// Scheduler owns the in-flight merges, so a cancel aborts the running
	// attempt immediately instead of waiting for the next claim.
	Scheduler *scheduler.Scheduler
	// Filehouse lists the segments of a stream when a create body names
	// device_id and stream_id instead of explicit segments.
	Filehouse *filehouse.Client
	// FFmpeg resolves the embedded binary for the readiness probe and the
	// container extension a requested codec defaults to.
	FFmpeg *ffmpeg.FFmpeg
	// DefaultSourceBucket is the bucket segments are read from when a create
	// body names none (MERGE_SOURCE_BUCKET).
	DefaultSourceBucket string
	// DefaultDestBucket is the bucket merged objects are written to when a
	// create body names none (MERGE_DEST_BUCKET).
	DefaultDestBucket string
	// DefaultFPS is the frame rate of a create body without fps
	// (MERGE_DEFAULT_FPS).
	DefaultFPS int
	// MaxAttempts is the attempt budget stamped on a created job
	// (MERGE_MAX_ATTEMPTS).
	MaxAttempts int
	// WorkDir is the directory merges run in and the readiness probe writes to
	// (MERGE_WORK_DIR).
	WorkDir string
	// Version is the build version the process reports (cmd/merge -version).
	Version string
	// Logger receives request-scoped warnings and errors.
	Logger *slog.Logger
}

// Server is the service's HTTP surface: the chi router of registerRoutes
// together with the dependencies every handler closes over. It embeds the
// router, so it is both the http.Handler cmd/merge serves and a chi.Router
// cmd/genspec walks to replay the route table into the OpenAPI document.
type Server struct {
	chi.Router
	deps Deps
	log  *slog.Logger
}

// New builds the HTTP router: the public health probes and the /api/v1 group
// guarded by the teamusers middleware and the per-route permission ladder.
func New(d Deps) *Server {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{deps: d, log: logger}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)
	registerRoutes(s, r)
	s.Router = r
	return s
}

// corsMiddleware applies the fleet's permissive browser policy. It mirrors the
// policy smartclass-dispatchub installs, reimplemented here so this service
// keeps the dependency set of go.mod.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Access-Control-Allow-Origin", "*")
		header.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		header.Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, X-Idempotency-Key")
		header.Set("Access-Control-Expose-Headers", "Location, X-Request-Id, Idempotency-Replayed")
		header.Set("Access-Control-Max-Age", "300")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusResponse is the body of both health probes.
type statusResponse struct {
	Status string `json:"status"`
}

// listResponse is the envelope every collection route returns. NextCursor is
// empty on the last page.
type listResponse[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor"`
}

// handleHealthz answers GET /healthz. It never touches a dependency, so it
// keeps reporting liveness while PostgreSQL or filehouse is down.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// handleReadyz answers GET /readyz after pinging PostgreSQL, probing the work
// directory and resolving the ffmpeg binary. The failing dependencies are
// logged, never returned: the body is the same {"status":"unavailable"} the
// contract freezes.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()

	var unreachable []string
	if s.deps.Store == nil {
		unreachable = append(unreachable, "postgres")
	} else if err := s.deps.Store.Ping(ctx); err != nil {
		s.log.Warn("readiness: postgres is unreachable", "error", err)
		unreachable = append(unreachable, "postgres")
	}
	if err := s.workDirWritable(); err != nil {
		s.log.Warn("readiness: work directory is not writable", "work_dir", s.deps.WorkDir, "error", err)
		unreachable = append(unreachable, "work dir")
	}
	if s.deps.FFmpeg == nil {
		unreachable = append(unreachable, "ffmpeg")
	} else if _, err := s.deps.FFmpeg.Path(ctx); err != nil {
		s.log.Warn("readiness: ffmpeg is not resolvable", "error", err)
		unreachable = append(unreachable, "ffmpeg")
	}
	if len(unreachable) > 0 {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "unavailable"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, statusResponse{Status: "ready"})
}

// workDirWritable probes the work directory by creating and removing a
// temporary file, which is exactly what a merge job must be able to do.
func (s *Server) workDirWritable() error {
	workDir := strings.TrimSpace(s.deps.WorkDir)
	if workDir == "" {
		return errors.New("MERGE_WORK_DIR is not configured")
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}
	probe, err := os.CreateTemp(workDir, ".readyz-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

// writeStoreError maps a store error onto a problem response: ErrNotFound
// becomes 404 with the route's not-found detail, ErrValidation and ErrConflict
// become 400 and 409 invalid_request, everything else a logged 500.
func (s *Server) writeStoreError(w http.ResponseWriter, r *http.Request, err error, notFoundDetail string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, notFoundDetail)
	case errors.Is(err, store.ErrValidation):
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, store.ErrConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "invalid_request")
	default:
		s.writeInternalError(w, r, err)
	}
}

// writeInternalError logs err and answers a generic 500. The cause never
// reaches the client.
func (s *Server) writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	httpx.WriteProblem(w, r, http.StatusInternalServerError, "internal_error")
}

// writeInvalidRequest answers 400 invalid_request with a human-readable reason.
func (s *Server) writeInvalidRequest(w http.ResponseWriter, r *http.Request, reason string) {
	httpx.WriteProblemReason(w, r, http.StatusBadRequest, "invalid_request", reason)
}

// writeSourceUnavailable answers 503 source_unavailable with a reason: the
// segments of the recording cannot be listed, so no job can be created from
// them.
func (s *Server) writeSourceUnavailable(w http.ResponseWriter, r *http.Request, reason string) {
	httpx.WriteProblemReason(w, r, http.StatusServiceUnavailable, "source_unavailable", reason)
}

// parseLimit reads the optional limit query parameter: missing selects the
// default page size, a non-positive or non-numeric value is a 400, and a value
// above maxListLimit is clamped.
func parseLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return defaultListLimit, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		httpx.WriteProblemReason(w, r, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
		return 0, false
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	return limit, true
}

// defaultJobFPS returns the configured frame rate of a create body without fps,
// falling back to the documented default when the config was not validated
// (inert routers).
func (s *Server) defaultJobFPS() int {
	if fps := s.deps.DefaultFPS; fps > 0 {
		return fps
	}
	return defaultJobFPS
}

// maxJobAttempts returns the configured attempt budget, falling back to the
// documented default when the config was not validated (inert routers).
func (s *Server) maxJobAttempts() int {
	if attempts := s.deps.MaxAttempts; attempts > 0 {
		return attempts
	}
	return defaultJobMaxAttempts
}

// newJobListResponse wraps one store page, encoding a nil slice as [].
func newJobListResponse(page store.Page) listResponse[domain.Job] {
	items := page.Items
	if items == nil {
		items = []domain.Job{}
	}
	return listResponse[domain.Job]{Items: items, NextCursor: page.NextCursor}
}
