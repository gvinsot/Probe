---
name: graph
description: Explore the Probe repository graph of the current commit — components, packages, files, functions, types and external dependencies, with their calls, imports, dependencies and interface implementations — to see what a change affects beyond the diff. Use when the user asks who calls or depends on something, what a component depends on, which types implement an interface, or how one part of the code reaches another.
argument-hint: "[question | search TEXT | neighbors NODE | path FROM TO]"
allowed-tools: Bash(probe graph *) Bash(node *) Read Grep Glob
---

# Probe repository graph

## Current state

!`node "${CLAUDE_PLUGIN_ROOT}/scripts/probe-report.mjs" status`

## Request

`$ARGUMENTS`

Probe builds a graph of the committed files of a commit (cached by commit, so
repeated queries are fast) and answers three queries with JSON. Run them with
Bash from the repository; add `--commit REV` to query another commit than
`HEAD`:

- find nodes: `probe graph query search TEXT [--kind component|package|file|function|type|dependency]`
- follow relationships: `probe graph query neighbors NODE [--direction in|out|both] [--kinds calls,imports,depends_on,implements,member_of,contains,declares] [--depth 1..3]`
- find how one node reaches another: `probe graph query path FROM TO [--kinds …] [--undirected]`

A node is named by the id a previous answer gave, or by its name or any
suffix of it (`Cart.Total`, `internal/payment`, `npm:react`). An ambiguous
name returns candidates: repeat with an id.

Typical questions:

| Question | Query |
| --- | --- |
| Who calls this function? | `neighbors NAME --kinds calls --direction in --depth 2` |
| What depends on this package? | `neighbors DIR --kinds depends_on --direction in` |
| What does this component use? | `neighbors COMPONENT --kinds depends_on,declares --direction out` |
| Which types implement this interface? | `neighbors INTERFACE --kinds implements --direction in` |
| How does A reach B? | `path A B` |

Answer the user's question from the JSON, citing `path:line` locations, and
read the code where it matters. The graph is static: an edge is a place to
look, a missing edge is not proof that none exists (function values,
reflection, dynamic dispatch and generated code are not resolved), and
uncommitted changes are not in it — say so when the working tree differs.
