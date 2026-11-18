package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func rawEntries(sizes ...int) []json.RawMessage {
	out := make([]json.RawMessage, len(sizes))
	for i, n := range sizes {
		// {"s":"<pad>"} of exactly n bytes
		out[i] = json.RawMessage(fmt.Sprintf(`{"s":"%s"}`, strings.Repeat("x", n-8)))
	}
	return out
}

func arrayBytes(entries []json.RawMessage) int {
	b, _ := json.Marshal(entries)
	return len(b)
}

// sk-977.7.10: describe hands out the freshest contiguous tail of the live
// selections, oldest first inside it, within the entry and byte limits of
// the whole JSON array (brackets and commas counted), and says how many
// older ones it left out.
func TestDescribeTargets(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, omitted := describeTargets([]json.RawMessage{}, 20, 32<<10)
		if len(got) != 0 || omitted != 0 {
			t.Fatalf("got %d entries, %d omitted; want 0, 0", len(got), omitted)
		}
	})
	t.Run("all twenty usual clicks fit", func(t *testing.T) {
		in := rawEntries(repeat(872, 20)...)
		got, omitted := describeTargets(in, 20, 32<<10)
		if len(got) != 20 || omitted != 0 {
			t.Fatalf("got %d, omitted %d; want 20, 0", len(got), omitted)
		}
	})
	t.Run("entry count", func(t *testing.T) {
		in := rawEntries(repeat(100, 25)...)
		got, omitted := describeTargets(in, 20, 32<<10)
		if len(got) != 20 || omitted != 5 || &got[0][0] != &in[5][0] {
			t.Fatalf("got %d, omitted %d; want the last 20 and 5 omitted", len(got), omitted)
		}
	})
	t.Run("exact byte boundary", func(t *testing.T) {
		in := rawEntries(100, 100, 100)
		exact := arrayBytes(in[1:]) // two entries, brackets, one comma
		got, omitted := describeTargets(in, 20, exact)
		if len(got) != 2 || omitted != 1 || arrayBytes(got) != exact {
			t.Fatalf("budget %d: got %d (%d bytes), omitted %d; want 2 and 1", exact, len(got), arrayBytes(got), omitted)
		}
		got, omitted = describeTargets(in, 20, exact-1)
		if len(got) != 1 || omitted != 2 {
			t.Fatalf("budget %d: got %d, omitted %d; want 1 and 2", exact-1, len(got), omitted)
		}
	})
	t.Run("ten large clicks", func(t *testing.T) {
		in := rawEntries(repeat(3016, 12)...)
		got, omitted := describeTargets(in, 20, 32<<10)
		if len(got) != 10 || omitted != 2 || arrayBytes(got) > 32<<10 {
			t.Fatalf("got %d (%d bytes), omitted %d; want 10 within 32 KiB and 2 omitted", len(got), arrayBytes(got), omitted)
		}
	})
}

func repeat(n, times int) []int {
	out := make([]int, times)
	for i := range out {
		out[i] = n
	}
	return out
}
