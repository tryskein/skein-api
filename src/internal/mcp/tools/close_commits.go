package tools

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/issues"
)

// maxCommitsPerCloseCall caps the number of commit refs a single
// close call (skein_close, or each item in skein_close_many) can
// attach. Pinned to issues.MaxCommitsPerClose so the MCP boundary
// and the store-level guard can never silently disagree — bumping
// the cap requires touching one constant.
const maxCommitsPerCloseCall = issues.MaxCommitsPerClose

// commitInputItemSchema returns the JSON Schema fragment for one
// element of the `commits[]` array on skein_close / skein_close_many /
// skein_commits_attach. Hash is required; the rest are optional. Built
// as a map[string]any so every call site can drop it straight into
// mcpgo.Items(...) without schema drift.
//
// Field descriptions are deliberately one-liners (sk-255.2): this
// fragment is inlined into four places in the tools/list payload, so
// the repo-resolution rule ("repoId or repoUrl or omit both when the
// project has exactly one repo") is stated once in the guide under
// "Commit refs" and once in commitsArrayDescription — not seven times
// per tool.
func commitInputItemSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             []string{"hash"},
		"additionalProperties": false,
		"properties": map[string]any{
			"hash": map[string]any{
				"type":        "string",
				"description": "Lowercase hex commit hash (abbreviated SHA-1 .. full SHA-256).",
				"minLength":   7,
				"maxLength":   64,
				"pattern":     "^[0-9a-f]{7,64}$",
			},
			"repoUrl": map[string]any{
				"type":        "string",
				"description": "Exact match against project.gitRepos[].url.",
			},
			"repoId": map[string]any{
				"type":        "string",
				"description": "Repo UUID from project.gitRepos[].id. Wins over repoUrl.",
				"format":      "uuid",
			},
			"url": map[string]any{
				"type":        "string",
				"description": "Canonical commit URL.",
			},
			"message": map[string]any{
				"type":        "string",
				"description": "Commit subject line.",
			},
			"author": map[string]any{
				"type":        "string",
				"description": "Commit author (name or email).",
			},
			"committedAt": map[string]any{
				"type":        "string",
				"description": "Commit timestamp (RFC3339).",
				"format":      "date-time",
			},
		},
	}
}

// parseCommitInputs is the shared parser for `commits[]` on
// skein_close and skein_close_many. fieldPrefix is prepended to error
// messages so callers can surface tool-specific paths (e.g.
// "commits[2].hash" for skein_close, "items[5].commits[0].hash" for
// skein_close_many).
//
// Returns ([]issues.CloseCommitInput, *batchErrorDTO). On success the
// error is nil. On failure the slice is nil and the dto carries the
// classified validation_error envelope.
func parseCommitInputs(raw any, fieldPrefix string) ([]issues.CloseCommitInput, *batchErrorDTO) {
	if raw == nil {
		return nil, nil
	}
	rawSlice, ok := raw.([]any)
	if !ok {
		return nil, &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: fieldPrefix + " must be an array",
		}
	}
	if len(rawSlice) == 0 {
		return nil, nil
	}
	if len(rawSlice) > maxCommitsPerCloseCall {
		return nil, &batchErrorDTO{
			Code:    ErrCodeValidationError,
			Message: fmt.Sprintf("%s may carry at most %d items (got %d)", fieldPrefix, maxCommitsPerCloseCall, len(rawSlice)),
			Details: map[string]any{"max": maxCommitsPerCloseCall, "got": len(rawSlice)},
		}
	}

	out := make([]issues.CloseCommitInput, 0, len(rawSlice))
	for i, entry := range rawSlice {
		obj, ok := entry.(map[string]any)
		if !ok {
			return nil, &batchErrorDTO{
				Code:    ErrCodeValidationError,
				Message: fmt.Sprintf("%s[%d] must be an object", fieldPrefix, i),
				Details: map[string]any{"field": fmt.Sprintf("%s[%d]", fieldPrefix, i)},
			}
		}

		hash, _ := obj["hash"].(string)
		hash = strings.TrimSpace(hash)
		if hash == "" {
			return nil, &batchErrorDTO{
				Code:    ErrCodeValidationError,
				Message: fmt.Sprintf("%s[%d].hash is required", fieldPrefix, i),
				Details: map[string]any{"field": fmt.Sprintf("%s[%d].hash", fieldPrefix, i)},
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
					return nil, &batchErrorDTO{
						Code:    ErrCodeValidationError,
						Message: fmt.Sprintf("%s[%d].repoId is not a valid UUID", fieldPrefix, i),
						Details: map[string]any{"field": fmt.Sprintf("%s[%d].repoId", fieldPrefix, i)},
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
					return nil, &batchErrorDTO{
						Code:    ErrCodeValidationError,
						Message: fmt.Sprintf("%s[%d].committedAt must be RFC3339", fieldPrefix, i),
						Details: map[string]any{"field": fmt.Sprintf("%s[%d].committedAt", fieldPrefix, i)},
					}
				}
				ci.CommittedAt = &t
			}
		}

		out = append(out, ci)
	}
	return out, nil
}
