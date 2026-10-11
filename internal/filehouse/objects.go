package filehouse

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// headerSHA256 carries the bare lowercase digest of the stored bytes. The ETag
// response header is the same digest in its quoted form.
const headerSHA256 = "X-Filehouse-SHA256"

// Object is one stored object as the listing endpoint reports it.
type Object struct {
	Key       string
	SizeBytes int64
}

// objectRow is the wire form of one stored object; the client reads the key, the
// size and both digest spellings out of it.
type objectRow struct {
	Key      string `json:"key"`
	Size     int64  `json:"size"`
	BlobHash string `json:"blob_hash"`
	ETag     string `json:"etag"`
}

// objectPage is the pagination envelope of the object listing.
type objectPage struct {
	Items      []objectRow `json:"items"`
	NextCursor string      `json:"next_cursor"`
}

// PutResult is what filehouse stored for one upload: the byte count and the
// digest, in the bare (SHA256) and the quoted (ETag) spelling.
type PutResult struct {
	SizeBytes int64
	SHA256    string
	ETag      string
}

// ListObjects returns every object under prefix in key order, walking all pages
// of the listing (listPageLimit per request). The caller's context bounds the
// whole walk; each page is additionally bounded by the metadata timeout.
func (c *Client) ListObjects(ctx context.Context, bucket, prefix string) ([]Object, error) {
	var objects []Object
	cursor := ""
	for {
		query := url.Values{"limit": []string{strconv.Itoa(listPageLimit)}}
		if prefix != "" {
			query.Set("prefix", prefix)
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page objectPage
		err := c.call(ctx, apiRequest{
			action: "list objects",
			method: http.MethodGet,
			path:   objectsPath(bucket),
			query:  query,
			out:    &page,
			policy: policyGET,
		})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			objects = append(objects, Object{Key: item.Key, SizeBytes: item.Size})
		}
		if page.NextCursor == "" {
			return objects, nil
		}
		cursor = page.NextCursor
	}
}

// GetObject opens one object for streaming; the caller closes the returned
// reader. A missing object is an *Error unwrapping to ErrObjectNotFound. The job
// context bounds the transfer, so a long download is never cut off by the
// metadata timeout.
func (c *Client) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	body, err := c.download(ctx, apiRequest{
		action: "get object",
		method: http.MethodGet,
		path:   objectPath(bucket, key),
		policy: policyGET,
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// defaultContentType is the media type filehouse stores for a write that
// declares none. The client sends it explicitly, so the stored content type
// never depends on what the HTTP stack happens to add.
const defaultContentType = "application/octet-stream"

// PutObject stores body at bucket/key with one chunked PUT: no length and no
// digest are declared, so filehouse streams the body, enforces its own size cap
// and reports the digest it computed. contentType is stored as the object's
// content type, and an empty one stores defaultContentType. The caller hashes
// the bytes it sent and compares them with the returned result.
func (c *Client) PutObject(ctx context.Context, bucket, key, contentType string, body io.Reader) (PutResult, error) {
	if contentType == "" {
		contentType = defaultContentType
	}
	row, header, err := writeJSON[objectRow](ctx, c, apiRequest{
		action:      "put object",
		method:      http.MethodPut,
		path:        objectPath(bucket, key),
		contentType: contentType,
		stream:      body,
		policy:      policySingle,
	})
	if err != nil {
		return PutResult{}, err
	}
	return putResult(row, header), nil
}

// putResult merges an object row with the digest response headers. The headers
// win: they are the digest filehouse recorded for exactly the bytes it received,
// while the row carries the same value in its JSON spelling.
func putResult(row objectRow, header http.Header) PutResult {
	result := PutResult{SizeBytes: row.Size, SHA256: row.BlobHash, ETag: row.ETag}
	if header == nil {
		return result
	}
	if sum := header.Get(headerSHA256); sum != "" {
		result.SHA256 = sum
	}
	if etag := header.Get("ETag"); etag != "" {
		result.ETag = etag
	}
	return result
}

// objectsPath is the object collection route of one bucket.
func objectsPath(bucket string) string {
	return "/api/v1/buckets/" + url.PathEscape(bucket) + "/objects"
}

// objectPath is the object route of one key. A key is byte-preserved and may
// contain slashes, so every segment is escaped on its own: the separators stay
// literal separators, and filehouse decodes the wildcard remainder exactly once.
func objectPath(bucket, key string) string {
	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return objectsPath(bucket) + "/" + strings.Join(segments, "/")
}
