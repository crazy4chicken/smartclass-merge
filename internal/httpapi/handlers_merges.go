package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/crazy4chicken/smartclass-merge/internal/config"
	"github.com/crazy4chicken/smartclass-merge/internal/domain"
	"github.com/crazy4chicken/smartclass-merge/internal/ffmpeg"
	"github.com/crazy4chicken/smartclass-merge/internal/httpx"
	"github.com/crazy4chicken/smartclass-merge/internal/iamauth"
	"github.com/crazy4chicken/smartclass-merge/internal/store"
)

// segmentKeyLayout is the flush timestamp the recorder embeds in a segment key
// name.
const segmentKeyLayout = "20060102T150405"

// segmentKeySuffix is the extension of every recorded segment object.
const segmentKeySuffix = ".bin"

// createMergeRequest is the body of POST /api/v1/merges. Exactly one of
// Segments and (DeviceID, StreamID) selects the source: an explicit list or
// the whole recording, whose segments are then listed from filehouse.
type createMergeRequest struct {
	Bucket   string           `json:"bucket"`
	DeviceID string           `json:"device_id"`
	StreamID string           `json:"stream_id"`
	Segments []segmentRequest `json:"segments"`
	FPS      *int             `json:"fps"`
	Codec    string           `json:"codec"`
	Dest     *destRequest     `json:"dest"`
}

// segmentRequest is one explicitly selected segment. Seq is a pointer, so a
// body that omits it keeps the caller's order instead of sorting on a zero.
type segmentRequest struct {
	Key       string `json:"key"`
	Seq       *int64 `json:"seq"`
	SizeBytes int64  `json:"size_bytes"`
}

// destRequest is the destination of the merged object. Both members are
// optional: an empty bucket selects MERGE_DEST_BUCKET and an empty key is
// derived as merged/YYYY/MM/DD/<id><ext>.
type destRequest struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// listedSegment is one listed segment object together with the ordering key
// parsed from its key name.
type listedSegment struct {
	segment domain.Segment
	flush   time.Time
}

// handleMergeCreate answers POST /api/v1/merges. The body is decoded strictly,
// so an unknown member is a client error instead of a silently ignored field.
// The resolved source is frozen on the job and the response is 201 with the
// stored job; a retry carrying the same X-Idempotency-Key replays that job
// with Idempotency-Replayed: true instead of starting a second merge.
func (s *Server) handleMergeCreate(w http.ResponseWriter, r *http.Request) {
	var request createMergeRequest
	if err := httpx.DecodeJSONStrict(w, r, &request); err != nil {
		s.writeInvalidRequest(w, r, "invalid JSON request body")
		return
	}

	deviceID := strings.TrimSpace(request.DeviceID)
	streamID := strings.TrimSpace(request.StreamID)
	explicit := len(request.Segments) > 0
	derived := deviceID != "" || streamID != ""
	if explicit == derived {
		s.writeInvalidRequest(w, r, "either segments or device_id with stream_id must be given")
		return
	}

	codec, ok := supportedCodec(request.Codec)
	if !ok {
		s.writeInvalidRequest(w, r, unsupportedCodecReason(request.Codec))
		return
	}
	fps := s.defaultJobFPS()
	if request.FPS != nil {
		fps = *request.FPS
	}
	if fps < config.MinFPS || fps > config.MaxFPS {
		s.writeInvalidRequest(w, r, fmt.Sprintf("fps must be between %d and %d", config.MinFPS, config.MaxFPS))
		return
	}
	sourceBucket := strings.TrimSpace(request.Bucket)
	if sourceBucket == "" {
		sourceBucket = strings.TrimSpace(s.deps.DefaultSourceBucket)
	}
	if sourceBucket == "" {
		s.writeInvalidRequest(w, r, "bucket is required and MERGE_SOURCE_BUCKET is not configured")
		return
	}

	var prefix string
	var segments []domain.Segment
	if explicit {
		resolved, ok := s.parseSegmentRequests(w, r, request.Segments)
		if !ok {
			return
		}
		segments = resolved
	} else {
		if deviceID == "" || streamID == "" {
			s.writeInvalidRequest(w, r, "device_id and stream_id must be given together")
			return
		}
		prefix = streamPrefix(deviceID, streamID)
		resolved, ok := s.listStreamSegments(w, r, sourceBucket, prefix)
		if !ok {
			return
		}
		segments = resolved
	}

	destBucket := strings.TrimSpace(s.deps.DefaultDestBucket)
	if request.Dest != nil {
		if named := strings.TrimSpace(request.Dest.Bucket); named != "" {
			destBucket = named
		}
	}
	if destBucket == "" {
		s.writeInvalidRequest(w, r, "dest.bucket is required and MERGE_DEST_BUCKET is not configured")
		return
	}
	id := domain.NewID()
	destKey := ""
	if request.Dest != nil {
		destKey = strings.TrimSpace(request.Dest.Key)
	}
	if destKey == "" {
		destKey = defaultDestKey(id, codec)
	}

	claims, _ := iamauth.ClaimsFromContext(r.Context())
	job := &domain.Job{
		ID:        id,
		OwnerID:   claims.Subject,
		OwnerKind: claims.Kind,
		TeamID:    claims.Team,
		Source: domain.Source{
			Bucket:   sourceBucket,
			Prefix:   prefix,
			Segments: segments,
			FPS:      fps,
			Codec:    string(codec),
		},
		Dest:           domain.Dest{Bucket: destBucket, Key: destKey},
		MaxAttempts:    s.maxJobAttempts(),
		IdempotencyKey: idempotencyKey(r),
	}
	created, err := s.deps.Store.Jobs().Create(r.Context(), job)
	if err != nil {
		s.writeJobError(w, r, err, "job_not_found")
		return
	}
	if created.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/merges/"+created.Job.ID)
	httpx.WriteJSON(w, http.StatusCreated, created.Job)
}

// handleMergeGet answers GET /api/v1/merges/{id}. The route guard already
// resolved the job for the permission ladder, so the handler re-reads the row
// it authorized instead of trusting a stale copy.
func (s *Server) handleMergeGet(w http.ResponseWriter, r *http.Request) {
	job, err := s.deps.Store.Jobs().Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeJobError(w, r, err, "job_not_found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, job)
}

// handleMergeList answers GET /api/v1/merges, newest first. limit and cursor
// are passed through to the store, which clamps the page size and validates
// the cursor; the visibility filter comes from the rung that granted the
// request, so a team-scoped caller sees its team's jobs and an own-scoped
// caller only the jobs it created.
func (s *Server) handleMergeList(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	status := domain.Status(strings.TrimSpace(query.Get("status")))
	if status != "" && !knownJobStatus(status) {
		s.writeInvalidRequest(w, r, "status must be one of queued, running, succeeded, failed, canceled")
		return
	}
	claims, _ := iamauth.ClaimsFromContext(r.Context())
	page, err := s.deps.Store.Jobs().List(r.Context(), store.ListFilter{
		Limit:      limit,
		Cursor:     strings.TrimSpace(query.Get("cursor")),
		Status:     status,
		Visibility: visibilityOf(r),
		Subject:    claims.Subject,
		Team:       claims.Team,
	})
	if err != nil {
		s.writeJobError(w, r, err, "job_not_found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newJobListResponse(page))
}

// handleMergeCancel answers POST /api/v1/merges/{id}/cancel. The store moves a
// queued or running job to canceled first — an already canceled job is
// returned unchanged — and the scheduler then aborts the attempt this process
// is running, when there is one.
func (s *Server) handleMergeCancel(w http.ResponseWriter, r *http.Request) {
	job, err := s.deps.Store.Jobs().Cancel(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeJobError(w, r, err, "job_not_found")
		return
	}
	if s.deps.Scheduler != nil && s.deps.Scheduler.Cancel(job.ID) {
		s.log.Info("merge job canceled in flight", "job_id", job.ID)
	}
	httpx.WriteJSON(w, http.StatusOK, job)
}

// visibilityOf maps the rung that granted the request onto the store's row
// filter: any lists every job, while team and own narrow the list to the
// caller's team and to the jobs the caller created.
func visibilityOf(r *http.Request) string {
	scope, ok := iamauth.GrantedScopeFromContext(r.Context())
	if !ok {
		// Require always records the rung, so this is a programming error: the
		// narrowest filter is the safe answer.
		return store.VisibilityOwn
	}
	switch scope {
	case iamauth.ScopeAny:
		return store.VisibilityAny
	case iamauth.ScopeTeam:
		return store.VisibilityTeam
	default:
		return store.VisibilityOwn
	}
}

// parseSegmentRequests validates the explicit segment selection of a create
// body. When every entry carries seq the selection is sorted by it, so a
// caller that knows the recorder's numbering does not have to order the array;
// a partial seq keeps the given order (contract §4).
func (s *Server) parseSegmentRequests(w http.ResponseWriter, r *http.Request, inputs []segmentRequest) ([]domain.Segment, bool) {
	segments := make([]domain.Segment, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	ordered := true
	for _, input := range inputs {
		key := strings.TrimSpace(input.Key)
		if key == "" {
			s.writeInvalidRequest(w, r, "every segment needs a key")
			return nil, false
		}
		if _, duplicate := seen[key]; duplicate {
			s.writeInvalidRequest(w, r, "segments must not repeat the key "+key)
			return nil, false
		}
		seen[key] = struct{}{}
		if input.SizeBytes < 0 {
			s.writeInvalidRequest(w, r, "size_bytes must not be negative")
			return nil, false
		}
		segment := domain.Segment{Key: key, SizeBytes: input.SizeBytes}
		if input.Seq == nil {
			ordered = false
		} else {
			if *input.Seq < 0 {
				s.writeInvalidRequest(w, r, "seq must not be negative")
				return nil, false
			}
			segment.Seq = *input.Seq
		}
		segments = append(segments, segment)
	}
	if ordered {
		sort.SliceStable(segments, func(i, j int) bool { return segments[i].Seq < segments[j].Seq })
	}
	return segments, true
}

// streamPrefix is the filehouse prefix every segment object of one recording
// lives under (contract §1).
func streamPrefix(deviceID, streamID string) string {
	return deviceID + "/streams/" + streamID + "/"
}

// listStreamSegments lists the segment objects of a recording and orders them
// by the flush timestamp and first frame sequence number parsed from their key
// names, which is the recorder's own order (contract §1). It writes the failure
// itself, so the caller only checks ok.
func (s *Server) listStreamSegments(w http.ResponseWriter, r *http.Request, bucket, prefix string) ([]domain.Segment, bool) {
	if s.deps.Filehouse == nil {
		s.writeSourceUnavailable(w, r, "the filehouse client is not configured")
		return nil, false
	}
	objects, err := s.deps.Filehouse.ListObjects(r.Context(), bucket, prefix)
	if err != nil {
		s.writeSourceUnavailable(w, r, "listing the segments under "+prefix+" failed")
		s.log.Warn("list stream segments failed", "bucket", bucket, "prefix", prefix, "error", err)
		return nil, false
	}
	if len(objects) == 0 {
		s.writeInvalidRequest(w, r, "no segment object exists under prefix "+prefix)
		return nil, false
	}
	listed := make([]listedSegment, 0, len(objects))
	for _, object := range objects {
		flush, seq, err := parseSegmentKey(object.Key)
		if err != nil {
			s.writeSourceUnavailable(w, r, "the object "+object.Key+" does not carry a segment key name")
			s.log.Warn("unparseable segment object", "bucket", bucket, "key", object.Key, "error", err)
			return nil, false
		}
		listed = append(listed, listedSegment{
			segment: domain.Segment{Key: object.Key, Seq: seq, SizeBytes: object.SizeBytes},
			flush:   flush,
		})
	}
	sort.SliceStable(listed, func(i, j int) bool {
		if listed[i].flush.Equal(listed[j].flush) {
			return listed[i].segment.Seq < listed[j].segment.Seq
		}
		return listed[i].flush.Before(listed[j].flush)
	})
	segments := make([]domain.Segment, 0, len(listed))
	for _, entry := range listed {
		segments = append(segments, entry.segment)
	}
	return segments, true
}

// parseSegmentKey splits the recorder's key name
// {YYYYMMDDTHHMMSS}_{first_seq}.bin into the flush timestamp and the first
// frame's sequence number (contract §1).
func parseSegmentKey(key string) (time.Time, int64, error) {
	name := strings.TrimSuffix(path.Base(key), segmentKeySuffix)
	stamp, rawSeq, found := strings.Cut(name, "_")
	if !found {
		return time.Time{}, 0, errors.New("key name carries no first_seq")
	}
	flush, err := time.Parse(segmentKeyLayout, stamp)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("key name carries no flush timestamp: %w", err)
	}
	seq, err := strconv.ParseInt(rawSeq, 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("key name carries no numeric first_seq: %w", err)
	}
	return flush, seq, nil
}

// supportedCodec reports whether the create body's codec member names a codec
// this service merges. An empty member selects no explicit codec and is valid:
// the engine sniffs the codec from the first frame instead.
func supportedCodec(raw string) (ffmpeg.Codec, bool) {
	codec := ffmpeg.Codec(strings.ToLower(strings.TrimSpace(raw)))
	if codec == "" {
		return "", true
	}
	for _, supported := range ffmpeg.SupportedCodecs() {
		if codec == supported {
			return codec, true
		}
	}
	return "", false
}

// unsupportedCodecReason explains an unsupported codec. The recorder's
// vocabulary includes vp8 and vp9, whose raw elementary streams have no ffmpeg
// demuxer, so they get their own sentence (contract §1).
func unsupportedCodecReason(raw string) string {
	codec := strings.ToLower(strings.TrimSpace(raw))
	reason := fmt.Sprintf("codec %q is not supported; supported codecs: %s", codec, codecNames())
	if codec == "vp8" || codec == "vp9" {
		reason += fmt.Sprintf("; the recorder's %s streams are raw elementary streams without an ffmpeg demuxer", codec)
	}
	return reason
}

// codecNames lists the supported codecs as a comma-separated sentence.
func codecNames() string {
	codecs := ffmpeg.SupportedCodecs()
	names := make([]string, 0, len(codecs))
	for _, codec := range codecs {
		names = append(names, string(codec))
	}
	return strings.Join(names, ", ")
}

// defaultDestKey builds merged/YYYY/MM/DD/<id><ext> for a create body without
// dest.key. The partition is the UTC day the job was accepted and the
// extension is the requested codec's container, .mp4 when the body names no
// codec (contract §4).
func defaultDestKey(id string, codec ffmpeg.Codec) string {
	extension := defaultDestExtension
	if codec != "" {
		if format, ok := ffmpeg.FormatFor(codec, false); ok {
			extension = format.Extension
		}
	}
	return "merged/" + time.Now().UTC().Format("2006/01/02") + "/" + id + extension
}

// knownJobStatus reports whether status is one of the documented job states.
func knownJobStatus(status domain.Status) bool {
	switch status {
	case domain.StatusQueued, domain.StatusRunning, domain.StatusSucceeded, domain.StatusFailed, domain.StatusCanceled:
		return true
	default:
		return false
	}
}
