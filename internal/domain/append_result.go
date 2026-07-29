package domain

// AppendResult reports what appending a version did.
//
// Lives in the domain rather than in a storage adapter because it describes an
// OUTCOME of a domain operation, not a storage mechanism: it has no infra
// dependency, and both the service layer and the SSH layer read it to decide
// what to tell the user (WasPinned drives the "your pin held; the new version
// is not being served" warning). A port returning an adapter-owned type points
// the dependency arrow the wrong way - the port defines the vocabulary, and
// the adapter conforms to it.
type AppendResult struct {
	NewVer    int
	WasPinned bool // the paste was pinned to a specific version BEFORE this append
}
