package ssh

// Structured exit codes for SSH sessions. The SSH protocol carries the
// exit status back to the local ssh client, which surfaces it as the
// process exit code; shell scripts and CI pipelines use these to branch.
// Keep the mapping stable: existing scripts will rely on specific codes.
//
// See docs/SPEC.md > "Exit codes" for the prose contract. The values
// here are the source of truth; the spec mirrors them.
//
// Note: 5 is intentionally unused. It was historically reserved for an
// ErrNotOwner path that the owner-collapse contract eliminated (the SSH
// surface never observes ErrNotOwner because service.requireOwner
// collapses non-owner reads to ErrNotFound so existence can't leak
// across identities). Do not reuse 5 for a new meaning; pick the next
// free slot to avoid retroactively changing the semantics of a code
// that was already documented.
const (
	ExitOK          = 0 // success
	ExitErr         = 1 // generic / unclassified failure
	ExitUsage       = 2 // malformed args, bad verb, parser failure
	ExitAuth        = 3 // identity required (no key) or missing-owner service error
	ExitNotFound    = 4 // not found, including owner-collapsed permission failures
	_               = 5 // reserved; previously ErrNotOwner before owner-collapse made it dead. Do not reuse.
	ExitSybilRefuse = 6 // Sybil per-subnet rate-limit refusal
)
