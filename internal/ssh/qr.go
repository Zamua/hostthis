package ssh

import (
	"io"

	"github.com/mdp/qrterminal/v3"
)

// writeQR renders url as a compact half-block terminal QR code to w.
//
// This is the single rendering helper shared by the upload paths (which
// print the QR on create/update) and the `qr` verb (which re-shows it
// for an existing paste). Keeping it in one place means the visual is
// identical no matter which path produced it.
//
// Half-block mode packs two QR rows into one text line, so a short URL
// renders as a small, scannable block. The quiet zone is widened to 2
// modules (the library's floor is 1) so the finder patterns aren't
// flush against surrounding terminal text, which improves scan
// reliability. Errors from the encoder are intentionally swallowed: the
// QR is supplementary narration on stderr, and a failure to render it
// must never derail the upload or the URL on stdout.
func writeQR(w io.Writer, url string) {
	qrterminal.GenerateWithConfig(url, qrterminal.Config{
		Level:      qrterminal.L,
		Writer:     w,
		HalfBlocks: true,
		QuietZone:  2,
	})
}
