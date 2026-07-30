// Package mime adapts net/http's content sniffer so the domain need not import
// net/http. The domain owns the rule (content must sniff as text, a security
// control); this supplies the mechanism. A pass-through rather than a
// reimplementation: a hand-rolled classifier would change which payloads count
// as text.
package mime

import "net/http"

// Detect satisfies domain.MIMESniffer. Pass at most domain.MIMESniffLen bytes;
// the rest is ignored.
func Detect(b []byte) string { return http.DetectContentType(b) }
