package domain

import (
	"encoding/json"
	"os"
	"testing"
)

// The shared vectors the Worker's parseRange also runs.
func TestParseRange(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/byte-ranges.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Size  int64
		Cases []struct {
			Header, Verdict string
			Start, Length   int64
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]RangeVerdict{"ignored": RangeIgnored, "satisfiable": RangeSatisfiable, "unsatisfiable": RangeUnsatisfiable}
	for _, c := range doc.Cases {
		got, verdict := ParseRange(c.Header, doc.Size)
		want := ByteRange{c.Start, c.Length}
		if verdict != verdicts[c.Verdict] || got != want {
			t.Errorf("ParseRange(%q) = %+v, %d; want %+v, %s", c.Header, got, verdict, want, c.Verdict)
		}
	}
}
