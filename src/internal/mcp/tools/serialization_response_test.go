package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/issues"
)

func TestIssueWriteErrors_DistinguishRetryFromVersionConflict(t *testing.T) {
	ctx := context.Background()
	surfaces := []struct {
		name string
		call func(error) (string, map[string]any)
	}{
		{"close", func(err error) (string, map[string]any) {
			body := extractToolJSON(t, mapCloseError(ctx, nil, uuid.Nil, err, "skein_close", "user", "demo-1", ""))
			return body["error"].(string), body
		}},
		{"close_many", func(err error) (string, map[string]any) {
			got := classifyCloseRowError(ctx, ctx, nil, uuid.Nil, err, "user", "demo-1", "")
			return got.Code, got.Details
		}},
		{"update", func(err error) (string, map[string]any) {
			body := extractToolJSON(t, mapUpdateError(ctx, err, uuid.Nil, "demo-1", ""))
			return body["error"].(string), body
		}},
		{"update_many", func(err error) (string, map[string]any) {
			got := classifyUpdateError(ctx, err, "skein_update_many", "user", "demo-1")
			return got.Code, got.Details
		}},
		{"accept", func(err error) (string, map[string]any) {
			body := extractToolJSON(t, mapAcceptError(ctx, err, uuid.Nil, "demo-1"))
			return body["error"].(string), body
		}},
	}
	for _, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			code, details := surface.call(fmt.Errorf("write aborted: %w", issues.ErrSerializationRetry))
			if code != "serialization_retry" {
				t.Fatalf("code = %q, want serialization_retry", code)
			}
			if _, exists := details["currentVersion"]; exists {
				t.Fatal("retryable abort must not claim a conflicting version")
			}
			code, details = surface.call(&issues.ErrVersionConflict{CurrentVersion: 9})
			if code != "version_conflict" || fmt.Sprint(details["currentVersion"]) != "9" {
				t.Fatalf("genuine mismatch = %s %v, want version_conflict with currentVersion 9", code, details)
			}
		})
	}
}
