package domain_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

func sniff(b []byte) string { return http.DetectContentType(b) }

// Each new kind is detected from its own content, with no type hint.
func TestDetectKind_NewKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want domain.ContentKind
	}{
		{"mermaid flowchart", "flowchart TD\n  A[Start] --> B[End]\n", domain.KindMermaid},
		{"mermaid sequence", "sequenceDiagram\n  Alice->>Bob: hi\n", domain.KindMermaid},
		{"mermaid graph", "graph LR\n  a-->b\n", domain.KindMermaid},
		{"mermaid after a directive", "%%{init: {'theme':'dark'}}%%\nflowchart TD\n  A-->B\n", domain.KindMermaid},
		{"json object", `{"a":1,"b":[2,3]}`, domain.KindJSON},
		{"json array", `[{"a":1},{"a":2}]`, domain.KindJSON},
		{"jsonl", "{\"a\":1}\n{\"a\":2}\n{\"a\":3}\n", domain.KindJSON},
		{"pretty json truncated by the sniff window", "{\n  \"a\": 1,\n  \"b\": \"" + strings.Repeat("x", 900), domain.KindJSON},
		{"csv", "name,role,team\nada,eng,core\ngrace,eng,core\n", domain.KindCSV},
		{"tsv", "name\trole\tteam\nada\teng\tcore\ngrace\teng\tcore\n", domain.KindCSV},
		{"csv with quoted commas", "name,addr,zip\n\"a\",\"Springfield, IL\",1\n\"b\",\"Dayton, OH\",2\n", domain.KindCSV},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.DetectKind([]byte(tc.body), "", sniff)
			if err != nil {
				t.Fatalf("DetectKind: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// PDF is accepted on its magic and ONLY on its magic: the signature is the
// gate, exactly as gzip is for a site archive.
func TestDetectKind_PDF(t *testing.T) {
	pdf := []byte("%PDF-1.7\n1 0 obj\n<</Type/Catalog>>\nendobj\n")

	got, err := domain.DetectKind(pdf, "", sniff)
	if err != nil || got != domain.KindPDF {
		t.Fatalf("unhinted PDF: got %q err %v, want pdf", got, err)
	}
	if got, err = domain.DetectKind(pdf, "pdf", sniff); err != nil || got != domain.KindPDF {
		t.Fatalf("hinted PDF: got %q err %v, want pdf", got, err)
	}

	// A pdf hint over non-PDF bytes must NOT relabel them: the viewer would
	// fail and the Content-Type would be a lie.
	if _, err := domain.DetectKind([]byte("# not a pdf\n"), "pdf", sniff); err == nil {
		t.Fatal("a pdf hint over markdown bytes must be rejected, not relabelled")
	}
	// And the reverse: PDF bytes under a text hint stay rejected, so a binary
	// cannot be smuggled through and served as text/html.
	if _, err := domain.DetectKind(pdf, "html", sniff); err == nil {
		t.Fatal("PDF bytes under an html hint must be rejected")
	}
}

// The CSV gate must not fire on prose. Two-column consistency is exactly what
// wrapped prose produces, which is why the threshold is three fields.
func TestDetectKind_ProseIsNotCSV(t *testing.T) {
	for _, body := range []string{
		"Hello, world\nGoodbye, world\nFarewell, world\n",
		"- one, two\n- three, four\n- five, six\n",
		"# Title\n\nA sentence, with a clause.\nAnother sentence, also clausal.\nA third, likewise.\n",
	} {
		got, err := domain.DetectKind([]byte(body), "", sniff)
		if err == nil && got == domain.KindCSV {
			t.Fatalf("prose detected as CSV, which would render a paragraph as a spreadsheet:\n%s", body)
		}
	}
}

// Every kind is forceable by hint, so a document the gates classify differently
// can still be rendered the way its author intended.
func TestDetectKind_HintsForceEveryKind(t *testing.T) {
	// One body that satisfies several gates at once: without a hint it is a
	// diff, so each hint below has to actually override the detection.
	body := []byte("--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-a,b,c\n+a,b,d\n")
	for hint, want := range map[string]domain.ContentKind{
		"markdown": domain.KindMarkdown,
		"md":       domain.KindMarkdown,
		"html":     domain.KindHTML,
		"diff":     domain.KindDiff,
		"mermaid":  domain.KindMermaid,
		"mmd":      domain.KindMermaid,
		"csv":      domain.KindCSV,
		"tsv":      domain.KindCSV,
		"json":     domain.KindJSON,
		"jsonl":    domain.KindJSON,
	} {
		got, err := domain.DetectKind(body, hint, sniff)
		if err != nil {
			t.Fatalf("hint %q: %v", hint, err)
		}
		if got != want {
			t.Fatalf("hint %q gave %q, want %q", hint, got, want)
		}
	}
}

// An unrecognised hint still rejects rather than falling back to sniffing.
func TestDetectKind_UnknownHintStillRejects(t *testing.T) {
	if _, err := domain.DetectKind([]byte("# hi\n"), "wat", sniff); err == nil {
		t.Fatal("an unrecognised hint must reject")
	}
}
