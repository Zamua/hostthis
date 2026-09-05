package domain

// AppendResult reports what appending a version did: the OUTCOME of a domain
// operation, so the port defines it and the adapters conform.
type AppendResult struct {
	NewVer    int
	WasPinned bool // the paste was already pinned to a specific version when the append ran
}
