package domain_test

import (
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// The shapes the tools actually export, not an idealised one.
func TestDetectKind_Log(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"go slog", `{"time":"2026-08-02T03:00:00Z","level":"INFO","msg":"started"}` + "\n" +
			`{"time":"2026-08-02T03:00:01Z","level":"ERROR","msg":"boom","err":"x"}` + "\n"},
		{"ecs / @timestamp", `{"@timestamp":"2026-08-02T03:00:00Z","log.level":"info","message":"a"}` + "\n" +
			`{"@timestamp":"2026-08-02T03:00:01Z","log.level":"warn","message":"b"}` + "\n"},
		{"bunyan / pino", `{"ts":1785600000,"level":30,"msg":"listening"}` + "\n" +
			`{"ts":1785600001,"level":50,"msg":"failed"}` + "\n"},
		{"python structlog", `{"timestamp":"2026-08-02 03:00:00","levelname":"INFO","message":"x"}` + "\n" +
			`{"timestamp":"2026-08-02 03:00:01","levelname":"DEBUG","message":"y"}` + "\n"},
		{"loki stream objects", `{"stream":{"app":"api"},"values":[["1785600000000000000","up"]]}` + "\n" +
			`{"stream":{"app":"web"},"values":[["1785600001000000000","hit"]]}` + "\n"},
		{"opensearch bulk, action lines interleaved",
			`{"create":{}}` + "\n" +
				`{"@timestamp":"2026-08-02T03:00:00Z","level":"INFO","message":"a"}` + "\n" +
				`{"create":{}}` + "\n" +
				`{"@timestamp":"2026-08-02T03:00:01Z","level":"INFO","message":"b"}` + "\n"},
		{"blank lines between records",
			`{"ts":"2026-08-02T03:00:00Z","level":"INFO","msg":"a"}` + "\n\n" +
				`{"ts":"2026-08-02T03:00:01Z","level":"INFO","msg":"b"}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.DetectKind([]byte(tc.body), "", sniff)
			if err != nil {
				t.Fatalf("DetectKind: %v", err)
			}
			if got != domain.KindLog {
				t.Fatalf("got %q, want log", got)
			}
		})
	}
}

// Logs are valid NDJSON, so the ordering against the JSON gate is the whole
// correctness argument. Plain JSONL must still be JSON.
func TestDetectKind_PlainJSONLIsNotALog(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"records with no time", `{"a":1,"msg":"x"}` + "\n" + `{"a":2,"msg":"y"}` + "\n"},
		{"records with no level or message", `{"ts":"2026-08-02T03:00:00Z","v":1}` + "\n" +
			`{"ts":"2026-08-02T03:00:01Z","v":2}` + "\n"},
		{"plain data", `{"id":1,"name":"ada"}` + "\n" + `{"id":2,"name":"grace"}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.DetectKind([]byte(tc.body), "", sniff)
			if err != nil {
				t.Fatalf("DetectKind: %v", err)
			}
			if got != domain.KindJSON {
				t.Fatalf("got %q, want json", got)
			}
		})
	}
}

// A log capture is longer than the sniff window, so the last line in it is
// always cut. Same class of bug as the folded-stack detector shipped with.
func TestDetectKind_LogPrefixIsTruncated(t *testing.T) {
	var sb strings.Builder
	for i := 0; sb.Len() < 3*domain.SniffPrefixLen; i++ {
		sb.WriteString(`{"@timestamp":"2026-08-02T03:00:00Z","level":"INFO",` +
			`"message":"a reasonably long log line so records span the window","seq":` +
			strings.Repeat("9", 3) + "}\n")
	}
	full := []byte(sb.String())
	for _, n := range []int{512, 2048, domain.SniffPrefixLen} {
		got, err := domain.DetectKind(full[:n], "", sniff)
		if err != nil || got != domain.KindLog {
			t.Fatalf("prefix of %d bytes: got %q err=%v, want log", n, got, err)
		}
	}
}

// Plain text used to be REJECTED unless it carried a Markdown cue, so a
// config file or a stack trace bounced.
func TestDetectKind_PlainTextFallsBackToText(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"prose with no markdown cue", "the quick brown fox jumped over the lazy dog\nand then it did so again\n"},
		{"a stack trace", "Traceback (most recent call last):\n  File \"a.py\", line 3, in <module>\n    raise ValueError\nValueError\n"},
		{"an ini file", "[server]\nhost = localhost\nport = 8080\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.DetectKind([]byte(tc.body), "", sniff)
			if err != nil {
				t.Fatalf("DetectKind: %v", err)
			}
			if got != domain.KindText {
				t.Fatalf("got %q, want text", got)
			}
		})
	}
}

// Text is the fallback and must never outrank a richer gate.
func TestDetectKind_TextDoesNotStealFromRicherKinds(t *testing.T) {
	for _, tc := range []struct {
		body string
		want domain.ContentKind
	}{
		{"# heading\n\nprose\n", domain.KindMarkdown},
		{"flowchart TD\n  A-->B\n", domain.KindMermaid},
		{"a,b,c\n1,2,3\n4,5,6\n7,8,9\n", domain.KindCSV},
		{`{"a":1}` + "\n" + `{"a":2}` + "\n", domain.KindJSON},
		{"main;a 4\nmain;b 9\n", domain.KindFlamegraph},
	} {
		got, err := domain.DetectKind([]byte(tc.body), "", sniff)
		if err != nil {
			t.Fatalf("DetectKind(%q): %v", tc.body[:12], err)
		}
		if got != tc.want {
			t.Fatalf("got %q, want %q for %q", got, tc.want, tc.body[:12])
		}
	}
}

// Binary is still refused, including under a text hint: the fallback must not
// become a way in for bytes no viewer can render.
func TestDetectKind_TextFallbackStillRefusesBinary(t *testing.T) {
	bin := append([]byte{0x00, 0x01, 0x02, 0xff, 0xfe}, make([]byte, 600)...)
	if _, err := domain.DetectKind(bin, "", sniff); err == nil {
		t.Fatal("binary accepted by the text fallback")
	}
	if _, err := domain.DetectKind(bin, "text", sniff); err == nil {
		t.Fatal("binary accepted under a text hint")
	}
}
