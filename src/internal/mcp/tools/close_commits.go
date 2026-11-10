package tools

import (
	"skein-api/internal/issueops"
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

// parseCommitInputs — see issueops.ParseCommitInputs; the refusal as a
// batch row error.
func parseCommitInputs(raw any, fieldPrefix string) ([]issues.CloseCommitInput, *batchErrorDTO) {
	c, f := issueops.ParseCommitInputs(raw, fieldPrefix)
	return c, batchErrorFrom(f)
}
