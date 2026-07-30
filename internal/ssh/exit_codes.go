package ssh

// Structured exit codes for SSH sessions, carried back to the local ssh client
// as the process exit code. Scripts branch on them, so the mapping is frozen:
// a value's meaning never changes, and a new meaning takes the next free slot.
// The source of truth; docs/SPEC.md > "Exit codes" mirrors it.
//
// 5 is documented-but-dead and stays that way. The SSH surface never observes
// ErrNotOwner: service.requireOwner collapses non-owner reads to ErrNotFound so
// existence cannot leak across identities.
const (
	ExitOK          = 0 // success
	ExitErr         = 1 // generic / unclassified failure
	ExitUsage       = 2 // malformed args, bad verb, parser failure
	ExitAuth        = 3 // identity required (no key) or missing-owner service error
	ExitNotFound    = 4 // not found, including owner-collapsed permission failures
	_               = 5 // reserved, do not reuse
	ExitSybilRefuse = 6 // Sybil per-subnet rate-limit refusal
)
