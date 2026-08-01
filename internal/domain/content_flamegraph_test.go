package domain_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// Folded stack profiles are detected from their shape, with no type hint.
func TestDetectKind_Folded(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"plain folded", "main;a;b 12\nmain;a;c 7\nmain;d 3\n"},
		{"frames containing spaces", "main;void foo(int, int);bar 91\nmain;void foo(int, int);baz 4\n"},
		{"mixed depth with flat roots", "runtime.mcall 5\nmain;serve;read 40\nmain;serve;write 22\n"},
		{"comment lines ignored", "# perf 6.1\nmain;a 4\nmain;b 9\n"},
		{"tab before the count", "main;a\t14\nmain;b\t2\n"},
		{"no trailing newline", "main;a 4\nmain;b 9"},
		{"crlf", "main;a 4\r\nmain;b 9\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.DetectKind([]byte(tc.body), "", sniff)
			if err != nil {
				t.Fatalf("DetectKind: %v", err)
			}
			if got != domain.KindFlamegraph {
				t.Fatalf("got %q, want flamegraph", got)
			}
		})
	}
}

// The gate exists to keep ordinary text out. Each of these ends lines in a
// number, contains semicolons, or both, and none is a profile.
func TestDetectKind_NotFolded(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"numbered list", "item 1\nitem 2\nitem 3\n"},
		{"prose ending in a year", "Released in 2019\nRewritten in 2024\n"},
		{"one folded line among prose", "main;a;b 12\nthis is a sentence about it\n"},
		{"semicolons but no counts", "a;b;c\nd;e;f\n"},
		{"single line", "main;a;b 12\n"},
		{"css", "a { color: red; }\nb { margin: 0; }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := domain.DetectKind([]byte(tc.body), "", sniff)
			if got == domain.KindFlamegraph {
				t.Fatalf("detected as flamegraph, want anything else")
			}
		})
	}
}

// A C++ profile carries a comma-consistent argument list on every line, which
// is exactly the shape the CSV gate looks for. Ordering, not the gates
// themselves, is what keeps this right.
func TestDetectKind_CppProfileIsNotCSV(t *testing.T) {
	body := "main;std::vector<int, alloc>::push_back(int, int);grow 41\n" +
		"main;std::vector<int, alloc>::push_back(int, int);copy 18\n" +
		"main;std::map<int, int>::find(int, int);cmp 6\n"
	got, err := domain.DetectKind([]byte(body), "", sniff)
	if err != nil {
		t.Fatalf("DetectKind: %v", err)
	}
	if got != domain.KindFlamegraph {
		t.Fatalf("got %q, want flamegraph", got)
	}
}

// The service sniffs a ~512-byte PREFIX, not the whole upload, so a real
// profile always arrives with its final line cut mid-count. Every fixture
// above is smaller than that window and so could not catch this: detection
// passed on them and rejected every real profile.
func TestDetectKind_FoldedPrefixIsTruncated(t *testing.T) {
	var sb strings.Builder
	for i := 0; sb.Len() < 4096; i++ {
		fmt.Fprintf(&sb, "main;serve;handler%d;read %d\n", i, 10+i)
	}
	full := []byte(sb.String())
	for _, n := range []int{512, 700, 1024} {
		got, err := domain.DetectKind(full[:n], "", sniff)
		if err != nil || got != domain.KindFlamegraph {
			t.Fatalf("prefix of %d bytes: got %q err=%v, want flamegraph", n, got, err)
		}
	}
}

// A real Go profile's frames are fully-qualified, so ONE folded line averages
// around 600 bytes and often exceeds it. The 512-byte window the pipeline used
// to capture could therefore hold no complete line at all, and no line-based
// gate can fire on that. The fixtures above all use short frames and so cannot
// catch it: they passed while every real profile was rejected.
func TestDetectKind_FoldedLinesLongerThanTheMIMEWindow(t *testing.T) {
	frame := "github.com/example/project/internal/transport.(*Server).handleConnection"
	var sb strings.Builder
	for i := 0; sb.Len() < 3*domain.SniffPrefixLen; i++ {
		for f := range 9 {
			fmt.Fprintf(&sb, "%s.step%d.variant%d;", frame, f, i)
		}
		fmt.Fprintf(&sb, "leaf%d %d\n", i, 7+i)
	}
	full := []byte(sb.String())
	if first := strings.Index(sb.String(), "\n"); first < domain.MIMESniffLen {
		t.Fatalf("fixture defeats its purpose: first line is %d bytes, want > %d",
			first, domain.MIMESniffLen)
	}
	got, err := domain.DetectKind(full[:domain.SniffPrefixLen], "", sniff)
	if err != nil || got != domain.KindFlamegraph {
		t.Fatalf("got %q err=%v, want flamegraph", got, err)
	}
}

// Forgiving the window's last line must not forgive one in the middle.
func TestDetectKind_MalformedLineBeforeTheEndStillRejects(t *testing.T) {
	body := "main;a;b 12\nthis line is prose, not a stack\nmain;c;d 9\nmain;e 4\n"
	if got, _ := domain.DetectKind([]byte(body), "", sniff); got == domain.KindFlamegraph {
		t.Fatal("detected as flamegraph despite a malformed interior line")
	}
}

// The hint forces the kind, and a binary payload is still refused under it.
func TestDetectKind_FoldedHint(t *testing.T) {
	for _, h := range []string{"flamegraph", "flame", "folded"} {
		got, err := domain.DetectKind([]byte("anything at all\n"), h, sniff)
		if err != nil {
			t.Fatalf("hint %q: %v", h, err)
		}
		if got != domain.KindFlamegraph {
			t.Fatalf("hint %q: got %q, want flamegraph", h, got)
		}
	}
	if _, err := domain.DetectKind([]byte("\x00\x01\x02\xff\xfe"+strings.Repeat("\x00", 60)), "flamegraph", sniff); err == nil {
		t.Fatal("binary accepted under a flamegraph hint")
	}
}
