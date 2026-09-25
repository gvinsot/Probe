# Impact analysis

Impact analysis points a reviewer at code the change did not touch but that depends on what it changed: the places in unchanged code that reference a changed Go function or method, and the existing Go tests that reach it within a few calls. It also backs the reviewer's `find_references`, `inspect_symbol` and `find_callers` tools.

It is static and approximate. It never executes repository code, never creates evidence, and never removes a signal, lowers a severity or supports a dismissal. It only adds low-severity review targets and tool observations. A listed caller is a place to review, not a defect, and an absent caller is not proof that none exists.

## What the index is

`lint` and `review` build the index on the host, in process, from committed Git objects:

- Files are listed with `git ls-tree` and read with one `git cat-file --batch` stream (`gitrepo.Tree` and `gitrepo.ReadBlobs`), never from the working tree. Only regular files are read: symlinks and submodules are skipped.
- Only `.go` files and `go.mod` files are read, outside the directories the go command ignores (`testdata`, `vendor`, and names starting with `_` or `.`). Files whose path the sandbox also hides (`.env*`, `*.pem`, `.ssh/`, `credentials`, and the rest of the snapshot exclusions) are neither read nor indexed.
- Every `go.mod` defines a module; a directory belongs to its innermost module. Files outside every module are not indexed.
- Files are selected as the go command would select them for `linux/amd64` with cgo enabled (file-name suffixes and `//go:build` lines), then parsed and type-checked with the Go standard library (`go/parser`, `go/types`), one package at a time, in dependency order. A package's in-package test files are checked with it; an external test package (`package x_test`) is checked on its own.
- Imports from outside the repository are **not loaded**: they are replaced by empty packages, so calls into them are not resolved. The index never runs the go command, cgo, `go generate`, build scripts, gopls or any other tool, and uses no network.

The index is built only when the change touches at least one Go file. Otherwise the section says `not_applicable` and the reviewer tools stay lexical.

## Enabling and disabling

Impact analysis is on by default for `lint` and `review`. `--impact=false` disables it: the report then has no `impact` object, no impact signal is added, and the reviewer tools stay lexical. There is no policy key, so the candidate cannot enable, disable or tune it, and no release ordering applies to it. `--impacted-tests` (review only) needs the index and exits 3 with `--impact=false`.

## Changed functions

A changed function is a function or method of a changed non-test Go file whose candidate version differs from its baseline version:

- `signature_changed` when the tokens of the declaration up to its body differ;
- `body_changed` when only the tokens of the body differ.

Tokens are compared with the digest the linter uses (`linter.TokenDigest`): comments and spacing are ignored. Joining or splitting lines can change where Go inserts semicolons, so such a reformatting is reported as `body_changed`, the conservative direction. A function is identified by its directory, package clause and name (the receiver type for a method), not by its file, so a function moved between files of one package without other change is not listed. Added and removed functions are not changed functions. `init` functions, blank (`_`) functions and `main` in package `main` cannot be called and are not listed. Test files are not compared: [changed baseline tests](BASE_TESTS.md) handle changed tests.

## Callers and resolution

The callers of a changed function are its reference sites in **unchanged, non-test code**: a reference inside a changed or added function, on an added line, or in a `_test.go` file belongs to the change or to the tests and is not listed. One site is kept per line. Each site has a resolution:

| Resolution | Meaning |
| --- | --- |
| `static` | `go/types` resolved the identifier to the changed function (generic functions and methods of instantiated types map to their origin; promoted methods resolve to the embedded type's method). This includes references that do not call, such as a function value passed as an argument. |
| `interface` | A call of a method of a named interface that the changed method's receiver type (`T` or `*T`) implements. The call **may** dispatch to the changed method; dispatch is not resolved. |

A method call on a value whose type could not be resolved (typically a value from a package outside the repository) is recorded by name only. It appears in `find_references` output as a `name_match` and never in callers, signals or test selection.

## Reaching tests

A reaching test is an existing `TestX(t *testing.T)` function (recognized syntactically in a `_test.go` file, since the testing package is not loaded) that reaches the changed function within 3 references, through static references or interface calls. Each test is reported at the smallest depth found, breadth first in index order, with the chain of declarations (`via`) and whether the change touched its file (`file_changed`). A test that reaches a function is not evidence that it asserts that function's behavior. Benchmarks, examples and fuzz targets are not selected.

## Signals

| Kind | Severity | Where | When |
| --- | --- | --- | --- |
| `impacted_caller` | low | the caller's line (new side) | one per listed caller site: "Unchanged caller of a changed Go function", "Unchanged reference to a changed Go function" or "Unchanged interface call that may dispatch to a changed Go method" |
| `analysis_limited` (symbol `impact_index`) | medium | the first caller site left out, else line 1 of the first changed Go file | at most one per run, when caller signals were left out by the caps, or when the index is limited or unavailable and changed functions are concerned |

At most 10 `impacted_caller` signals are added per changed function and 100 per run; the `analysis_limited` signal states how many caller sites were left out. The low targets land on unchanged lines, so the review surface's focused-line count does not change. Neither kind requests human review or changes the exit code.

## Statuses

| `impact.status` | Meaning |
| --- | --- |
| `not_applicable` | No Go file changed; no index was built. |
| `indexed` | The index was built without a known gap. It remains approximate (see above). |
| `limited` | The index was built, but part of it is known to be missing. `reason` lists each cause: files larger than 2 MiB, files that could not be parsed, files whose package clause differs from their directory's package, `go.mod` files without a readable module path or with a duplicate module path, packages whose type check stopped, the reference limit, the unresolved-call limit, the time limit, changed files that could not be compared, or changed functions beyond the listed 200. |
| `unavailable` | No index could be built: no `go.mod`, a tree over the file or byte limits, more packages than the limit, or a Git read failure. Changed functions are still listed, as not indexed. |

A changed function the index does not hold (for example one in a `_windows.go` file, excluded by the `linux/amd64` constraints) has `indexed: false` and a `reason`; its callers and tests were not searched.

## Report fields

`impact` is present in `lint` and `review` unless `--impact=false`.

| Field | Content |
| --- | --- |
| `status`, `reason` | See Statuses. |
| `indexed_files` | Go files the index type-checked. |
| `changed_functions[]` | `path`, `line`, `end_line`, `symbol` (import path and name, for example `example.test/shop/cart.Cart.Total`), `change`, `indexed`, `reason`, `callers` (at most 10: `path`, `line`, `symbol` of the referencing declaration, `depth` 1, `resolution`), `callers_total` (sites the bounded search found), `tests` (at most 20: `name`, `path`, `line`, `package` import path, `depth` 1 to 3, `resolution`, `via`, `file_changed`), `tests_total`. |
| `tests_status`, `tests_reason`, `tests[].evidence_id`, `tests[].status`, `tests[].reason` | Impacted tests, recorded only with `--impacted-tests` (review only). Each test status comes from verified evidence for that test alone; a status stored in a saved report is re-derived when it is rendered. |
| `note` | The fixed note: what the index is and what it does not establish. |

The Markdown report has an `## Impact Analysis` section after Changed-line Execution: the index status, each changed function (at most 20) with up to 5 callers and 5 tests, and the note. The console prints one line, for example:

```text
Impact analysis (static Go index, approximate): 2 changed Go functions; 2 caller sites in unchanged code and 2 reaching tests, counted per function.
```

It prints nothing when no Go file changed, and the reason when the index is unavailable.

## Reviewer tools

When an index exists, `find_references`, `inspect_symbol` and `find_callers` answer from it for Go functions and methods. Every answer carries `"method": "go_static_index"` and a fixed `limitations` text, and redacted one-line snippets of at most 240 bytes.

- A symbol may be a full key (`example.test/shop/price.Total`), a key suffix (`price.Total`), a package-qualified name, a bare name (`Total`, `Cart.Total`) or the `(*T).M` form. Several matches return the candidates instead.
- `find_references`: every recorded site, static then through interfaces, at most 100, plus unresolved method calls of the same name (`name_matches`, at most 20).
- `find_callers` (`depth` 1 to 3, default 1): the reference sites of the symbol, then of the declarations holding them, breadth first, at most 100 results and 5,000 visits, each with its depth and `via` chain. Test functions are listed but not followed.
- `inspect_symbol`: the declaration (with `changed` when the change modified it), the number of reference sites, what it references, the interface methods a method may be dispatched from or the methods that implement an interface method, and the reaching tests.

A symbol that is not an indexed Go function or method (a type, a variable, TypeScript code) gets the lexical answer with an `index` note saying why; so does every query when no index exists. Index answers are observations: they never create evidence records.

## Limits

| Limit | Value | Effect when reached |
| --- | --- | --- |
| Go and `go.mod` files read | 20,000 | `unavailable` |
| Source bytes read | 64 MiB | `unavailable` |
| One file | 2 MiB | the file is not indexed; `limited` |
| Packages | 5,000 | `unavailable` |
| Recorded references | 2,000,000 | later references dropped; `limited` |
| Unresolved method calls | 200,000 | later ones dropped; `limited` |
| Changed functions listed | 200 | the rest omitted; `limited` |
| Time | 120 s | checked between packages and on each type error; remaining packages not indexed; `limited` |
| Depth | 3 | callers and tests beyond it are not searched |
| Callers listed / signals | 10 per function; 100 signals per run | `callers_total` and the `analysis_limited` signal state the rest |
| Reaching tests listed | 20 per function | `tests_total` counts all found |
| Tool answers | 100 results, 5,000 visits, 20 candidates, 20 name matches | `truncated` |

The time limit makes the result depend on the machine when it is reached; it is then reported. Measured costs are in [performance](PERFORMANCE.md).

## Exit effects

The static section never changes the exit code: `impacted_caller` (low) and `analysis_limited` (medium) do not request human review, and a limited or unavailable index is not an operational failure. Only a cancelled run stops the analysis with an error. Impacted tests (`--impacted-tests`) may request review; nothing in impact analysis produces exit 1.

## Trust and security

The index parses and type-checks untrusted committed source in process, with the standard library only. It runs no repository code, no tool and no network access, reads only committed Git objects of the candidate commit (and of the base commit for the changed files), and skips sensitive paths and symlinks. Its work is bounded by the limits above; a panic inside `go/types` is recovered per package and reported as `limited`. An imported package is marked so that `go/types` does not build error messages for names it lacks, which would otherwise cost a lookup over the imported package per missing name. The candidate fully controls what is indexed, so it can hide callers (reflection, function values) or add reference sites: the index therefore only adds low-severity signals and tool observations. Snippets and report strings are redacted.

## What this does not claim

- Not "no callers", "unused", "all callers", or a complete call graph: an absent caller is not proof of absence, and callers outside the repository, through function values, reflection, `go:linkname`, assembly, generated code, or imports that are not loaded, are not found.
- Not that an interface call dispatches to the changed method at run time: it is possible dispatch only.
- Not that a caller is affected by the change, or that the change is safe to make.
- Not that a reaching test exercises, covers or asserts the changed behavior: it reaches the function within 3 references in the static index.
- Not the compiler's view: imports from outside the repository are not loaded, `linux/amd64` constraints select the files, and a directory's in-package test files are checked with its package.
- TypeScript and JavaScript are not indexed; their lookups stay lexical.

<!-- F6b:begin -->
<!-- F6b:end -->
