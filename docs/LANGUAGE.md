# Ubiquitous language

One word per concept, and the same word in the code, the help text, error
strings, log lines and these docs. Where a second word has crept in, this
file says which one wins and which one goes.

The rule that decides ties: **prefer the word already in the user's mouth.**
`ssh <apex> list` prints a `KIND` column, and people read that column before
they read any doc.

## The words

| word | is | is not |
| --- | --- | --- |
| **paste** | the unit a user creates, owns, lists, versions and deletes | not "artifact", not "record", not "entry" |
| **site** | a paste whose content is many files under paths | not "directory", not "bundle", not "static site" in prose |
| **kind** | what a paste's content IS: `markdown`, `html`, `diff`, `csv`, `site`, ... | not "type", not "format" |
| **version** | one immutable snapshot of a paste's content | not "revision", not "generation" |
| **slug** | the short id in the URL | not "name", not "id" |
| **name** | the optional human label a user sets with `rename` | not "title" |
| **identity** | the owner, derived from an SSH public key | not "user", not "account" |
| **manifest** | a site's path -> entry map | not "index", not "tree" |
| **blob** | stored content bytes, addressed by hash | not "object", not "file" |
| **entry** | one row of a per-owner enumeration index | not "pointer" |

A site is a KIND of paste, not a sibling of one. That single sentence is what
keeps the vocabulary from splitting in two again: everything true of a paste is
true of a site, and the only difference is how many files its manifest holds.

## What goes

**`artifact` is retired.** It arrived with the paste/site collapse and duplicates
`paste` exactly. It is also three syllables where the thing it names already had
one. Measured before writing this: `paste` appears ~1,300 times and `site`
~1,060, against `artifact`'s ~93 - so the cheap rename is the newcomer, not the
entrenched word. Ten of those 93 had already reached user-facing packages, which
is how a private synonym becomes a public one.

Replace `artifact` with `paste`. Where the sentence is specifically about the
many-file case, use `site`.

**`directory` and `document` are not nouns for our things.** Use `site` and
`paste`. `directory` is fine as plain English for a folder inside an uploaded
tarball, because that is what it is; it is not a synonym for `site`.

## Where this has to hold

Naming drift is invisible in code review and obvious to a user reading two
screens that disagree. So the same word has to survive all of:

- exported and unexported Go identifiers
- `help` output and per-verb help
- error strings, including sentinel text
- log lines an operator will grep
- `docs/SPEC.md`, `README.md` and this file

## Applying it

The rename is mechanical and should land as its own commit, touching no
behaviour. Check the user-facing surfaces by hand afterwards - `help`, the error
sentinels and the landing page - because those are the ones a grep over `.go`
files will under-count and a user will over-notice.
