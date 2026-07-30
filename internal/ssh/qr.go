package ssh

import (
	"io"

	"github.com/mdp/qrterminal/v3"
)

// writeQR renders url as a compact half-block terminal QR code to w. The
// upload paths and the `qr` verb share it so the visual is identical either
// way.
//
// Half-block mode packs two QR rows per text line. The quiet zone is widened
// to 2 modules, above the library's floor of 1, so the finder patterns are not
// flush against surrounding terminal text, which improves scan reliability.
// Encoder errors are swallowed: the QR is supplementary narration on stderr
// and must never derail the upload or the URL on stdout.
func writeQR(w io.Writer, url string) {
	qrterminal.GenerateWithConfig(url, qrterminal.Config{
		Level:      qrterminal.L,
		Writer:     w,
		HalfBlocks: true,
		QuietZone:  2,
	})
}
