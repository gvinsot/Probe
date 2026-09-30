# Repository graph

`lint` and `review` build a graph of the whole head commit, not only of the
diff, and give it to the AI reviewer so it can reason beyond the changed
lines: who depends on a changed package, which component a change crosses
into, which types implement a changed interface, how a changed function is
reached, which external dependencies a component uses. The report records
the graph's summary and how the change moves its structure against the base.
`--graph=false` turns it off.

## What the graph holds

| Node | Source |
| --- | --- |
| `component` | a directory holding `go.mod`, `package.json`, `Cargo.toml`, `pyproject.toml` or `setup.py` (the deepest one wins; the repository root does not count), else the top-level directory |
| `package` | a Go package (named by its import path), or a source directory |
| `file` | a Go, TypeScript/JavaScript, Python or Rust source file |
| `function` | every function, method and interface method of the static symbol index (Go type-checked, the others lexical; see [impact analysis](IMPACT.md)) |
| `type` | Go structs, interfaces and named types; TypeScript classes, interfaces, enums and type aliases; Python classes; Rust structs, enums, traits and type aliases |
| `dependency` | an external module or package: `go:`, `npm:`, `pypi:` or `cargo:` |

| Edge | Meaning |
| --- | --- |
| `contains` | component → package → file → function or type |
| `calls` | function → function it calls or uses as a value (from the symbol index; package initializers and module bodies are attributed to their file) |
| `member_of` | method → its type |
| `implements` | Go type → interface it implements (bounded search) |
| `imports` | file → package, file or dependency |
| `depends_on` | package → package and component → component or dependency, aggregated from the imports, with their count |
| `declares` | component → dependency its manifest declares |

Imports are resolved per language: Go by module path (`go.mod` of every
module of the repository) or to the longest `require` it matches; relative
TypeScript/JavaScript imports to files (including `./x.js` for `x.ts`), bare
ones to npm packages; Python modules through the package tree (from the
root, component roots and their `src/`), third-party ones only when a
manifest declares them; Rust `crate::`, `mod x;` and declared crates. The
standard libraries, Node built-ins and unresolved aliases are left out.

The graph is built from the Git objects of the commit, never from a working
tree, so a commit always gives the same graph. Sensitive paths (`.env`, keys,
certificates) are neither read nor listed. Types and imports are read line by
line outside Go: a statement split over several lines is missed, as the
lexical index misses it.

## How the reviewer uses it

The reviewer's input gains a `graph` field: every component with its
dependencies, and for each changed file its component, package, the packages
that depend on it, the files that import it, and how many calls reach its
functions from other files and other components. Three read-only tools query
the graph of the head commit, in `review` and `review --read-only`:

| Tool | Answers |
| --- | --- |
| `graph_search` | nodes whose name or path contains a text, optionally of one kind (at most 30) |
| `graph_neighbors` | the nodes linked to a node, following chosen edge kinds in or out, up to 3 hops (at most 100) |
| `graph_path` | a shortest chain of edges from one node to another (at most 12 edges), forward or undirected |

A node is named by its id, or by its name or any suffix of it: `Cart.Total`,
`internal/payment`, `npm:react`. Every call is audited like the other
reviewer tools, and every answer is marked as an observation of committed
files: an edge is a place to look, a missing edge is not proof that none
exists (function values, reflection, dynamic dispatch and generated code are
not resolved), and a concern found through the graph is an `UNVERIFIED`
hypothesis until an experiment supports it.

## Report

The `graph` section of `confidence-report.json` (schema
`#/$defs/graph`) and the "Repository Graph" Markdown section record:

- `status` (`built`, `partial` with `limitations`, or `unavailable` with a
  `reason`: the review then goes on without it), the head `commit`, and
  whether functions and calls are included (`calls`);
- node and edge counts by kind, and each component with its files,
  component dependencies and number of external dependencies;
- `delta`: the components, packages, types and external dependencies, and
  the `depends_on` and `declares` edges, added or removed against the base
  commit (at most 100 each, with totals). A new external
  dependency or a new dependency between components shows here even when
  the diff hides it in a manifest or an import line;
- `cache`: `hit`, `stored` or `off`.

The graph adds no signal and never changes the exit code.

## Cache

Graphs are cached by commit so that each is built once: the graph of a base
branch tip serves every review against it, and a review run again on the same
head reads its graph. The default location is the user cache directory
(`probe/graph` under `$XDG_CACHE_HOME`, `~/Library/Caches` or
`%LocalAppData%`); `--graph-cache DIR` names another one and
`--graph-cache off` disables it. In CI, keep the directory between runs (for
example with `actions/cache`) to reuse base graphs.

The directory follows the rules of the [execution cache](EXECUTION_CACHE.md):
outside the repository and the report directory, not a link, owner-only. An
explicit directory that breaks them exits 3; the default one only prints a
warning and runs without cache. An entry is keyed by the graph format, the
probe build (version and executable hash), the commit, the limits and whether
calls are included; every read checks the key, the content hash, the format
and the commit, and deletes an entry that fails. These checks detect
corruption, not forgery: anyone who can write the directory can plant a
graph, which only ever reaches the reviewer as observations. Entries expire
after 30 days without use; at most 200 are kept.

## Limits

At most 20,000 files and 64 MiB are read, 300,000 nodes and 2,000,000 edges
kept, and the interface-implementation search runs for at most 20 seconds;
anything beyond is left out and listed in `limitations`. When the impact
index is itself limited, the graph says so. Building the graph of this
repository (about 500 files) takes about 0.1 s on top of the symbol index.

## Outside a review

```sh
probe graph build [--commit HEAD] [--out graph.json]      # build (or read from the cache), optionally export
probe graph query search Cart.Total [--kind function]
probe graph query neighbors internal/payment --kinds depends_on --direction in --depth 2
probe graph query path api.Handle Cart.Total
```

The answers are the JSON the reviewer receives. Coding agents can use them
the same way; the [Claude Code plugin](../../plugins/probe/README.md) does
through `/probe:graph`.
