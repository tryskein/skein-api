package attachments

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"

	"skein-api/internal/storage"
)

// ErrStorageDisabled is surfaced when an endpoint is called but
// S3 was not configured (S3_URL empty). REST handler maps this
// to 503 attachments_unavailable.
var ErrStorageDisabled = errors.New("attachments disabled: S3 not configured")

// PostPolicy describes the data the browser needs to upload to S3.
// Mirrors UploadURLResponse but lives here so the store can produce the
// shape without importing the wire-DTO package back into itself.
type PostPolicy struct {
	URL    string
	Fields map[string]string
}

// S3Access is the narrow interface the Store + cleanup goroutine
// require from S3. Letting them depend on the interface (rather than
// *storage.S3Client directly) keeps the package testable without
// spinning up a real S3 container.
type S3Access interface {
	// PresignedPostPolicy mints a multipart-POST policy with size +
	// content-type bindings. Returns the URL the browser POSTs to and
	// the form-data fields it must include.
	PresignedPostPolicy(ctx context.Context, key, contentType string, sizeBytes int64, ttl time.Duration) (PostPolicy, error)
	// PresignedGetURL mints a presigned GET URL the browser can open
	// directly. ttl bounds the lifetime; the URL is signed with the
	// API's credentials so the bucket can stay private.
	PresignedGetURL(ctx context.Context, key, downloadFilename string, ttl time.Duration) (string, error)
	// RemoveObject hard-deletes the object. Best-effort: S3 returns
	// nil for "object not found" too, which is exactly what the cleanup
	// path wants.
	RemoveObject(ctx context.Context, key string) error
	// StatObject returns nil if the object exists, an error otherwise.
	// Used by the confirm path to verify the upload actually landed
	// before we flip status='ready'.
	StatObject(ctx context.Context, key string) error
	// ListKeys streams every object key under prefix to the callback.
	// Used by the cleanup goroutine for orphan reconciliation.
	ListKeys(ctx context.Context, prefix string, fn func(key string) error) error
}

// S3ClientAdapter wraps *storage.S3Client with the S3Access
// interface. Lives here so storage.S3Client stays a generic SDK
// wrapper not coupled to the attachments domain.
type S3ClientAdapter struct {
	C *storage.S3Client
}

// PresignedPostPolicy mints a multipart-POST policy. We use the SDK's
// PostPolicy machinery to bind the key, content-type, and exact size —
// S3 rejects any POST that violates these conditions, so the browser
// cannot upload a different file than the one we authorised.
func (a *S3ClientAdapter) PresignedPostPolicy(ctx context.Context, key, contentType string, sizeBytes int64, ttl time.Duration) (PostPolicy, error) {
	if a == nil || a.C == nil {
		return PostPolicy{}, ErrStorageDisabled
	}
	policy := minio.NewPostPolicy()
	if err := policy.SetBucket(a.C.Bucket); err != nil {
		return PostPolicy{}, fmt.Errorf("policy SetBucket: %w", err)
	}
	if err := policy.SetKey(key); err != nil {
		return PostPolicy{}, fmt.Errorf("policy SetKey: %w", err)
	}
	if err := policy.SetExpires(time.Now().UTC().Add(ttl)); err != nil {
		return PostPolicy{}, fmt.Errorf("policy SetExpires: %w", err)
	}
	if err := policy.SetContentType(contentType); err != nil {
		return PostPolicy{}, fmt.Errorf("policy SetContentType: %w", err)
	}
	// Bind the EXACT size: SetContentLengthRange enforces inclusive
	// [min, max] bounds at S3. Same value top + bottom = exact-size
	// binding — defends against the browser uploading a much larger
	// file than declared (a way to bypass the API-level cap).
	if err := policy.SetContentLengthRange(sizeBytes, sizeBytes); err != nil {
		return PostPolicy{}, fmt.Errorf("policy SetContentLengthRange: %w", err)
	}
	// PresignedPostPolicy signs the policy locally — BUT only when the
	// client's `Region` is known. If Region is unset, minio-go v7 first
	// issues `GET /{bucket}/?location=` to discover the region for SigV4,
	// which means a network round-trip to the client's endpoint. For
	// `a.C.Public` that endpoint is the browser-facing host (e.g.
	// `localhost:9000`), unreachable from inside this container in dev →
	// `connection refused` → presign fails with `internal_error`.
	//
	// We pin the region (S3_REGION) on the Public client in
	// internal/storage/s3.go to keep this path 100% local. If you
	// change the client construction, preserve that invariant.
	//
	// We use the public client so the resulting URL embeds the
	// browser-reachable host (S3_PUBLIC_URL).
	rawURL, fields, err := a.C.Public.PresignedPostPolicy(ctx, policy)
	if err != nil {
		return PostPolicy{}, fmt.Errorf("minting presigned post: %w", err)
	}
	return PostPolicy{URL: rawURL.String(), Fields: fields}, nil
}

// PresignedGetURL signs a GET URL. We force a downloadable
// Content-Disposition header so the browser saves the file under the
// original filename instead of the UUID-laden S3 key.
func (a *S3ClientAdapter) PresignedGetURL(ctx context.Context, key, downloadFilename string, ttl time.Duration) (string, error) {
	if a == nil || a.C == nil {
		return "", ErrStorageDisabled
	}
	reqParams := make(url.Values)
	if downloadFilename != "" {
		// RFC 6266 / 5987: emit an ASCII-safe `filename="…"` fallback
		// AND a `filename*=UTF-8''<percent-encoded>` parameter so
		// browsers pick the percent-encoded UTF-8 form when available
		// and fall back to the ASCII version (with non-ASCII chars
		// replaced) for older clients. Without this the bare quoted
		// UTF-8 form is technically illegal and many browsers degrade
		// to the URL-final segment of the S3 key (the UUID).
		ascii := asciiFallbackFilename(downloadFilename)
		encoded := rfc5987Encode(downloadFilename)
		disposition := fmt.Sprintf(
			`attachment; filename="%s"; filename*=UTF-8''%s`,
			escapeForHeader(ascii), encoded)
		reqParams.Set("response-content-disposition", disposition)
	}
	rawURL, err := a.C.Public.PresignedGetObject(ctx, a.C.Bucket, key, ttl, reqParams)
	if err != nil {
		return "", fmt.Errorf("minting presigned get: %w", err)
	}
	return rawURL.String(), nil
}

// RemoveObject asks S3 to delete the object. Errors that indicate
// "already gone" are swallowed by the SDK, so the caller can treat any
// non-nil error as a real fault.
func (a *S3ClientAdapter) RemoveObject(ctx context.Context, key string) error {
	if a == nil || a.C == nil {
		return ErrStorageDisabled
	}
	return a.C.Internal.RemoveObject(ctx, a.C.Bucket, key, minio.RemoveObjectOptions{})
}

// StatObject probes for object existence. Returns nil iff the object is
// present; any other condition (missing, denied, unreachable) is an
// error the caller can branch on.
func (a *S3ClientAdapter) StatObject(ctx context.Context, key string) error {
	if a == nil || a.C == nil {
		return ErrStorageDisabled
	}
	_, err := a.C.Internal.StatObject(ctx, a.C.Bucket, key, minio.StatObjectOptions{})
	return err
}

// ListKeys walks every object key under the prefix. The SDK returns a
// channel; we drain it to match the synchronous fn-callback shape used
// by the cleanup goroutine.
func (a *S3ClientAdapter) ListKeys(ctx context.Context, prefix string, fn func(key string) error) error {
	if a == nil || a.C == nil {
		return ErrStorageDisabled
	}
	for obj := range a.C.Internal.ListObjects(ctx, a.C.Bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return fmt.Errorf("listing objects: %w", obj.Err)
		}
		if err := fn(obj.Key); err != nil {
			return err
		}
	}
	return nil
}

// escapeForHeader removes characters that would break a quoted-string
// HTTP header value. This is intentionally minimal: S3 enforces UTF-
// 8 in response-content-disposition but unquoted CR/LF would terminate
// the header.
func escapeForHeader(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '"', '\\':
			// Escape backslash + double-quote so the surrounding
			// quotes don't get terminated.
			out = append(out, '\\', r)
		case '\r', '\n':
			// Header continuation is an attack vector — drop.
			continue
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// asciiFallbackFilename builds the `filename="…"` value for old clients
// that don't grok RFC 5987. Non-ASCII runes collapse to '_' and CR/LF
// are dropped; quotes are escaped by escapeForHeader at the caller.
// Empty result falls back to "download" so we never emit an empty
// filename parameter.
func asciiFallbackFilename(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\r' || r == '\n':
			// Drop — header injection vector.
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "download"
	}
	return out
}

// rfc5987Encode percent-encodes a UTF-8 string per RFC 5987 §3.2.1.
// `attr-char` (the unreserved set for ext-value) is ALPHA / DIGIT /
// "!#$&+-.^_`|~". Everything else — including space, "*", "'", "%" —
// is percent-encoded as %HH on its UTF-8 bytes. Cannot use url.QueryEscape:
// that encodes space as "+" (wrong) and decodes "/" without escaping it.
func rfc5987Encode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isAttrChar(c) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// isAttrChar implements the RFC 5987 `attr-char` production. Returns
// true iff c is safe to emit verbatim inside an ext-value.
func isAttrChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}
