package merge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crazy4chicken/smartclass-merge/internal/filehouse"
)

const (
	// multipartAbortTimeout bounds the cleanup of a failed multipart upload. It
	// runs on a context detached from the job, so a canceled job still removes
	// the parts it staged.
	multipartAbortTimeout = 30 * time.Second
	// maxMultipartParts caps how many parts one upload may have. The completion
	// body is a JSON document filehouse caps at 1 MiB, so an object split into
	// far more parts than this could never be completed; failing before the
	// transfer names the misconfiguration instead of uploading for hours.
	maxMultipartParts = 10000
)

// upload transfers the merged file to its destination bucket and verifies what
// filehouse stored. A file at or below ObjectMaxBytes goes through one chunked
// PUT; a larger one uses a sequential multipart upload of PartBytes parts, which
// filehouse assembles server-side and which is not bound by the direct-PUT size
// cap.
func (e *Engine) upload(ctx context.Context, in RunInput, path string, size int64, rep *reporter) (filehouse.PutResult, error) {
	if size <= e.opts.ObjectMaxBytes {
		return e.putObject(ctx, in, path, size, rep)
	}
	return e.putMultipart(ctx, in, path, size, rep)
}

// putObject uploads the merged file with one PUT, hashing it in flight, and
// verifies the stored size and digest against the bytes that were sent: a
// truncated or corrupted transfer must not be recorded as a successful merge.
// The object carries the container's media type, exactly like the multipart
// path.
func (e *Engine) putObject(ctx context.Context, in RunInput, path string, size int64, rep *reporter) (filehouse.PutResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return filehouse.PutResult{}, fmt.Errorf("upload: open merged file: %w", err)
	}
	defer file.Close()
	body := &uploadReader{
		r:    file,
		hash: sha256.New(),
		onRead: func(read int64) {
			rep.set(false, func(p *Progress) { p.UploadedBytes = read })
		},
	}
	result, err := e.filehouse.PutObject(ctx, in.DestBucket, in.DestKey, objectContentType(in.DestKey), body)
	if err != nil {
		return filehouse.PutResult{}, fmt.Errorf("upload %s/%s: %w", in.DestBucket, in.DestKey, err)
	}
	sum := hex.EncodeToString(body.hash.Sum(nil))
	stored := storedDigest(result)
	switch {
	case body.read != size:
		return filehouse.PutResult{}, fmt.Errorf("upload %s/%s: read %d of %d bytes from the merged file", in.DestBucket, in.DestKey, body.read, size)
	case result.SizeBytes != size:
		return filehouse.PutResult{}, fmt.Errorf("upload %s/%s: filehouse stored %d bytes, sent %d", in.DestBucket, in.DestKey, result.SizeBytes, size)
	case stored == "":
		return filehouse.PutResult{}, fmt.Errorf("upload %s/%s: filehouse answered without a digest", in.DestBucket, in.DestKey)
	case !strings.EqualFold(stored, sum):
		return filehouse.PutResult{}, fmt.Errorf("upload %s/%s: digest mismatch: filehouse stored %s, sent %s", in.DestBucket, in.DestKey, stored, sum)
	}
	return result, nil
}

// putMultipart uploads the merged file as sequential parts of PartBytes, checks
// every part against the digest filehouse computed while staging it, completes
// the upload and verifies the assembled size. A failure before completion aborts
// the upload, so no staged parts are left behind.
func (e *Engine) putMultipart(ctx context.Context, in RunInput, path string, size int64, rep *reporter) (filehouse.PutResult, error) {
	if e.opts.PartBytes <= 0 {
		return filehouse.PutResult{}, errors.New("merge: a multipart upload needs a positive part size (MERGE_MULTIPART_PART_BYTES)")
	}
	if parts := (size + e.opts.PartBytes - 1) / e.opts.PartBytes; parts > maxMultipartParts {
		return filehouse.PutResult{}, fmt.Errorf("merge: %d bytes in %d parts exceed the %d-part limit; raise MERGE_MULTIPART_PART_BYTES", size, parts, maxMultipartParts)
	}
	file, err := os.Open(path)
	if err != nil {
		return filehouse.PutResult{}, fmt.Errorf("upload: open merged file: %w", err)
	}
	defer file.Close()
	uploadID, err := e.filehouse.StartMultipart(ctx, in.DestBucket, in.DestKey, objectContentType(in.DestKey), size)
	if err != nil {
		return filehouse.PutResult{}, fmt.Errorf("start multipart upload %s/%s: %w", in.DestBucket, in.DestKey, err)
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), multipartAbortTimeout)
		defer cancel()
		if err := e.filehouse.AbortMultipart(abortCtx, in.DestBucket, uploadID); err != nil {
			e.logger.Warn("abort multipart upload failed",
				"job_id", in.JobID,
				"upload_id", uploadID,
				"error", err,
			)
		}
	}()

	var (
		parts    []filehouse.PartResult
		uploaded int64
	)
	for partNo := 1; uploaded < size; partNo++ {
		length := int64(e.opts.PartBytes)
		if remaining := size - uploaded; remaining < length {
			length = remaining
		}
		body := &uploadReader{
			r:    io.NewSectionReader(file, uploaded, length),
			hash: sha256.New(),
			onRead: func(read int64) {
				rep.set(false, func(p *Progress) { p.UploadedBytes = uploaded + read })
			},
		}
		part, err := e.filehouse.PutPart(ctx, in.DestBucket, uploadID, partNo, body)
		if err != nil {
			return filehouse.PutResult{}, fmt.Errorf("upload part %d of %s/%s: %w", partNo, in.DestBucket, in.DestKey, err)
		}
		sum := hex.EncodeToString(body.hash.Sum(nil))
		switch {
		case body.read != length:
			return filehouse.PutResult{}, fmt.Errorf("upload part %d: read %d of %d bytes from the merged file", partNo, body.read, length)
		case part.PartNo != partNo:
			return filehouse.PutResult{}, fmt.Errorf("upload part %d: filehouse recorded part %d", partNo, part.PartNo)
		case part.SizeBytes != length:
			return filehouse.PutResult{}, fmt.Errorf("upload part %d: filehouse staged %d bytes, sent %d", partNo, part.SizeBytes, length)
		case !strings.EqualFold(part.SHA256, sum):
			return filehouse.PutResult{}, fmt.Errorf("upload part %d: digest mismatch: filehouse staged %s, sent %s", partNo, part.SHA256, sum)
		}
		parts = append(parts, filehouse.PartResult{PartNo: partNo, SizeBytes: length, SHA256: sum})
		uploaded += length
	}
	result, err := e.filehouse.CompleteMultipart(ctx, in.DestBucket, in.DestKey, uploadID, parts)
	if err != nil {
		return filehouse.PutResult{}, fmt.Errorf("complete multipart upload %s/%s: %w", in.DestBucket, in.DestKey, err)
	}
	completed = true
	if result.SizeBytes != size {
		return filehouse.PutResult{}, fmt.Errorf("complete multipart upload %s/%s: filehouse stored %d bytes, sent %d", in.DestBucket, in.DestKey, result.SizeBytes, size)
	}
	return result, nil
}

// uploadReader hashes and counts everything read through it and reports the
// running total, so an upload can publish its progress. It is not safe for
// concurrent use: the HTTP client reads it from the calling goroutine.
type uploadReader struct {
	r      io.Reader
	hash   hash.Hash
	read   int64
	onRead func(int64)
}

// Read implements io.Reader.
func (u *uploadReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	if n > 0 {
		_, _ = u.hash.Write(p[:n])
		u.read += int64(n)
		if u.onRead != nil {
			u.onRead(u.read)
		}
	}
	return n, err
}

// countingReader counts the bytes read through it and reports the running total,
// so a merge can publish how much of the recording it has consumed.
type countingReader struct {
	r      io.Reader
	read   int64
	onRead func(int64)
}

// Read implements io.Reader.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.read += int64(n)
		if c.onRead != nil {
			c.onRead(c.read)
		}
	}
	return n, err
}

// storedDigest returns the bare digest filehouse reported for a stored object:
// the X-Filehouse-SHA256 value, or the ETag with its quotes stripped.
func storedDigest(result filehouse.PutResult) string {
	if result.SHA256 != "" {
		return strings.Trim(result.SHA256, `"`)
	}
	return strings.Trim(result.ETag, `"`)
}

// videoContentTypes maps the extension of a merged object onto its media type.
// filehouse stores the declared type as the object's content type, and the
// container ffmpeg wrote is the only source of truth here; the table is explicit
// because the runtime mime table varies by host.
var videoContentTypes = map[string]string{
	".mp4": "video/mp4",
	".mov": "video/quicktime",
}

// objectContentType names the media type of a merged object; an extension this
// service does not produce falls back to the generic binary type.
func objectContentType(key string) string {
	if contentType, ok := videoContentTypes[strings.ToLower(filepath.Ext(key))]; ok {
		return contentType
	}
	return "application/octet-stream"
}
