package spaces

// shortid_test.go — sk-674.18 T023, matrix row 91 (K-20). Reference — plan
// K-20, written before the code: the space short id is 8 characters of
// lowercase base32 (a–z, 2–7), unique; a collision is drawn again — never an
// error and never a duplicate. The random source and the insert are the
// generator's two boundaries; both are substituted here (insertRegularSpace
// passes crypto/rand and the database's ON CONFLICT answer).

import (
	"bytes"
	"errors"
	"regexp"
	"testing"
)

var shortIDForm = regexp.MustCompile(`^[a-z2-7]{8}$`)

func TestShortID_CollisionIsDrawnAgain(t *testing.T) {
	// Two draws' worth of bytes: the first id is taken, the second is free.
	src := bytes.NewReader(append(bytes.Repeat([]byte{0}, 8), bytes.Repeat([]byte{1}, 8)...))
	taken := map[string]bool{"aaaaaaaa": true}
	var offered []string
	got, err := withFreshShortID(src, func(short string) (bool, error) {
		offered = append(offered, short)
		if taken[short] {
			return false, nil
		}
		taken[short] = true
		return true, nil
	})
	if err != nil {
		t.Fatalf("a collision ended in an error: %v", err)
	}
	if len(offered) != 2 {
		t.Fatalf("offered %v, want the taken id and one more draw", offered)
	}
	if got == "aaaaaaaa" || got != offered[1] {
		t.Errorf("got %q after offering %v, want the second, free draw", got, offered)
	}
	for _, s := range offered {
		if !shortIDForm.MatchString(s) {
			t.Errorf("short id %q is not 8 lowercase base32 characters", s)
		}
	}
}

func TestShortID_FormOverTheWholeAlphabet(t *testing.T) {
	// Every byte value: the id stays inside the alphabet.
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	src := bytes.NewReader(all)
	seen := map[byte]bool{}
	for range 32 {
		s, err := drawShortID(src)
		if err != nil {
			t.Fatalf("draw: %v", err)
		}
		if !shortIDForm.MatchString(s) {
			t.Fatalf("short id %q is not 8 lowercase base32 characters", s)
		}
		for i := 0; i < len(s); i++ {
			seen[s[i]] = true
		}
	}
	if len(seen) != 32 {
		t.Errorf("the draws used %d symbols, want all 32 of the alphabet", len(seen))
	}
}

func TestShortID_InsertErrorIsNotACollision(t *testing.T) {
	boom := errors.New("database down")
	src := bytes.NewReader(bytes.Repeat([]byte{7}, 64))
	calls := 0
	_, err := withFreshShortID(src, func(string) (bool, error) {
		calls++
		return false, boom
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Errorf("err %v after %d inserts, want the insert's error at once", err, calls)
	}
}
