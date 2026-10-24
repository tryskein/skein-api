package storage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// The bucket start-up path is tested against a stub of the S3 HTTP boundary:
// what is asserted is what OUR code does with the answers — the error text an
// operator reads, and the CORS request it sends — not what a real storage does.

const s3ErrorXML = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`

// stubS3 answers the three bucket calls EnsureBucket makes and records the
// body of the CORS request.
type stubS3 struct {
	bucketExists bool
	makeStatus   int    // answer to MakeBucket when the bucket is missing
	makeCode     string // S3 error code for makeStatus
	corsStatus   int    // answer to PutBucketCors; 200 stores the rule
	corsCode     string

	mu       sync.Mutex
	corsBody string
	corsHits int
}

func (s *stubS3) handler(w http.ResponseWriter, r *http.Request) {
	_, isCORS := r.URL.Query()["cors"]
	switch {
	case r.Method == http.MethodHead && !isCORS:
		if s.bucketExists {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodPut && isCORS:
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.corsHits++
		s.corsBody = string(body)
		s.mu.Unlock()
		if s.corsStatus != http.StatusOK {
			w.WriteHeader(s.corsStatus)
			_, _ = io.WriteString(w, sprintfErr(s.corsCode))
			return
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut:
		if s.makeStatus == 0 || s.makeStatus == http.StatusOK {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(s.makeStatus)
		_, _ = io.WriteString(w, sprintfErr(s.makeCode))
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func sprintfErr(code string) string {
	return strings.NewReplacer("%s", code).Replace(s3ErrorXML)
}

// levelRecorder is a slog handler that keeps the level of every record.
type levelRecorder struct {
	mu     sync.Mutex
	levels []slog.Level
}

func (h *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.levels = append(h.levels, r.Level)
	return nil
}
func (h *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecorder) WithGroup(string) slog.Handler      { return h }

// maxLevel returns the highest level recorded, or slog.LevelDebug-1 if none.
func (h *levelRecorder) maxLevel() slog.Level {
	h.mu.Lock()
	defer h.mu.Unlock()
	max := slog.LevelDebug - 1
	for _, l := range h.levels {
		if l > max {
			max = l
		}
	}
	return max
}

// recordLevels routes slog to a levelRecorder for the test.
func recordLevels(t *testing.T) *levelRecorder {
	t.Helper()
	rec := &levelRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}

func newStubClient(t *testing.T, stub *stubS3, origins []string) *S3Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(srv.Close)
	cl, err := minio.New(strings.TrimPrefix(srv.URL, "http://"), &minio.Options{
		Creds:  credentials.NewStaticV4("key", "secret", ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}
	return &S3Client{Internal: cl, Public: cl, Bucket: "skein-attachments", CORSOrigins: origins}
}

func TestBuildCORSConfig(t *testing.T) {
	xml, err := buildCORSConfig([]string{"https://skein.example.com", "http://localhost:8080"}).ToXML()
	if err != nil {
		t.Fatalf("ToXML: %v", err)
	}
	got := string(xml)
	for _, want := range []string{
		"<AllowedOrigin>https://skein.example.com</AllowedOrigin>",
		"<AllowedOrigin>http://localhost:8080</AllowedOrigin>",
		"<AllowedMethod>POST</AllowedMethod>",
		"<AllowedMethod>GET</AllowedMethod>",
		"<AllowedMethod>HEAD</AllowedMethod>",
		"<AllowedHeader>*</AllowedHeader>",
		"<ExposeHeader>ETag</ExposeHeader>",
		"<ExposeHeader>Content-Disposition</ExposeHeader>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CORS rule lacks %s:\n%s", want, got)
		}
	}
}

// A storage whose key may not create buckets must report it as a typed
// error the operator-facing start-up code can recognise.
func TestEnsureBucket_AccessDeniedIsBucketNotCreatable(t *testing.T) {
	stub := &stubS3{makeStatus: http.StatusForbidden, makeCode: "AccessDenied"}
	err := newStubClient(t, stub, nil).EnsureBucket(context.Background())
	var notCreatable *BucketNotCreatableError
	if !errors.As(err, &notCreatable) {
		t.Fatalf("EnsureBucket = %v, want a *BucketNotCreatableError", err)
	}
	if notCreatable.Bucket != "skein-attachments" {
		t.Errorf("Bucket = %q, want skein-attachments", notCreatable.Bucket)
	}
	var storageErr minio.ErrorResponse
	if !errors.As(err, &storageErr) || storageErr.Code != "AccessDenied" {
		t.Errorf("the storage's own answer is not reachable through Unwrap: %v", err)
	}
}

func TestEnsureBucket_SetsCORSOnExistingBucket(t *testing.T) {
	stub := &stubS3{bucketExists: true, corsStatus: http.StatusOK}
	err := newStubClient(t, stub, []string{"https://skein.example.com"}).EnsureBucket(context.Background())
	if err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	if stub.corsHits != 1 {
		t.Fatalf("CORS requests = %d, want 1", stub.corsHits)
	}
	if !strings.Contains(stub.corsBody, "<AllowedOrigin>https://skein.example.com</AllowedOrigin>") {
		t.Errorf("CORS body lacks the configured origin:\n%s", stub.corsBody)
	}
}

func TestEnsureBucket_NoOriginsLeavesCORSAlone(t *testing.T) {
	stub := &stubS3{bucketExists: true, corsStatus: http.StatusOK}
	if err := newStubClient(t, stub, nil).EnsureBucket(context.Background()); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	if stub.corsHits != 0 {
		t.Errorf("CORS requests = %d, want 0 when no origin is configured", stub.corsHits)
	}
}

// A storage without a CORS API, and one that refuses the rule, must not
// stop the start: the operator may have configured CORS by hand. The two
// differ in how loudly they are logged: the first is expected (below
// warning), the second tells the operator uploads fail until CORS is set.
func TestEnsureBucket_CORSFailureDoesNotStop(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		code       string
		wantWarned bool
	}{
		{"not implemented", http.StatusNotImplemented, "NotImplemented", false},
		{"not implemented without a body code", http.StatusNotImplemented, "", false},
		{"access denied", http.StatusForbidden, "AccessDenied", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := recordLevels(t)
			stub := &stubS3{bucketExists: true, corsStatus: tc.status, corsCode: tc.code}
			err := newStubClient(t, stub, []string{"https://skein.example.com"}).EnsureBucket(context.Background())
			if err != nil {
				t.Fatalf("EnsureBucket = %v, want nil", err)
			}
			if stub.corsHits != 1 {
				t.Errorf("CORS requests = %d, want 1", stub.corsHits)
			}
			if warned := rec.maxLevel() >= slog.LevelWarn; warned != tc.wantWarned {
				t.Errorf("warning or worse logged = %v, want %v (max level %v)", warned, tc.wantWarned, rec.maxLevel())
			}
		})
	}
}

// A replica that loses the create race still ends with CORS set.
func TestEnsureBucket_CreateRaceStillSetsCORS(t *testing.T) {
	stub := &stubS3{
		makeStatus: http.StatusConflict, makeCode: "BucketAlreadyOwnedByYou",
		corsStatus: http.StatusOK,
	}
	err := newStubClient(t, stub, []string{"https://skein.example.com"}).EnsureBucket(context.Background())
	if err != nil {
		t.Fatalf("EnsureBucket = %v, want nil on BucketAlreadyOwnedByYou", err)
	}
	if stub.corsHits != 1 {
		t.Errorf("CORS requests = %d, want 1", stub.corsHits)
	}
}

// A bucket this call creates gets the CORS rule too.
func TestEnsureBucket_CreatedBucketGetsCORS(t *testing.T) {
	stub := &stubS3{makeStatus: http.StatusOK, corsStatus: http.StatusOK}
	err := newStubClient(t, stub, []string{"https://skein.example.com"}).EnsureBucket(context.Background())
	if err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	if stub.corsHits != 1 {
		t.Errorf("CORS requests = %d, want 1", stub.corsHits)
	}
}

func TestNewS3Client_RejectsEmptyRegion(t *testing.T) {
	_, err := NewS3Client(context.Background(), S3Config{
		Endpoint: "http://127.0.0.1:1", AccessKey: "k", SecretKey: "s", Bucket: "b",
	})
	if !errors.Is(err, ErrEmptyRegion) {
		t.Fatalf("NewS3Client without region = %v, want ErrEmptyRegion", err)
	}
}
