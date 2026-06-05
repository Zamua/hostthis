# Architecture Decision Records

We file an ADR here whenever we make a large design decision worth remembering — anything that future-us would otherwise have to re-derive from scratch (or worse, would silently reverse without realizing we'd considered it).

## When to write one

Write an ADR when the decision:
- Has multiple defensible alternatives
- Is hard to reverse later (data shape, URL shape, wire protocol, identity model)
- Would surprise a reader of the code who didn't see it discussed

Skip an ADR for small / easily-reversed choices (lib selection, dir layout, code-style).

## Shape

One file per decision, named `NNNN-short-slug.md` where `NNNN` is the next sequential number (`0001-`, `0002-`, …). Inside:

```
# NNNN. Short title

- **Date**: YYYY-MM-DD
- **Status**: proposed | accepted | superseded by NNNN

## Context
What was the situation that forced a choice?

## Options
What did we consider? Bullet each one with a sentence on why it was on the table.

## Decision
What did we pick? Why? Be brief.

## Consequences
What follows from this — good and bad.
```

Don't edit an accepted ADR. If we change our mind, write a new ADR that supersedes it.
