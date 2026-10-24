package projects

// validate_test.go — sk-674.18 T025, matrix row 105 (US-15). Reference —
// US-15 and SAAS-PLAN 4.4: a project prefix never starts with a digit; such
// a prefix is refused at creation with prefix_invalid.

import (
	"errors"
	"testing"
)

func TestValidateIDPrefix_FirstCharacterIsALetter(t *testing.T) {
	for _, bad := range []string{"1abc", "9", "0-app", "42_x"} {
		_, err := ValidateIDPrefix(bad)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != CodePrefixInvalid {
			t.Errorf("%q: %v, want %s", bad, err, CodePrefixInvalid)
		}
	}
	for _, good := range []string{"a", "abc1", "app2", "a-1"} {
		if got, err := ValidateIDPrefix(good); err != nil || got != good {
			t.Errorf("%q: %q %v, want it accepted", good, got, err)
		}
	}
}
