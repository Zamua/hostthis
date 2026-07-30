package domain

// keys returns a manifest's paths, unordered. Lives here rather than in the
// archive package's tests because both sides need it: the domain tests the
// manifest rules, the archive tests the codec that feeds them.
func keys(m map[string]ManifestEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// stubSniffer returns a fixed media type, so a test can drive DetectKind's
// text-only rule directly instead of hunting for bytes that happen to sniff a
// particular way. That decoupling is the point of the MIMESniffer port.
func stubSniffer(mime string) MIMESniffer {
	return func([]byte) string { return mime }
}
