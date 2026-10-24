package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/cors"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config is the subset of config.Config the S3 client needs. Kept
// as a struct (rather than passing config.Config directly) so the storage
// package does not import config — keeps the dep graph one-way.
type S3Config struct {
	// Endpoint accepts either "host:port" or a full URL (scheme is
	// stripped before handoff to minio-go, which expects the bare
	// host:port form).
	Endpoint string
	// PublicEndpoint is the host:port the BROWSER uses (presigned URLs
	// embed it). Empty falls back to Endpoint. See config.S3PublicURL
	// for the rationale.
	PublicEndpoint string
	AccessKey      string
	SecretKey      string
	Bucket         string
	UseSSL         bool
	// PublicUseSSL controls the scheme of presigned URLs returned to the
	// browser. May differ from UseSSL when the in-cluster connection is
	// HTTP but the browser sees HTTPS via ingress.
	PublicUseSSL bool
	// Region both clients sign with. Pinned (never discovered) so presigning
	// stays a local computation: without it minio-go asks the endpoint for
	// the bucket location, and for the browser-facing client that endpoint is
	// not reachable from inside this process.
	Region string
	// CORSOrigins are the browser origins EnsureBucket sets the bucket CORS
	// rule for. Empty leaves the bucket CORS untouched.
	CORSOrigins []string
}

// S3Client bundles the SDK client + the public-URL variant used to
// rewrite presigned URLs the browser will actually hit. The two clients
// share credentials but differ only in their endpoint and TLS settings.
type S3Client struct {
	// Internal is the client speaking to the in-cluster endpoint. Used
	// for bucket admin (EnsureBucket, RemoveObject, StatObject).
	Internal *minio.Client
	// Public is the client used to MINT presigned URLs for the browser.
	// When Public == Internal there is no rewrite path — both URLs use
	// the same endpoint.
	Public *minio.Client
	// Bucket is the single bucket every attachment lives in.
	Bucket string
	// CORSOrigins is copied from S3Config; see there.
	CORSOrigins []string
}

// ErrEmptyRegion is returned by NewS3Client when no region is configured.
var ErrEmptyRegion = errors.New("S3 region is empty")

// BucketNotCreatableError says the bucket is missing and the S3 key may not
// create buckets, so the operator has to create it and give the key read and
// write access (Garage, SeaweedFS with a non-admin key). Err is the storage's
// own answer.
type BucketNotCreatableError struct {
	Bucket string
	Err    error
}

func (e *BucketNotCreatableError) Error() string {
	return fmt.Sprintf("bucket %s does not exist and the S3 key may not create buckets: "+
		"create the bucket and grant the key read and write access to it: %v", e.Bucket, e.Err)
}

func (e *BucketNotCreatableError) Unwrap() error { return e.Err }

// NewS3Client constructs an SDK client + verifies credentials by
// listing buckets. Returns nil + error when the endpoint is unreachable
// or the credentials are wrong — caller treats this as fatal at startup.
//
// Endpoint normalisation: minio-go's New() takes a bare "host:port" with
// the scheme controlled by the secure flag. We accept full URLs (scheme
// embedded) and strip the prefix here.
func NewS3Client(ctx context.Context, cfg S3Config) (*S3Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("S3 endpoint is empty")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("S3 access key or secret key is empty")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("S3 bucket name is empty")
	}

	internalEP, internalSSL, err := normalizeEndpoint(cfg.Endpoint, cfg.UseSSL)
	if err != nil {
		return nil, fmt.Errorf("parsing S3_URL %q: %w", cfg.Endpoint, err)
	}

	// Region is set explicitly on BOTH clients. Without it, minio-go v7
	// signs SigV4 requests with an unknown region — and at the first
	// presign-policy call it issues `GET /{bucket}/?location=` over HTTP
	// to look it up. For the Public client (pointed at the browser-facing
	// `localhost:9000`) that GET runs from INSIDE the api container,
	// where `localhost:9000` is not S3 → `connection refused` → the
	// whole presign fails with `internal_error`. Pinning the region
	// keeps presign 100% local: zero HTTP round-trips.
	//
	// The region is S3_REGION and has to match what the storage is
	// configured with (Garage s3_region, RustFS RUSTFS_REGION).
	if cfg.Region == "" {
		return nil, ErrEmptyRegion
	}

	internal, err := minio.New(internalEP, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: internalSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("constructing internal s3 client: %w", err)
	}

	// A second client points at the browser-facing endpoint and is used
	// only for presigning. Region MUST be set here too — without it,
	// presigning falls back to a network round-trip to the public
	// endpoint, which is unreachable from inside this container in dev.
	publicEP := internalEP
	publicSSL := internalSSL
	if cfg.PublicEndpoint != "" {
		publicEP, publicSSL, err = normalizeEndpoint(cfg.PublicEndpoint, cfg.PublicUseSSL)
		if err != nil {
			return nil, fmt.Errorf("parsing S3_PUBLIC_URL %q: %w", cfg.PublicEndpoint, err)
		}
	}
	public, err := minio.New(publicEP, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: publicSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("constructing public s3 client: %w", err)
	}

	c := &S3Client{Internal: internal, Public: public, Bucket: cfg.Bucket, CORSOrigins: cfg.CORSOrigins}

	// ListBuckets is the cheapest call that exercises auth + reach.
	// We do this with a short timeout — if S3 is down at startup the
	// operator wants a clear error, not a hang.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := internal.ListBuckets(probeCtx); err != nil {
		return nil, fmt.Errorf("probing s3 at %s: %w", internalEP, err)
	}

	return c, nil
}

// EnsureBucket makes the configured bucket usable: it creates the bucket
// when it is missing, then sets the browser CORS rule when origins are
// configured. Returns nil when the bucket already exists.
//
// Storages that keep bucket creation for administrators (Garage, SeaweedFS
// with a non-admin key) answer AccessDenied to MakeBucket; the error then
// says what the operator has to do instead of surfacing the bare S3 code.
func (c *S3Client) EnsureBucket(ctx context.Context) error {
	exists, err := c.Internal.BucketExists(ctx, c.Bucket)
	if err != nil {
		return fmt.Errorf("checking bucket %s: %w", c.Bucket, err)
	}
	if !exists {
		if err := c.Internal.MakeBucket(ctx, c.Bucket, minio.MakeBucketOptions{}); err != nil {
			// Race: another replica created it between BucketExists and
			// MakeBucket. minio-go returns a typed error; we treat the
			// "already owned by you" case as success. errors.As unwraps
			// so wrapped ErrorResponse still matches.
			var errResp minio.ErrorResponse
			if errors.As(err, &errResp) {
				switch errResp.Code {
				case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
					return c.ensureCORS(ctx)
				case "AccessDenied":
					return &BucketNotCreatableError{Bucket: c.Bucket, Err: err}
				}
			}
			return fmt.Errorf("creating bucket %s: %w", c.Bucket, err)
		}
		slog.Info("s3 bucket ensured", "bucket", c.Bucket)
	}
	return c.ensureCORS(ctx)
}

// ensureCORS sets the bucket CORS rule for the configured origins. The
// browser POSTs the file to the storage and fetches it back from there, both
// cross-origin, so a storage that does not reflect Origin on its own needs
// the rule. A failure is logged and does not stop the start: an operator may
// have configured CORS by hand, and the API is useful without uploads.
func (c *S3Client) ensureCORS(ctx context.Context) error {
	if len(c.CORSOrigins) == 0 {
		return nil
	}
	err := c.Internal.SetBucketCors(ctx, c.Bucket, buildCORSConfig(c.CORSOrigins))
	if err == nil {
		slog.Info("s3 bucket cors set", "bucket", c.Bucket, "origins", c.CORSOrigins)
		return nil
	}
	var errResp minio.ErrorResponse
	if errors.As(err, &errResp) && (errResp.Code == "NotImplemented" || errResp.StatusCode == http.StatusNotImplemented) {
		slog.Info("s3 storage has no bucket CORS API; CORS is up to the storage configuration",
			"bucket", c.Bucket)
		return nil
	}
	slog.Warn("s3 bucket cors was not set; browser uploads and downloads fail until CORS is configured",
		"bucket", c.Bucket, "origins", c.CORSOrigins, "error", err.Error())
	return nil
}

// buildCORSConfig is the CORS rule the browser needs: the multipart POST of
// the upload, the GET of the download and the preflight in front of them.
// Content-Disposition is exposed so the browser sees the file name the
// download URL asks for.
func buildCORSConfig(origins []string) *cors.Config {
	return cors.NewConfig([]cors.Rule{{
		ID:            "skein-attachments",
		AllowedOrigin: origins,
		AllowedMethod: []string{"GET", "HEAD", "POST", "PUT"},
		AllowedHeader: []string{"*"},
		ExposeHeader:  []string{"ETag", "Content-Disposition"},
		MaxAgeSeconds: 3000,
	}})
}

// HealthCheck pings S3 via a cheap auth-required call. Returns nil
// when the service is reachable and the credentials work. Used by the
// /ready handler.
func (c *S3Client) HealthCheck(ctx context.Context) error {
	// BucketExists is cheaper than ListBuckets for the same liveness
	// signal: it's a single HEAD bucket call. Auth is enforced server
	// side.
	if _, err := c.Internal.BucketExists(ctx, c.Bucket); err != nil {
		return fmt.Errorf("s3 bucket exists check: %w", err)
	}
	return nil
}

// normalizeEndpoint accepts either a full URL ("http://s3:9000") or a
// bare "host:port", returning the bare form plus a derived secure flag.
// The fallback flag is used when the input has no scheme.
func normalizeEndpoint(endpoint string, fallbackSSL bool) (string, bool, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false, errors.New("empty endpoint")
	}

	// Bare host:port — leave as-is.
	if !strings.Contains(endpoint, "://") {
		return endpoint, fallbackSSL, nil
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false, err
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("endpoint %q has no host", endpoint)
	}
	secure := u.Scheme == "https"
	return u.Host, secure, nil
}
