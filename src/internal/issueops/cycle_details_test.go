package issueops_test

// sk-1007 US-4, matrix row No. 42. Reference — spec US-4 and INV-5, plan K7:
// a cycle refusal carries the key "path" only when the store gave a path;
// without one the key is absent (neither null nor an empty list), and the
// code and message are those of any cycle refusal. Each builder of the
// refusal in this package is checked through its exported entry point.

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/dependencies"
	"skein-api/internal/issueops"
	"skein-api/internal/issues"
)

func TestCycleDetails_PathKeyOnlyWithAPath(t *testing.T) {
	ctx := context.Background()
	builders := []struct {
		name  string
		build func(err error) (code, message string, details map[string]any)
	}{
		{"ClassifyDepStoreError", func(err error) (string, string, map[string]any) {
			c := issueops.ClassifyDepStoreError(err, "A-1", "A-2", "blocks")
			if c == nil {
				t.Fatalf("ClassifyDepStoreError(%v) = nil, want a class", err)
			}
			return c.Code, c.Message, c.Details
		}},
		{"MapInlineDepError", func(err error) (string, string, map[string]any) {
			f := issueops.MapInlineDepError(ctx, &issues.ErrInlineDepFailed{Index: 0, ToID: "A-2", Type: "blocks", Err: err},
				uuid.New(), uuid.New())
			return f.Code, f.Message, f.Fields
		}},
		{"MapReparentError", func(err error) (string, string, map[string]any) {
			f := issueops.MapReparentError(ctx, err, uuid.New(), "A-1", "A-2")
			return f.Code, f.Message, f.Fields
		}},
	}
	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			hiddenCode, hiddenMsg, hidden := b.build(&dependencies.ErrDepCycle{Path: nil})
			if hiddenCode != "dependency_cycle" {
				t.Errorf("code without a path = %q, want dependency_cycle", hiddenCode)
			}
			if v, ok := hidden["path"]; ok {
				t.Errorf("details without a path carry path = %#v, want no key", v)
			}

			visibleCode, visibleMsg, visible := b.build(&dependencies.ErrDepCycle{Path: []string{"A-1", "A-2", "A-1"}})
			if visibleCode != "dependency_cycle" {
				t.Errorf("code with a path = %q, want dependency_cycle", visibleCode)
			}
			if got := visible["path"]; !reflect.DeepEqual(got, []string{"A-1", "A-2", "A-1"}) {
				t.Errorf("path = %#v, want [A-1 A-2 A-1]", got)
			}
			if hiddenMsg != visibleMsg {
				t.Errorf("message without a path %q differs from the message with one %q", hiddenMsg, visibleMsg)
			}
		})
	}
}
