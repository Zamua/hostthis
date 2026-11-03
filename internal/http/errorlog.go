package http

import (
	"log"
	"net/netip"
	"regexp"
	"strings"
)

// ServerErrorLog is the http.Server.ErrorLog that cannot print a client
// address: net/http names the peer in its panic and TLS-handshake lines, and
// any address-shaped token is replaced before the line reaches printf.
func ServerErrorLog(printf func(format string, args ...any)) *log.Logger {
	return log.New(scrubWriter{printf}, "", 0)
}

type scrubWriter struct {
	printf func(format string, args ...any)
}

func (w scrubWriter) Write(p []byte) (int, error) {
	w.printf("%s", scrubPeers(strings.TrimRight(string(p), "\n")))
	return len(p), nil
}

var token = regexp.MustCompile(`\S+`)

// scrubPeers replaces every token that is an IP address or address:port.
func scrubPeers(s string) string {
	return token.ReplaceAllStringFunc(s, func(tok string) string {
		core := strings.Trim(tok, ":,;()")
		if _, err := netip.ParseAddrPort(core); err == nil {
			return strings.Replace(tok, core, "<peer>", 1)
		}
		if _, err := netip.ParseAddr(core); err == nil {
			return strings.Replace(tok, core, "<peer>", 1)
		}
		return tok
	})
}
