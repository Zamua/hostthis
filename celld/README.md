# hostthis on celld

Experimental second backend for the metadata plane. shale remains the default;
this exists to be measured against it.

`src/index.js` is the Worker plus the Durable Object classes. The Go side talks
to it over HTTP from `internal/celld`, because celld serves HTTP and WebSocket
only and hostthis's interface is SSH: `internal/ssh` stays a Go process and
becomes a client.

## One application per fleet

A celld fleet serves exactly one application. `deploy/current.json` at the root
of the deploy prefix names one script, so a second `celld deploy` against the
same bucket replaces whatever was there, with no error at deploy time and no
warning at startup. hostthis therefore gets its own bucket and its own nodes,
sharing only the MinIO cluster.

## Cell topology

| cell | holds |
| --- | --- |
| `<scope>` (owner identity) | that owner's outstanding durable intents |

The intent log is scoped to the owner because that is the recovery unit, and
because a paste's row and its owner index live in different cells with no
transaction spanning them - the same condition that made the intent log
necessary on shale.
