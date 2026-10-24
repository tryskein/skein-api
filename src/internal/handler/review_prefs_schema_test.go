package handler

// review_prefs_schema_test.go — sk-303.14, review A/9 repair (finding H):
// a gate on the "empty value is allowed" flag in the spec.
//
// The three schedule parameters of the review workflow MAY be empty — this
// is not an edge case, it is a mandatory part of the contract: empty is
// exactly "reset the parameter to factory", and an untouched profile
// returns all three empty on the most ordinary path.
//
// The previous rationale said this was inexpressible by an OpenAPI 2.0
// schema, and it was wrong: what is inexpressible is only the distinction
// "key omitted" versus "null arrived", while the emptiness itself is
// expressed by the vendor extension `x-nullable`, which the generator
// supports. While the flag was missing, the spec declared the three
// fields as not allowing emptiness, and a client generated from it would
// break on a legitimate value.
//
// What is checked here is the TAG, not the generated spec, for the same
// reason the neighbouring gates are built that way: the requirement must
// survive the next edit. The tag is what the generator builds the schema
// from.
//
// ⚠ CORRECTED RATIONALE (review A/9, finding K-1; not erased). The
// previous edition of this paragraph explained the choice by saying the
// generator "is installed neither in the integration harness nor in CI
// jobs" — and stopped there, repeating exactly the error the same round
// was repairing on the metrics validator: "it is not here" was taken for
// "it cannot be run". It can be run: `swag` v1.16.4 is installed in the
// development environment, and ONE run was enough to see a defect no
// tree guard saw — a fragment of Russian prose that rode into the
// request-body schema description (finding K-1, fixed in review_prefs.go
// and guarded by openapi_schema_descriptions_test.go).
//
// The generator's absence from the harness and from CI is true, and it
// remains the reason to keep the tag gate: a one-shot run of an external
// tool does not survive the next edit. But it is NOT a reason not to run
// the generator: the gate is "on top of", not "instead of", and a run
// before handover is mandatory.

import (
	"reflect"
	"strings"
	"testing"

	"skein-api/internal/notifications"
)

// nullableSchemaFields is the EXHAUSTIVE markup of empty-value
// permissibility by C4-contract types: key is the type, value is the
// fields whose schema MUST declare empty. Every OTHER field of a listed
// type MUST NOT declare empty.
//
// ⚠ THE LIST IS READ IN BOTH DIRECTIONS (review A/9, finding P-1). The
// previous edition checked only the presence of the tag on the named
// fields, and an extra tag on a neighbouring field was invisible to it —
// which the wave then used: `x-nullable` was hung on ReviewScheduleDTO
// and ReviewBoundsDTO, where empty never happens. The error is the
// INVERSE of finding H: there the spec denied empty that happens; here
// it promised empty that does not. On ReviewBoundsDTO it additionally
// changed the SHAPE of the schema — three `$ref`s rode into
// `allOf: [$ref]`.
//
// An empty field list is not "the type is not checked", it is the claim
// "this type has empty on none of its fields", and it is checked on a
// par with the others.
var nullableSchemaFields = map[reflect.Type][]string{
	reflect.TypeOf(putReviewPrefsRequest{}): {
		"BurstCount", "UnacceptedIntervalMinutes", "UnreviewedIntervalMinutes",
	},
	reflect.TypeOf(notifications.ReviewPrefsDTO{}): {
		"BurstCount", "UnacceptedIntervalMinutes", "UnreviewedIntervalMinutes",
	},
	// Resolved schedule: factory is already substituted, fields are not pointers.
	reflect.TypeOf(notifications.ReviewScheduleDTO{}): {},
	// Bounds: every field is a struct, always filled.
	reflect.TypeOf(notifications.ReviewBoundsDTO{}): {},
}

func TestReviewPrefsSchema_NullableFieldsSaySoInTheSpec(t *testing.T) {
	t.Parallel()
	checked := 0
	for typ, fields := range nullableSchemaFields {
		want := map[string]bool{}
		for _, name := range fields {
			want[name] = true
		}
		// The other side: no UNNAMED field of the type has the right to
		// declare empty. The promise "the spec tells the truth about empty"
		// breaks equally painfully in both directions.
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if want[f.Name] {
				continue
			}
			checked++
			if strings.Contains(f.Tag.Get("extensions"), "x-nullable") {
				t.Errorf("%s.%s carries `extensions:\"x-nullable\"` even though it never "+
					"serialises as empty: the spec promises the client empty where it "+
					"does not happen. For a struct field this additionally changes the "+
					"schema shape — `$ref` rides into `allOf: [$ref]`, and the SDK "+
					"generator builds different code from it. Either the tag is extra, "+
					"or the field has started allowing empty and belongs in "+
					"nullableSchemaFields",
					typ.Name(), f.Name)
			}
		}
		for _, name := range fields {
			f, ok := typ.FieldByName(name)
			if !ok {
				t.Errorf("%s: field %s is gone — the subject of the claim disappeared, "+
					"and the gate must be rewritten, not left standing", typ.Name(), name)
				continue
			}
			checked++
			if !strings.Contains(f.Tag.Get("extensions"), "x-nullable") {
				t.Errorf("%s.%s does not carry `extensions:\"x-nullable\"` — the spec "+
					"will declare the field as not allowing emptiness, while an empty "+
					"value is a mandatory part of the contract (\"reset to factory\") and "+
					"arrives on the most ordinary path, on an untouched profile. A client "+
					"generated from such a spec will break on a legitimate value",
					typ.Name(), name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("not a single field was checked — the list is broken, and the gate " +
			"is tautologically green")
	}
}
