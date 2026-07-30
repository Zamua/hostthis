// Package mime is the content-sniffing adapter, so the domain need not import
// net/http. The domain owns the rule (content must sniff as text, a security
// control); this supplies the mechanism.
//
// A thin pass-through rather than a reimplementation: hand-rolling a classifier
// would risk changing which payloads count as text.
package mime

import "net/http"

// Detect reports a media type for a byte prefix. Satisfies domain.MIMESniffer.
// Pass at most domain.MIMESniffLen bytes; the rest is ignored.
func Detect(b []byte) string { return http.DetectContentType(b) }
