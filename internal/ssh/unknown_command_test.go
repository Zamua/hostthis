package ssh

import "testing"

func TestNearestVerb(t *testing.T) {
	cases := []struct {
		word string
		want string // "" means no suggestion
	}{
		{"versons", "versions"},
		{"verison", "versions"},
		{"lsit", "list"},
		{"LIST", "list"},
		{"delte", "delete"},
		{"whomai", "whoami"},
		{"pins", "pin"},
		{"gte", "get"},
		{"bogusverb", ""},
		{"totallybogus", ""},
		{"wibble", ""},
		{"ls", ""},
		{"x", ""},
	}
	for _, c := range cases {
		got, ok := nearestVerb(c.word)
		if c.want == "" {
			if ok {
				t.Errorf("nearestVerb(%q) = %q, want no suggestion", c.word, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("nearestVerb(%q) = (%q, %v), want %q", c.word, got, ok, c.want)
		}
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "get", 3},
		{"get", "get", 0},
		{"gte", "get", 1},
		{"versons", "versions", 1},
		{"kitten", "sitting", 3},
		{"café", "cafe", 1},
	}
	for _, c := range cases {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
