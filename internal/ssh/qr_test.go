package ssh

import (
	"bytes"
	"strings"
	"testing"
)

// TestWriteQR_RendersBlock is the unit test for the shared QR helper:
// given a URL it writes a non-empty, multi-line block of half-block
// glyphs. Decoding a terminal QR back to text is not worth a dependency
// here; the structural assertions pin that something scannable was
// emitted (and that the helper never panics on a typical URL).
func TestWriteQR_RendersBlock(t *testing.T) {
	const qrGlyphs = "█▀▄"
	var buf bytes.Buffer
	writeQR(&buf, "https://abc12345.paste.test")
	out := buf.String()
	if out == "" {
		t.Fatal("writeQR produced no output")
	}
	if !strings.ContainsAny(out, qrGlyphs) {
		t.Fatalf("writeQR output has no half-block glyphs: %q", out)
	}
	if lines := strings.Count(strings.TrimRight(out, "\n"), "\n"); lines < 5 {
		t.Fatalf("writeQR output too small to be a QR (%d newlines):\n%s", lines, out)
	}
}
