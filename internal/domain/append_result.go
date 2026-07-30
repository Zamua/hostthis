package domain

// AppendResult reports what appending a version did. It lives in the domain
// rather than a storage adapter because it is the OUTCOME of a domain
// operation, not a storage mechanism: the port defines the vocabulary and the
// adapter conforms to it.
type AppendResult struct {
	NewVer    int
	WasPinned bool // the paste was already pinned to a specific version when the append ran
}
