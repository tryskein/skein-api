package issueops

import (
	"fmt"
	"skein-api/internal/issues"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ParseCommitInputs is the shared parser for `commits[]` on
// skein_close and skein_close_many. fieldPrefix is prepended to error
// messages so callers can surface tool-specific paths (e.g.
// "commits[2].hash" for skein_close, "items[5].commits[0].hash" for
// skein_close_many).
//
// Returns ([]issues.CloseCommitInput, *Failure). On success the
// error is nil. On failure the slice is nil and the dto carries the
// classified validation_error envelope.
func ParseCommitInputs(raw any, fieldPrefix string) ([]issues.CloseCommitInput, *Failure) {
	if raw == nil {
		return nil, nil
	}
	rawSlice, ok := raw.([]any)
	if !ok {
		return nil, &Failure{
			Code:    ErrCodeValidationError,
			Message: fieldPrefix + " must be an array",
		}
	}
	if len(rawSlice) == 0 {
		return nil, nil
	}
	if len(rawSlice) > issues.MaxCommitsPerClose {
		return nil, &Failure{
			Code:    ErrCodeValidationError,
			Message: fmt.Sprintf("%s may carry at most %d items (got %d)", fieldPrefix, issues.MaxCommitsPerClose, len(rawSlice)),
			Fields:  map[string]any{"max": issues.MaxCommitsPerClose, "got": len(rawSlice)},
		}
	}

	out := make([]issues.CloseCommitInput, 0, len(rawSlice))
	for i, entry := range rawSlice {
		obj, ok := entry.(map[string]any)
		if !ok {
			return nil, &Failure{
				Code:    ErrCodeValidationError,
				Message: fmt.Sprintf("%s[%d] must be an object", fieldPrefix, i),
				Fields:  map[string]any{"field": fmt.Sprintf("%s[%d]", fieldPrefix, i)},
			}
		}

		hash, _ := obj["hash"].(string)
		hash = strings.TrimSpace(hash)
		if hash == "" {
			return nil, &Failure{
				Code:    ErrCodeValidationError,
				Message: fmt.Sprintf("%s[%d].hash is required", fieldPrefix, i),
				Fields:  map[string]any{"field": fmt.Sprintf("%s[%d].hash", fieldPrefix, i)},
			}
		}

		ci := issues.CloseCommitInput{Hash: hash}

		if v, ok := obj["repoUrl"].(string); ok {
			ci.RepoURL = strings.TrimSpace(v)
		}
		if v, ok := obj["repoId"].(string); ok {
			s := strings.TrimSpace(v)
			if s != "" {
				id, err := uuid.Parse(s)
				if err != nil {
					return nil, &Failure{
						Code:    ErrCodeValidationError,
						Message: fmt.Sprintf("%s[%d].repoId is not a valid UUID", fieldPrefix, i),
						Fields:  map[string]any{"field": fmt.Sprintf("%s[%d].repoId", fieldPrefix, i)},
					}
				}
				ci.RepoID = &id
			}
		}
		if v, ok := obj["url"].(string); ok {
			s := strings.TrimSpace(v)
			if s != "" {
				ci.URL = &s
			}
		}
		if v, ok := obj["message"].(string); ok {
			s := strings.TrimSpace(v)
			if s != "" {
				ci.Message = &s
			}
		}
		if v, ok := obj["author"].(string); ok {
			s := strings.TrimSpace(v)
			if s != "" {
				ci.Author = &s
			}
		}
		if v, ok := obj["committedAt"].(string); ok {
			s := strings.TrimSpace(v)
			if s != "" {
				t, err := time.Parse(time.RFC3339, s)
				if err != nil {
					return nil, &Failure{
						Code:    ErrCodeValidationError,
						Message: fmt.Sprintf("%s[%d].committedAt must be RFC3339", fieldPrefix, i),
						Fields:  map[string]any{"field": fmt.Sprintf("%s[%d].committedAt", fieldPrefix, i)},
					}
				}
				ci.CommittedAt = &t
			}
		}

		out = append(out, ci)
	}
	return out, nil
}
