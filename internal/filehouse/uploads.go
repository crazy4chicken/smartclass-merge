package filehouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// PartResult is one staged part of a multipart upload: the number filehouse
// recorded, its byte count and its SHA-256.
type PartResult struct {
	PartNo    int
	SizeBytes int64
	SHA256    string
}

// initiateUploadRequest is the body of the multipart creation call. size is a
// pointer so a caller without one declares nothing instead of declaring zero.
type initiateUploadRequest struct {
	Key         string `json:"key"`
	ContentType string `json:"content_type,omitempty"`
	Size        *int64 `json:"size,omitempty"`
}

// initiateUploadResponse is the reply of a multipart upload creation.
type initiateUploadResponse struct {
	UploadID  string    `json:"upload_id"`
	ExpiresAt time.Time `json:"expires_at"`
	PartCount int       `json:"part_count"`
}

// uploadPartResponse is the reply of one staged part.
type uploadPartResponse struct {
	UploadID string `json:"upload_id"`
	PartNo   int    `json:"part_no"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

// completeUploadRequest is the body of the completion call.
type completeUploadRequest struct {
	Parts []completeUploadPart `json:"parts"`
}

// completeUploadPart names one staged part together with the digest the client
// computed for its bytes; filehouse compares it case-insensitively against the
// digest it staged, so a corrupted transfer fails the completion instead of
// being assembled.
type completeUploadPart struct {
	PartNo int    `json:"part_no"`
	SHA256 string `json:"sha256"`
}

// StartMultipart creates a multipart upload and returns its id. contentType may
// be empty, which stores the filehouse default; a positive size is declared so
// filehouse can authorize the completion up front.
func (c *Client) StartMultipart(ctx context.Context, bucket, key, contentType string, size int64) (string, error) {
	request := initiateUploadRequest{Key: key, ContentType: contentType}
	if size > 0 {
		request.Size = &size
	}
	var out initiateUploadResponse
	err := c.call(ctx, apiRequest{
		action:   "create upload",
		method:   http.MethodPost,
		path:     uploadsPath(bucket),
		jsonBody: request,
		out:      &out,
		policy:   policySingle,
	})
	if err != nil {
		return "", err
	}
	if out.UploadID == "" {
		return "", errors.New("filehouse: create upload answered without an upload id")
	}
	return out.UploadID, nil
}

// PutPart stages one part of an upload. The bytes are streamed, so the call is
// never retried and an invalid token is surfaced instead of resending a
// half-consumed body. The returned digest is the server's own hash of the bytes
// it staged.
func (c *Client) PutPart(ctx context.Context, bucket, uploadID string, partNo int, body io.Reader) (PartResult, error) {
	out, _, err := writeJSON[uploadPartResponse](ctx, c, apiRequest{
		action: "put upload part",
		method: http.MethodPut,
		path:   partsPath(bucket, uploadID, partNo),
		stream: body,
		policy: policySingle,
	})
	if err != nil {
		return PartResult{}, err
	}
	return PartResult{PartNo: out.PartNo, SizeBytes: out.Size, SHA256: out.SHA256}, nil
}

// CompleteMultipart assembles the staged parts into bucket/key and returns what
// filehouse stored. key is checked against the object the server reports, so a
// mismatched route cannot publish the recording elsewhere unnoticed.
func (c *Client) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []PartResult) (PutResult, error) {
	request := completeUploadRequest{Parts: make([]completeUploadPart, 0, len(parts))}
	for _, part := range parts {
		request.Parts = append(request.Parts, completeUploadPart{PartNo: part.PartNo, SHA256: part.SHA256})
	}
	row, header, err := writeJSON[objectRow](ctx, c, apiRequest{
		action:   "complete upload",
		method:   http.MethodPost,
		path:     completePath(bucket, uploadID),
		jsonBody: request,
		policy:   policySingle,
	})
	if err != nil {
		return PutResult{}, err
	}
	if row.Key != "" && row.Key != key {
		return PutResult{}, fmt.Errorf("filehouse: complete upload stored key %q instead of %q", row.Key, key)
	}
	return putResult(row, header), nil
}

// AbortMultipart discards an upload and its staged parts. It is the cleanup of a
// failed multipart upload: an upload that is already gone answers
// upload_not_found or upload_expired, which the caller treats as done, because
// the staging area it wanted removed no longer exists.
func (c *Client) AbortMultipart(ctx context.Context, bucket, uploadID string) error {
	return c.call(ctx, apiRequest{
		action: "abort upload",
		method: http.MethodDelete,
		path:   uploadPath(bucket, uploadID),
		policy: policySingle,
	})
}

// uploadsPath is the multipart collection route of one bucket.
func uploadsPath(bucket string) string {
	return "/api/v1/buckets/" + url.PathEscape(bucket) + "/uploads"
}

// uploadPath is one multipart upload.
func uploadPath(bucket, uploadID string) string {
	return uploadsPath(bucket) + "/" + url.PathEscape(uploadID)
}

// partsPath is the part route of one multipart upload.
func partsPath(bucket, uploadID string, partNo int) string {
	return uploadPath(bucket, uploadID) + "/parts/" + strconv.Itoa(partNo)
}

// completePath is the completion route of one multipart upload.
func completePath(bucket, uploadID string) string {
	return uploadPath(bucket, uploadID) + "/complete"
}
