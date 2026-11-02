package tools

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issues"
)

var patchTextFields = []string{"description", "design", "acceptanceCriteria", "notes", "externalRef", "sourceContext"}

type patchTextState struct {
	text      [6]string
	title     string
	priority  int
	version   int
	updatedAt time.Time
	events    int
}

func readPatchTextState(t *testing.T, m *mcpDA, id string) patchTextState {
	t.Helper()
	var st patchTextState
	if err := m.pool.QueryRow(context.Background(), `
SELECT description, design, acceptance_criteria, notes, external_ref, source_context,
       title, priority, version, updated_at,
       (SELECT count(*) FROM events WHERE entity_type = 'issue' AND entity_id = issues.id)
  FROM issues WHERE id = $1`, id).Scan(
		&st.text[0], &st.text[1], &st.text[2], &st.text[3], &st.text[4], &st.text[5],
		&st.title, &st.priority, &st.version, &st.updatedAt, &st.events); err != nil {
		t.Fatalf("read issue text: %v", err)
	}
	return st
}

func seedPatchText(t *testing.T, m *mcpDA) string {
	t.Helper()
	id := m.bobTask()
	if _, err := m.pool.Exec(context.Background(), `
UPDATE issues SET description = 'original description', design = 'original design',
       acceptance_criteria = 'original criteria', notes = 'original notes',
       external_ref = 'original reference', source_context = 'original source'
 WHERE id = $1`, id); err != nil {
		t.Fatalf("seed issue text: %v", err)
	}
	return id
}

func TestUpdate_NonStringTextFieldsRefused(t *testing.T) {
	for _, field := range patchTextFields {
		t.Run(field, func(t *testing.T) {
			m := newMCPDA(t)
			handler := handleUpdate(m.pool, issues.NewStore(m.pool))
			for _, tc := range nonStringAssignees {
				t.Run(tc.name, func(t *testing.T) {
					id := seedPatchText(t, m)
					before := readPatchTextState(t, m, id)
					body, isErr := m.call(m.ctx, handler, map[string]any{
						"id": id, "expectedVersion": before.version, field: tc.value,
						"title": "must not be written", "priority": float64(0),
					})
					if !isErr || body["error"] != ErrCodeValidationError ||
						body["code"] != "text_field_not_string" || body["field"] != field {
						t.Errorf("response = %v, want validation_error/text_field_not_string for %s", body, field)
					}
					if after := readPatchTextState(t, m, id); after != before {
						t.Errorf("invalid patch changed the issue: before %+v, after %+v", before, after)
					}
				})
			}
		})
	}
}

func TestUpdateMany_NonStringTextFieldsRefusedPerRow(t *testing.T) {
	for _, field := range patchTextFields {
		t.Run(field, func(t *testing.T) {
			m := newMCPDA(t)
			handler := handleUpdateMany(m.pool, issues.NewStore(m.pool))
			first, last := seedPatchText(t, m), seedPatchText(t, m)
			items := []any{map[string]any{"id": first, "expectedVersion": 1, "title": "accepted before"}}
			var ids []string
			var states []patchTextState
			for _, tc := range nonStringAssignees {
				id := seedPatchText(t, m)
				before := readPatchTextState(t, m, id)
				ids, states = append(ids, id), append(states, before)
				items = append(items, map[string]any{
					"id": id, "expectedVersion": before.version, field: tc.value,
					"title": "must not be written", "priority": float64(0),
				})
			}
			items = append(items, map[string]any{"id": last, "expectedVersion": 1, "title": "accepted after"})
			body, isErr := m.call(m.ctx, handler, map[string]any{"items": items})
			if isErr {
				t.Fatalf("batch failed: %v", body)
			}
			rows, _ := body["results"].([]any)
			if len(rows) != len(items) {
				t.Fatalf("batch returned %d rows, want %d", len(rows), len(items))
			}
			for i, id := range ids {
				row := rows[i+1].(map[string]any)
				errObj, _ := row["error"].(map[string]any)
				details, _ := errObj["details"].(map[string]any)
				if row["ok"] != false || errObj["code"] != ErrCodeValidationError ||
					details["code"] != "text_field_not_string" || details["field"] != field {
					t.Errorf("row %d = %v, want validation_error/text_field_not_string for %s", i+1, row, field)
				}
				if after := readPatchTextState(t, m, id); after != states[i] {
					t.Errorf("rejected row changed %s: before %+v, after %+v", id, states[i], after)
				}
			}
			for _, tc := range []struct {
				index int
				id    string
				title string
			}{{0, first, "accepted before"}, {len(rows) - 1, last, "accepted after"}} {
				if row := rows[tc.index].(map[string]any); row["ok"] != true {
					t.Errorf("valid row %d failed: %v", tc.index, row)
				}
				if after := readPatchTextState(t, m, tc.id); after.title != tc.title || after.version != 2 {
					t.Errorf("valid row did not persist: %+v", after)
				}
			}
		})
	}
}

func TestUpdate_TextFieldValueSemantics(t *testing.T) {
	for _, surface := range []string{"single", "batch"} {
		for fieldIndex, field := range patchTextFields {
			for _, tc := range []struct {
				name    string
				present bool
				value   any
				want    string
			}{
				{"null", true, nil, ""},
				{"empty", true, "", ""},
				{"string", true, "replacement text", "replacement text"},
				{"omitted", false, nil, ""},
			} {
				t.Run(surface+"/"+field+"/"+tc.name, func(t *testing.T) {
					m := newMCPDA(t)
					id := seedPatchText(t, m)
					before := readPatchTextState(t, m, id)
					args := map[string]any{"id": id, "expectedVersion": before.version, "title": "accepted patch"}
					if tc.present {
						args[field] = tc.value
					}
					var handler server.ToolHandlerFunc
					if surface == "single" {
						handler = handleUpdate(m.pool, issues.NewStore(m.pool))
					} else {
						handler = handleUpdateMany(m.pool, issues.NewStore(m.pool))
						args = map[string]any{"items": []any{args}}
					}
					body, isErr := m.call(m.ctx, handler, args)
					if isErr {
						t.Fatalf("patch failed: %v", body)
					}
					if surface == "batch" {
						rows, _ := body["results"].([]any)
						if len(rows) != 1 || rows[0].(map[string]any)["ok"] != true {
							t.Fatalf("batch patch failed: %v", body)
						}
					}
					wantText := before.text
					if tc.present {
						wantText[fieldIndex] = tc.want
					}
					if after := readPatchTextState(t, m, id); after.text != wantText || after.title != "accepted patch" || after.version != before.version+1 {
						t.Errorf("persisted state = %+v, want text %q and accepted title/version", after, wantText)
					}
				})
			}
		}
	}
}
