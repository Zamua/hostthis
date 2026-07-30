// Package mime is the content-sniffing ADAPTER.
//
// It exists so the domain does not import net/http. The domain owns the RULE
// ("content must sniff as some flavour of text, so a binary payload is rejected
// even when the user labels it html" - a security control), and declares
// domain.MIMESniffer as the port. This supplies the mechanism.
//
// Deliberately a thin pass-through rather than a reimplementation. The sniffing
// here is load-bearing for a security guard, and hand-rolling a classifier
// would risk changing which payloads are considered text - the exact behaviour
// that guard was added to pin.
package mime

import "net/http"

// Detect reports a media type for a byte prefix, e.g. "text/plain;
// charset=utf-8" or "application/octet-stream". Satisfies domain.MIMESniffer.
//
// Callers should pass at most domain.MIMESniffLen bytes; the underlying
// algorithm is defined over a bounded prefix and ignores the rest.
func Detect(b []byte) string { return http.DetectContentType(b) }
