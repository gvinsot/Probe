# Probe

**Spend review time on the changes that need your judgment.**

Probe is a Go CLI for reviewing AI-assisted pull requests. It maps Git changes to risk signals, optionally runs isolated checks and adversarial tests, and produces a focused review plan with traceable evidence.

It works without an AI provider. It does not assign confidence percentages or automatically approve PRs. Passing tests and a small review surface are not correctness guarantees.

## Quick start

Download a binary for Windows, Linux or macOS from [GitHub Releases](https://github.com/gvinsot/Probe/releases). Git is required at runtime. Docker with Linux containers is required only to execute repository code.

To build from source, install Go 1.23+ and run from the `app` directory of the repository:

```sh
go build -o probe ./cmd/probe
go test ./...
```

To build Windows amd64 **and Linux amd64/arm64** together, run from the `app` directory on any supported host:

```sh
go run ./tools/build
```

Binaries are written to `dist/windows-amd64/probe.exe`, `dist/linux-amd64/probe` and `dist/linux-arm64/probe`. The same command produces portable ZIP/tar.gz archives and `dist/SHA256SUMS`. Every successful CI run retains these archives as the `probe-build` artifact.

Put the binary on your PATH, then run `probe lint --base main` in a Git repository. Windows builds produce `probe.exe` when using `go build ./cmd/probe`. The [release workflow](docs/RELEASE.md) prepares portable archives for distribution.

Open `.probe/CONFIDENCE_REPORT.md`; see a [real example report](examples/CONFIDENCE_REPORT.md). Its companion `confidence-report.json` contains commit IDs, changed lines, signals, checks, hypotheses, evidence, audit events and artifact hashes. Add `.probe/` to your project's `.gitignore`.

```sh
probe init
# Review .probe.json and commit it to your base branch.
probe review --base main --ci
```

Checks require an **image containing the toolchain and project dependencies**: preload it, or let the trusted base-branch policy derive it with an optional `prepare` command ([dependency preparation](docs/PREPARE.md)). Probe never pulls images and never installs dependencies from candidate content. For a Go project without third-party dependencies, preload the default image in a trusted environment:

```sh
docker pull golang:1.26-bookworm
probe review --base main
```

Use `--config .probe.json` to explicitly try the local policy before committing it. Missing Docker or images produce an operational error; execution never falls back to your host.

**See what needs your attention, even when tests pass.** In this [real example report](examples/CONFIDENCE_REPORT.md), all three checks passed, but Probe highlighted a changed authorization function for human review. No AI provider was used. Excerpt from the generated `.probe/CONFIDENCE_REPORT.md`:

```markdown
## Automated Checks

- **PASS** test (check-1; exit 0; 6646 ms)
- **PASS** typecheck (check-2; exit 0; 5425 ms)
- **PASS** build (check-3; exit 0; 599 ms)

## Suggested Human Review

- **high** auth.go:4–4 (new): Authentication or authorization function body changed
- **low** auth.go (whole file): No nearby test file changed

## Review Surface

Focused review: **1 / 2 changed lines**.
```

Start with the flagged lines and the reason for each review target. These signals guide your review; they do not establish a confirmed bug.

## Capabilities

- Immutable Git comparisons: renames, deletions, binaries and merge-base semantics.
- Go AST comparison of exported declarations and changed security/payment function bodies, plus labelled lexical risk signals and branch-growth heuristics across text files.
- Security checks without a model on added lines of every text file (configuration included): hard-coded credentials and private keys (masked in the report), credentials or secrets in URLs, hard-coded e-mail and IP addresses, TLS verification disabled, debug mode, excessive Linux permissions or container privileges, and disabled protections (CSRF, CORS, cookies, JWT, SELinux/firewall, public cloud resources). They are regex heuristics: medium in tests, and low for configuration patterns quoted in documentation ([security checks](docs/SECURITY_CHECKS.md)).
- Signals for sensitive paths, dependencies, network/DB calls, authentication, removed validation/error handling, unsafe constructs, missing associated changed tests, and added Go lines that a recorded coverage run did not execute.
- Configured test, typecheck and build commands executed as argv arrays in disposable containers.
- An optional reviewer with bounded source/search/test tools and temporary generated tests.
- Differential evidence: a generated test passing on baseline and failing on candidate can support a reproduced issue. Missing evidence remains **UNVERIFIED**.
- Deduplicated review ranges with old/new coordinates and counts of actual changed lines.
- Optional trusted dependency preparation (`prepare` policy object): the base branch's own command runs on inputs exported from the base commit, and its container becomes the local image for every check of the run ([dependency preparation](docs/PREPARE.md)).
- An opt-in baseline execution cache (`--cache-dir`) and up to four initial checks at a time (`--parallel`); a replayed baseline run never supports a positive result ([execution cache](docs/EXECUTION_CACHE.md)).
- Changed baseline tests (`--base-tests`): the baseline version of each Go test function (or TypeScript/JavaScript test, with a Vitest or Jest template) the change modified or removed runs on baseline and candidate code, and a baseline pass with a candidate failure is reported as `FAILS_ON_CANDIDATE` for review, not as a defect ([changed baseline tests](docs/BASE_TESTS.md)).
- Impact analysis (`--impact`, on by default): a static index lists callers of changed functions and the existing tests that reach them, labelled approximate. Go packages are type-checked; TypeScript/JavaScript, Python and Rust sources are scanned lexically and calls are linked by name (`name` resolution). `--impacted-tests` runs the reaching Go tests on both revisions ([impact analysis](docs/IMPACT.md)).
- Repository graph (`--graph`, on by default): a persistent graph of the whole head commit — components, packages, files, functions, types and external dependencies, linked by calls, imports, dependencies, membership and interface implementations — cached by commit and queried by the AI reviewer (`graph_search`, `graph_neighbors`, `graph_path`) to reason beyond the diff. The report records its summary and the structural delta against the base: new external dependencies, new dependencies between components or packages, added or removed types. `probe graph build` and `probe graph query` use it outside a review ([repository graph](docs/GRAPH.md)).
- Languages: Go, TypeScript/JavaScript, Python and Rust. `probe init` detects `go.mod`, `Cargo.toml`, `tsconfig.json`/`package.json` and `pyproject.toml`/`setup.py`/`requirements.txt`, and writes matching sandbox images and commands (`--language go|typescript|javascript|python|rust`). Changed-line coverage reads a Go profile or an LCOV report (TypeScript/JavaScript through Vitest or Jest); changed baseline tests, impacted-test runs, fuzzing and mutation testing cover Go and TS/JS (TS/JS with Vitest or Jest).
- Optional deterministic differential fuzzing (`fuzz` policy object): changed Go functions, and exported TypeScript/JavaScript functions read lexically, whose signature is unchanged run on identical seeded inputs on both revisions, without a model, and a confirmed difference is shown with both values ([differential fuzzing](docs/FUZZ.md)).
- Optional mutation of added Go lines, or TypeScript/JavaScript lines with a Vitest or Jest command (`mutation` policy object): surviving mutants are reported for review, killed mutants are only counted, and no mutation score is computed ([mutation of added lines](docs/MUTATION.md)).
- Observation experiments: a generated test may record values instead of asserting them, and a value that differs between revisions, after a live baseline repeat agreed with the first baseline run, is shown with both values as a behavior divergence for a human to judge ([observations](docs/OBSERVATIONS.md)).
- Jira issues as context (`--jira KEY` or `--jira auto`, for review, lint and plan): Probe fetches the ticket the change implements (Jira Cloud or Server/Data Center, configured by `PROBE_JIRA_URL` and `PROBE_JIRA_EMAIL`/`PROBE_JIRA_TOKEN`) and makes its summary, description and acceptance criteria the intent, so the reviewer and intent tests compare the implementation with the requirement ([Jira issues](docs/JIRA.md)).
- Linear issues as context (`--linear KEY` or `--linear auto`, for review, lint and plan): the same for Linear, configured by `PROBE_LINEAR_API_KEY`; `auto` also reads Linear's lower-case branch names and skips candidates Linear does not know ([Linear issues](docs/LINEAR.md)).
- Notion pages as context (`--notion PAGE[,PAGE]`, up to 5 URLs or IDs, for review, lint and plan): product, architecture or requirements pages join the intent, configured by `PROBE_NOTION_TOKEN`; their list items stay prose unless they sit under an "Acceptance criteria" heading ([Notion pages](docs/NOTION.md)).
- Acceptance criteria from `--intent` or `--intent-file`: the reviewer may write a candidate-only test for one quoted criterion, and a failure is reported as `INTENT_TEST_FAILED`, apart from reproduced issues and without a baseline control ([intent criteria](docs/INTENT.md)).
- Pre-change plans (`probe plan --intent-file FILE`, provider required): the model simulates the implementation read-only and submits a plan; Probe evaluates it with fixed rules (critical paths, callers and reaching tests, exported signatures, dependency manifests, new packages) into `PLAN.json` and `PLAN.md`, and `review --plan` / `lint --plan` report every file, exported signature, critical path or manifest the diff changes outside the plan. `review --plan` ends with a **plan gate**: no human review is required only when the re-assessed plan raised no category and was fully measured, the change conforms to it, the checks passed and nothing else in the report requests review; with `--ci` the gate is the exit code (0 or 2) ([plans, scope drift and the plan gate](docs/PLAN.md)).
- Evidence-only exports: `--format sarif,pr-comment` writes only findings backed by recorded evidence, and an empty export is not approval ([exports](docs/EXPORTS.md)).

Go risk signals come from syntactic analysis. Impact analysis builds a static index of the repository's own Go packages on the host from committed source, without running repository code or loading imports from outside the repository, so its call graph is approximate and partial: interface edges are possible dispatch only, and an absent caller is not proof of absence. TypeScript support is lexical in this version. Signals are reasons to investigate, not confirmed bugs. Missing test changes do not establish missing coverage. A recorded coverage run establishes only which added lines ran and which did not; neither establishes that a line is tested.

## Commands

```sh
probe lint --base main                       # no Docker or API
probe review --base main                     # configured sandbox checks
probe review --base main --checks=false      # explicitly skip execution
probe review main..HEAD                      # exact endpoints
probe review main...HEAD                     # common ancestor to head
probe review --base main --intent-file PR.md # acceptance criteria
probe review --base main --jira auto         # the Jira issue named by the branch or commits
probe review --base main --linear ENG-123    # a Linear issue as the intent
probe review --base main --notion URL        # a Notion page as context
probe review --base main --base-tests        # baseline versions of changed tests
probe review --base main --impacted-tests    # existing tests that reach changed functions
probe lint --base main --impact=false        # skip the static impact index
probe review --base main --cache-dir "$HOME/.cache/probe" --parallel 2
probe review --base main --deadline 25m      # overall bound for execution stages
probe review --base main --format markdown,json,sarif,pr-comment
probe plan --intent-file demande.md          # pre-change plan: PLAN.json, PLAN.md (provider required)
probe review --base main --plan .probe/PLAN.json  # scope drift against the plan
probe report --input .probe/confidence-report.json
probe graph query neighbors internal/payment --kinds depends_on --direction in   # repository graph
probe review --help
```

`--base main` uses the merge base by default; `--exact` selects a direct comparison. Only **committed** files are reviewed. Both refs resolve to immutable IDs. Local edits and untracked files are excluded. CI needs enough Git history to resolve the base.

`--out DIR` sets the report directory. `--format` selects the files to write: `markdown` (`CONFIDENCE_REPORT.md`), `json` (`confidence-report.json`), `sarif` (`confidence-report.sarif`) and `pr-comment` (`PR_COMMENT.md`); the default is `markdown,json`, and console output stays concise. Re-rendering a saved report neither reruns nor authenticates its evidence.

| Flag | Commands | Default | Effect |
| --- | --- | --- | --- |
| `--report-url URL` | review, lint, report | none | Links the full report from `PR_COMMENT.md`. Requires `pr-comment` in `--format` and an `https` URL. |
| `--base-tests` | review | off | Runs the baseline versions of changed Go tests (or TypeScript/JavaScript tests, with a Vitest or Jest template) on candidate code. |
| `--fuzz` | review | on | `--fuzz=false` skips differential fuzzing configured in policy. |
| `--impact` | lint, review | on | `--impact=false` skips the static impact index. |
| `--graph` | lint, review | on | `--graph=false` skips the repository graph ([repository graph](docs/GRAPH.md)). |
| `--graph-cache DIR` | lint, review, graph | user cache directory | Where graphs are cached by commit, outside the repository and the report directory; `off` disables the cache. An unusable explicit directory exits 3. |
| `--jira KEY` | lint, review, plan | none | Adds the Jira issue KEY (or, with `auto`, the issue named by the branch or the commit messages) to the intent. Needs `PROBE_JIRA_URL`; a Jira failure exits 3 ([Jira issues](docs/JIRA.md)). |
| `--linear KEY` | lint, review, plan | none | Adds the Linear issue KEY (or, with `auto`, the issue named by the branch or the commit messages) to the intent, after any Jira issue. Needs `PROBE_LINEAR_API_KEY`; a Linear failure exits 3 ([Linear issues](docs/LINEAR.md)). |
| `--notion PAGES` | lint, review, plan | none | Adds up to 5 Notion pages (URLs or IDs, comma-separated) to the intent as context, after any Jira or Linear issue; only items under their "Acceptance criteria" headings become criteria. Needs `PROBE_NOTION_TOKEN`; a Notion failure exits 3 ([Notion pages](docs/NOTION.md)). |
| `--plan FILE` | lint, review | none | Checks the diff against the contract of a PLAN.json written by `probe plan` and adds the `plan_drift` section ([plans and scope drift](docs/PLAN.md)). |
| `--impacted-tests` | review | off | Runs the existing Go tests (or TypeScript/JavaScript tests, with a Vitest or Jest template) that reach changed functions on both revisions. |
| `--cache-dir DIR` | review | none: no cache | Enables the baseline execution cache in DIR, which must be outside the repository and the report directory. |
| `--parallel N` | review | 1 | Runs up to N (at most 4) initial checks at a time. The runtime budget is unchanged. |
| `--allow-prepare-network` | review | off | Lets the policy's `prepare` command use the network, only if the policy also enables it. |
| `--deadline D` | review | none | Bounds preparation, sandbox runs and the reviewer to D (1m to 24h), keeping 30 s to write the report. |

The execution flags (`--base-tests`, `--fuzz`, `--impacted-tests`, `--cache-dir`, `--parallel`, `--allow-prepare-network` and `--deadline`) apply to `review` only: set explicitly on `lint`, they exit 3. `--base-tests` and `--impacted-tests` also exit 3 with `--checks=false`, and `--impacted-tests` exits 3 with `--impact=false`. Every flag error exits 3 before any container starts. `--no-network` also keeps the `prepare` container offline.

| Exit | Meaning |
| --- | --- |
| 0 | Report completed without a reproduced high/critical issue. Without `--ci`, unresolved areas and other review requests do not change this code. |
| 1 | High/critical hypothesis supported by differential failure. Nothing else produces 1: not divergences, not tests failing on candidate code, not intent-test failures, not surviving mutants. |
| 2 | With `--ci`: human review required, including high-risk signals, unverified areas, incomplete checks, behavior divergences, baseline or impacted tests that fail on candidate code (`FAILS_ON_CANDIDATE`), intent-test failures, inconclusive fuzz results, incomplete mutation runs and configured stages that did not run. |
| 3 | Invalid arguments, flag combination, output format, Git comparison or trusted configuration, including an unusable `--cache-dir` and an intent that is not UTF-8 or contains NUL. No container starts. |
| 4 | Harness, analysis or report-writing operational error, including dependency preparation that failed or was not permitted, and a baseline-side fuzz harness that could not be built. |

## Configuration and trust

`probe init` generates `.probe.json`; see [the Go example](examples/probe.go.json). Policy comes from the **tip of the base branch** (`--base`, `main` by default), never implicitly from the candidate checkout. The diff still starts at the merge base, so a branch forked before the policy landed still gets it. The report records the policy commit, and Probe warns when no policy exists there and built-in defaults apply. `--config PATH` explicitly selects a local file you trust.

Commands are argv arrays, not shell strings. Configure only checks your project provides. `generated_test` accepts `{file}` and `{package}`; Go's default uses the test's package so it can exercise unexported code. Verified Go experiments require one standalone target placeholder; use `-tags=integration` for valued flags. Multi-package commands, execution wrappers and overlays cannot produce verified Go evidence. Avoid scripts that silently skip generated tests.

v0.4 adds three optional top-level policy objects, `fuzz`, `mutation` and `prepare`, which `init` never writes. Every binary before v0.4.0 rejects a policy that contains one of them with exit 3, so read the [release ordering](docs/CI.md#release-ordering-for-v04) before committing one to a base branch.

### Verified TypeScript/JavaScript experiments

A JavaScript or TypeScript `generated_test` command supports differential evidence when it runs the generated file as one standalone `{file}` argument and writes a Jest-compatible JSON report to `{results_out}` (exactly once, and only in this command). Vitest and Jest both produce this format:

```json
{
  "commands": {
    "generated_test": ["npx", "--no", "vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"]
  }
}
```

For Jest: `["npx", "--no", "jest", "{file}", "--json", "--outputFile={results_out}"]`. `probe init --language typescript` writes the Vitest form. The report travels back on the sandbox payload channel, apart from the command's log, so log text cannot impersonate it; code executing in the sandbox can still write it. It is normalized (redacted, bounded messages), stored in the check's `results` field and retained as a hashed `test_results` artifact.

A run is verified only when the report has exactly one entry for `/workspace/<generated path>` and each generated title appears once in it at top level: every title passed for a passing run, and at least one failed for a failing run. Generated files must declare uniquely titled `test("…", …)` or `it("…", …)` calls at column 0, with static titles (no escapes or `${}`) and no `describe` block. A missing or truncated report, skipped or nested tests, and failures unrelated to the generated titles are inconclusive. `npm`, `yarn`, `pnpm`, `bun`, `sh`, `bash` and `env` cannot start the template, since they run repository-defined scripts; call the runner binary (for example through `npx`). The image must provide the runner and the project's dependencies, for example installed under `/node_modules`, which Node resolves from `/workspace`. Existing policies without `{results_out}` keep working, with `UNVERIFIED` results.

Sandbox networking requires both `sandbox.network: true` and `--allow-network`. `--no-network` forces it off. This controls test containers; a configured reviewer separately makes provider HTTP requests from the CLI. Use `--reviewer=false` to disable those calls.

Trust and preferably digest-pin the preloaded image. Prepare dependencies in it outside review execution, or through the trusted policy's `prepare` command, and configure commands to use them. Stock Node/Python images do not contain project dependencies; their commands must be adapted accordingly.

### Changed-line execution (optional `coverage` command)

Add a `coverage` command to the trusted policy to measure which **added** Go lines a recorded sandbox run actually executed. Its argv must contain the token `{coverage_out}` exactly once:

```json
{
  "commands": {
    "coverage": ["go", "test", "-covermode=count", "-coverprofile={coverage_out}", "./..."]
  }
}
```

Probe expands `{coverage_out}` to the in-container profile path and never appends a coverage flag of its own, so the executed argv equals the argv you reviewed. That fragment is the Go default written by `probe init --language go`. Adding `-coverpkg=./...` is your choice and is what attributes execution across packages: without it, a line exercised only through another package's tests is reported as not executed. Profile entries are matched through the root `go.mod` and the `use` modules of a root `go.work`, so a Go module kept in a subdirectory (for example `app/`) is measured under its own module path; configure such a repository with workspace patterns such as `./app/...`.

The coverage command runs **after the other configured checks and in addition to** `test`, before the v0.4 evidence stages and the reviewer, so it roughly doubles sandbox time against `sandbox.max_runtime_seconds`; raise that budget before enabling it. An existing `.probe.json` does **not** acquire the key automatically: `init` refuses to overwrite an existing file, and policy decoding starts from an empty command map rather than merging the defaults. Add the key by hand, and read the release-ordering rule in [CI integration](docs/CI.md) first — an older pinned binary rejects the key with exit 3.

Each added Go line in a changed non-test file is reported in exactly one of four states: **executed**, **not executed**, **not inside any instrumented block**, or **not measured**. Absent, truncated, unparsable or unmapped profile data is always reported as *not measured*, never as not executed. Executed means the line ran at least once; it does not mean the line is tested, asserted, correct or safe.

#### TypeScript and JavaScript (LCOV)

A coverage command may instead write an LCOV report. Put the token `{coverage_dir}` in its argv, exactly once and in place of `{coverage_out}`: Probe expands it to an empty in-container directory and reads the `lcov.info` file the tool writes there. The format is recognized from the report itself, and `coverage.format` records it (`go` or `lcov`).

```json
{
  "commands": {
    "coverage": ["vitest", "run", "--coverage.enabled", "--coverage.reporter=lcovonly", "--coverage.reportsDirectory={coverage_dir}"]
  }
}
```

For Jest: `["npx", "--no", "--", "jest", "--coverage", "--coverageReporters=lcovonly", "--coverageDirectory={coverage_dir}"]`. Keep the `--` after `npx --no`: without an argument after `jest`, npx takes Jest's options as its own and Jest runs without coverage. Another tool that writes `lcov.info` into a directory it is given can be used the same way. Vitest needs the `@vitest/coverage-v8` (or `@vitest/coverage-istanbul`) package in the image; `probe init --language typescript` does not write a coverage command, since the stock Node image has none.

An LCOV report measures the added lines of changed TypeScript and JavaScript sources (`.ts`, `.tsx`, `.mts`, `.cts`, `.js`, `.jsx`, `.mjs`, `.cjs`), except declaration files, minified bundles, test files (`*.test.*`, `*.spec.*`, `__tests__/`) and `node_modules`; a Go profile measures only Go files, so one measurement never reports the other language's lines as not measured. Verdicts are line-granular: a line the report has no line entry for is *not inside any instrumented block*. Paths are matched as the report writes them, relative to the repository root (the default of Istanbul-based tools run from `/workspace`) or as `/workspace/<path>`. A report whose paths are relative to a subdirectory, for example a Vitest `--root web`, leaves every file *not measured*: run the command from the repository root. A file absent from the report is *not measured*; with Vitest's default `coverage.include`, that is every source no test loaded. A whole-repository report must fit in `sandbox.max_output_bytes`; narrow `coverage.include` otherwise. A binary before this release rejects `{coverage_dir}` with exit 3, so read the [release ordering](docs/CI.md#release-ordering-for-v04) rule first.

## Optional AI investigation

`probe review` automatically uses the LLM when `reviewer.model` is nonempty in trusted policy or in the deployment environment (see [provider settings from the deployment](#provider-settings-from-the-deployment)). No model anywhere (the default) keeps review independent of any provider. `probe lint` always stays offline with respect to the reviewer, even when a model is configured.

Set the following fields in `.probe.json`, using the model identifier and endpoint supplied by your provider. This is a fragment to merge into the configuration generated by `probe init`:

```json
{
  "reviewer": {
    "endpoint": "https://your-provider.example/v1",
    "model": "your-tool-capable-model",
    "api_key_env": "PROBE_API_KEY",
    "max_iterations": 20,
    "max_generated_tests": 10
  }
}
```

Commit the policy to the trusted base branch, or use `--config .probe.json` to explicitly select your local policy. Candidate PR changes cannot activate or redirect the reviewer. The provider must support Chat Completions function calling. Both a `/v1` base URL and a full `/chat/completions` URL are accepted. Remote endpoints require HTTPS by default; local servers may use HTTP on loopback (for example `http://127.0.0.1:1234/v1`). For an internal HTTP server, see the explicit deployment exception below. Local providers may work without an API key. Redirects are refused.

```sh
# Set PROBE_API_KEY using your shell or CI secret store.
probe review --base main --max-iterations 20
# Override automatic activation for this run:
probe review --base main --reviewer=false
# Try a local configuration before committing it:
probe review --base main --config .probe.json
```

Configuring a model enables transmission of bounded, redacted source context during `review`. Common secret patterns and sensitive filenames are masked, but masking is best effort. Use static analysis or a local provider if source must stay local. API credentials come from the named environment variable or its Docker secret and never enter test containers. `--reviewer` remains supported as an explicit request and fails if no model is configured. `--checks=false` skips initial checks but still lets the configured reviewer request sandbox experiments; combine it with `--reviewer=false` for static analysis only, or use `lint`. It also turns off the v0.4 deterministic stages: configured differential fuzzing is recorded as `disabled`, configured mutation as `not_run`, and `--base-tests` or `--impacted-tests` exit 3.

Model claims are checked against harness evidence before entering reproduced issues. Tools cover file reads, diffs, source search, reference, symbol and caller lookup (a static Go index when available, labelled approximate, with a lexical fallback), existing checks, generated-test creation/execution/deletion and, when the intent contains acceptance criteria, candidate-only intent tests. Iteration, input, response, test-count, output and runtime budgets bound investigations. Provider failures and exhausted budgets leave deterministic results in the report and mark investigation incomplete; `--ci` requests human review. Invalid active provider settings fail before investigation. Empty changes do not call the provider.

The LLM can investigate business rules and interactions beyond static patterns and propose concrete counterexamples. Its findings remain hypotheses until supported by evidence. Better review quality or time savings must be measured on representative PRs; adding a model alone does not establish either.

### Read-only AI review

`probe review --read-only --base main --ci` analyzes the actual diff and
relevant source with the deployment's model, without Docker or repository code
execution. It skips preparation, checks, coverage execution, fuzzing and mutation.
The model can only read files, get the diff, search code and look up references,
symbols and callers. Write and execution calls are rejected by the dispatcher.
Execution flags cannot be combined with `--read-only`; `--deadline` remains
available. This is stronger than `--checks=false`, which still allows experiments
in ordinary review.

Set `PROBE_REVIEWER_MODEL` and `PROBE_REVIEWER_ENDPOINT` (the endpoint
defaults to OpenAI for standalone CLI use). In this mode all policy `reviewer`
settings are ignored: provider settings come from the deployment, credentials
use `PROBE_API_KEY` / `_FILE` / `/run/secrets/PROBE_API_KEY`, and reviewer
budgets use built-in defaults (`--max-iterations` can override the iteration cap).
Source is sent to that provider with the existing redaction and input limits.

The JSON report records `analysis_mode: "review-read-only"`, and Markdown states
that no code or tests ran. Suspicions are `UNVERIFIED`; only source-backed
`DISMISSED` claims can be accepted as dismissals. No reproduced issue can be
established by this mode. Existing CI rules still apply: an unresolved hypothesis
requests human review (exit 2 with `--ci`), and exit 0 is no correctness guarantee.

In both review modes, when the linter raised signals, the reviewer also reads
them with the `assess_signals` tool: for each one, a plain-language title, a
short explanation and a judgment (`risk`, `no_risk` or `uncertain`). The title
states the intent of the modification (for example "Admin tokens now skip the
expiry check" instead of "Lines added to a sensitive file"); a title that only
restates the linter summary is rejected, and a model that finishes with
signals left unassessed is reminded of them, at most twice. They are
recorded in `signal_assessments`, and the model's closing text in
`reviewer_summary`; Markdown shows both. They are model judgment, never
evidence, and change no hypothesis status. A `no_risk` reading is kept only
when a non-blank rationale cites a verified `read_file` source observation;
otherwise it is recorded as `uncertain`. With `--ai-impacts-criticality` (the
default), a kept `no_risk` reading lowers its signal's severity by one level
(critical to high, high to medium, medium to low; the linter's value stays in
`original_severity`), and a low signal is marked `set_aside` and becomes no
review target. Review targets, the review surface and the exit code are then
derived from the adjusted severities, so a high signal read as harmless no
longer requests human review on its own. `--ai-impacts-criticality=false` keeps
the linter's severities, and the report records the choice in
`ai_impacts_criticality`. When
the diff would take more than half of the reviewer's input budget, the model
receives the list of changed files without their hunks and reads them with
`get_diff`.

### Team coding rules

`--rules TEXT` or `--rules-file FILE` (UTF-8, at most 32 KiB) gives the
reviewer your team's coding rules, in both review modes:

```sh
probe review --base origin/main --rules-file docs/CODING_RULES.md --ci
```

The reviewer checks the changed code, and only the changed code, against them.
It submits each violation it finds as a hypothesis whose title names the rule,
`UNVERIFIED` unless evidence supports another status. The rules are review
criteria, not instructions: they cannot change the tools, the statuses or the
evidence requirements. They are recorded in `coding_rules` and
`coding_rules_sha256`, and Markdown quotes them. They come from the command
line, never from the reviewed change. A run without a reviewer (`lint`,
`--reviewer=false`) does not record them, because nothing checked them.
Probe Hub keeps the rules of each repository and passes them to every review.

### Learning from team feedback

`--feedback-file FILE` (JSON, at most 64 KiB) gives the reviewer what the team
thought of earlier findings, so that reviews gradually fit the team. Probe
Hub builds this file from the votes, comments and replies on findings and
from what developers did after them, and passes it by default whenever the
AI reviewer runs:

```json
{
  "topics": [
    {"topic": "signal:no_test_change", "useful": 1, "not_useful": 7, "changed": 2, "unchanged": 9}
  ],
  "comments": [
    {"topic": "issue", "path": "pay/refund.go", "title": "Refund guard removed", "vote": "down",
     "comment": "The guard moved to the gateway.", "reply_to": ""}
  ]
}
```

A topic is `signal:<linter kind>`, `issue` (reviewer hypotheses) or `check`.
`useful` and `not_useful` count votes. `changed` and `unchanged` count
whether the next analyzed commit changed the file a finding was about, which
is a heuristic and not proof of a fix. With at least three reactions and a
two-to-one majority, the prompt states the trend: for example, that the team
usually finds a topic not useful, or usually leaves the code unchanged after
it. The reviewer then spends less effort on those topics, investigates the
ones the team values more deeply, and follows the preferences the comments
express. The feedback is guidance for the model, never evidence. It changes
no status, severity or exit code by itself, it never justifies dismissing a
concrete defect, and a `no_risk` reading still needs its source observation.
The report records it in `team_feedback` and `team_feedback_sha256` when a
reviewer ran.

### Cross-repository context and repository clusters

A change often depends on contracts that live in another repository: shared
types, an SDK, an API client, a sibling microservice. The trusted policy can
name those repositories so that the reviewer reads them while reviewing a
change, and reports an incompatibility, for example a changed endpoint that
the SDK still calls the old way:

```json
{
  "context": {
    "repos": [
      "company/shared-types",
      {"name": "company/payment-sdk", "ref": "main", "paths": ["src/**"], "role": "payment SDK",
       "url": "https://github.com/company/payment-sdk.git"}
    ],
    "clusters": ["payments"],
    "cluster_definitions": {
      "payments": {"description": "Payment service, its SDK and shared types",
                   "repos": ["company/payment-service", "company/payment-sdk", "company/shared-types"]}
    }
  }
}
```

- **Repositories.** Each entry is a name (`owner/repo`) or an object: `ref`
  (branch, tag or commit; `HEAD` by default), `paths` (globs that restrict what
  is read), `role` (what it is, for the reviewer) and `url`.
- **Clusters.** A cluster is a named group of related repositories: a service
  and its clients, a backend and its SDK, the members of a distributed system.
  `clusters` lists the clusters this repository belongs to; every other member
  becomes context, and the repository itself is left out (Probe recognizes it
  from its `origin` remote). Clusters are defined in `cluster_definitions`, or
  once for a whole organization in an operator file given with
  `--clusters FILE` (`{"clusters": {"payments": {...}}}`). A cluster defined
  in both places is refused.
- **Where the code comes from.** Probe reads a local checkout, given with
  `--context-repo NAME=PATH` (repeatable) or found under `--context-dir DIR`
  as `DIR/owner/repo` or `DIR/repo`. It never uses the working tree: it reads
  the committed files at `ref`, from Git objects. With `--fetch-context`, a
  repository without a checkout is shallow-fetched from its `url` (https or
  ssh, no credentials in the URL; Git uses your credential helper). A
  repository that cannot be found is recorded as unavailable with the reason;
  it never fails the review.
- **What the reviewer can do.** With read-only tools it lists, reads and
  searches the context repositories. Sensitive files (`.env`, keys,
  credentials) and files over 1 MiB are never listed or read, and nothing is
  executed. It reports an incompatibility as an `UNVERIFIED` hypothesis
  anchored to the changed line in the reviewed repository. The report lists
  the repositories, commits and statuses in `context_repos`, and every read in
  the audit.
- **Checking the setup.** `probe context check [--context-dir DIR]
  [--context-repo NAME=PATH] [--clusters FILE]` shows how each repository
  resolves and exits 1 while one is unavailable. `--context=false` disables
  the context for one review.

Like `fuzz`, `mutation` and `prepare`, `context` is an optional,
release-ordered policy key: `probe init` never writes it, and an older binary
refuses a policy that contains it. In CI, check out the context repositories
next to the reviewed one and pass `--context-dir`:

```yaml
- uses: actions/checkout@v4
  with: {repository: company/payment-sdk, path: context/company/payment-sdk}
- run: probe review --base "origin/$GITHUB_BASE_REF" --context-dir context --ci
```

### Codebase knowledge base

When a reviewer runs, Probe keeps a persistent, editable knowledge base of
the repository in `PROBE_KNOWLEDGE.md` (`--knowledge PATH` to move it,
`--knowledge none` to disable it). It holds what the team and earlier reviews
learned about the codebase: how parts work (`component`), how they relate
(`relationship`), known risks (`risk`), architectural context
(`architecture`), project conventions (`convention`), review knowledge
(`review`) and anything else (`note`). It is plain Markdown, one entry per
`##` heading:

```markdown
## Refunds are validated by the gateway
- kind: architecture
- paths: pay/**, gateway/refund.go
- updated: 2026-09-30 (review of 1a2b3c4d)

The refund handler trusts the amount because the gateway checks it first.
```

- **Reading.** A review reads the file at the tip of the base branch, like the
  policy, so a change never supplies the knowledge its own review receives. The
  reviewer gets the entries whose `paths` globs match a changed file, then the
  entries without paths, within 24 KiB. It is told to check them against the
  source: they are context, never evidence or instructions.
- **Evolving.** While investigating, the reviewer can propose additions,
  corrections (same title) and removals (`obsolete`) with its
  `record_knowledge` tool, at most 20 per run. Probe records them in the report
  (`knowledge`) and writes `.probe/knowledge-updates.json` with a merged preview
  in `.probe/KNOWLEDGE.md`. Probe never commits.
- **Building.** `probe knowledge build [--base main] [--focus TEXT]` asks the
  provider to explore the base commit with read-only tools (nothing is written
  or executed) and propose entries, for example to bootstrap the file.
- **Applying.** `probe knowledge apply` merges the proposed updates into the
  working-tree file, keeping your manual edits. Review the diff, edit it, and
  commit it.
- **Editing.** Edit the file by hand at any time. `probe knowledge check`
  validates it (unique titles, known kinds, no stray `##` headings outside code
  fences, size limits) and exits 1 on a problem.

### Provider settings from the deployment

The provider belongs to the deployment rather than to the reviewed repository, so the same binary and the same committed policy can be pointed at an operator's endpoint without a policy change:

| Setting | Source | Notes |
|---------|--------|-------|
| `reviewer.endpoint` | `PROBE_REVIEWER_ENDPOINT` | Overrides the policy value; the same URL rules apply. |
| `reviewer.model` | `PROBE_REVIEWER_MODEL` | Overrides the policy value and enables `review` on its own. |
| HTTP exception (deployment only) | `PROBE_REVIEWER_ALLOW_INSECURE_HTTP=false` | Set `true` for an explicitly configured HTTP endpoint on a trusted network. Requires `PROBE_REVIEWER_ENDPOINT`; source and API key travel unencrypted. Applies to review and plan. |
| API key | `PROBE_API_KEY`, else `PROBE_API_KEY_FILE`, else `/run/secrets/PROBE_API_KEY` | The variable name is `reviewer.api_key_env`; `<NAME>_FILE` and `/run/secrets/<NAME>` follow it. |

A blank variable counts as unset and leaves the policy value in place. The key file is read whole, with surrounding whitespace stripped; a file named by `<NAME>_FILE` must be readable, and any mounted key file that cannot be used fails the run with exit 3 instead of silently sending an unauthenticated request. Provider settings and the HTTP exception come from the environment; execution settings stay decisions of the trusted policy. Read-only review uses built-in reviewer budgets. When the reviewer runs, the run log names each value's source — the variable or file name, never the credential.

In a Docker Swarm deployment the key is a [Docker secret](https://docs.docker.com/engine/swarm/secrets/), mounted as a file and never present in `docker service inspect`:

```yaml
services:
  probe:
    image: registry.example/probe:v0.3.0
    environment:
      - PROBE_REVIEWER_ENDPOINT=https://provider.internal/v1
      - PROBE_REVIEWER_MODEL=your-tool-capable-model
    secrets:
      - source: probe_PROBE_API_KEY
        target: PROBE_API_KEY

secrets:
  probe_PROBE_API_KEY:
    external: true
```

On the PulsarCD cluster this is automatic: a variable whose name ends in `_KEY` is converted into the Docker secret `<stack>_<NAME>` at deployment, removed from the `environment:` block and mounted at `/run/secrets/<NAME>`, so declaring `PROBE_API_KEY=${PROBE_API_KEY}` in the compose file and putting the value in `devops/.env` is enough. Keep the name aligned with `reviewer.api_key_env` if you change it, and never commit the value.

## Evidence stages beyond the initial checks

v0.4 adds stages that record more deterministic evidence and depend less on a model. Each one is opt-in or bounded, only adds evidence or review requests, and never produces exit 1: only a reproduced high/critical hypothesis does. During `review` they run in this order: dependency preparation, initial checks, coverage, changed baseline tests, impacted tests, differential fuzzing, mutation of added lines, then the reviewer, whose experiments may record observations or run intent tests. `lint` executes none of them; it builds only the static impact index. Every sandbox run shares the one `sandbox.max_runtime_seconds` budget; dependency preparation has its own timeout instead.

A stage's JSON object is present exactly when the stage was requested or configured, and its `status` then says what happened, including `not_run` with a reason. The [v0.4 specification](../specs/probe-v0.4-spec.md) holds the binding rules.

<!-- F8:begin -->
### Trusted dependency preparation

With a `prepare` object in the trusted base-branch policy, `review` first runs that policy's command once, in one bounded container, on the declared input files exported from the base commit (never from the candidate), and commits the container as a local image that every sandbox run of the review then uses by ID:

```json
"prepare": { "command": ["go", "mod", "download"], "inputs": ["go.mod", "go.sum"], "network": true }
```

The container gets the network only when the policy asks for it and `--allow-prepare-network` is passed (`--no-network` always wins); that permission never reaches checks, which keep their unchanged network setting (offline unless `sandbox.network` and `--allow-network` allow it). A later review of the same base commit with identical inputs reuses the image without starting a container. Preparation never pulls images and never installs candidate dependencies: a candidate edit to a declared input adds a `prepare_input_changed` signal and an Unverified entry. It fails closed: `failed` or `not_permitted` runs no check and exits 4. The prepared image is environment, not evidence: an equal key means equal inputs, not equal image content, and nothing is claimed about the dependencies. See [dependency preparation](docs/PREPARE.md).
<!-- F8:end -->

<!-- F7:begin -->
### Execution cache

Opt-in with `--cache-dir DIR`, review only. A baseline-side run (a check kind ending in `_base`, on the baseline snapshot, without network) is replayed instead of executed when two earlier live runs of byte-identical inputs agreed on its result; candidate-side runs always execute. The key covers the baseline tree and any staged test, the command and the complete sandbox arguments, the image ID the run executes, the Docker server, the trusted policy's execution settings and the Probe build. A replay never supports a reproduced issue, a divergence or a `FAILS_ON_CANDIDATE` result: before recording `REPRODUCED` on a replayed baseline, the harness runs that baseline again, live. A negative conclusion that rests on a replay is listed in `execution.replay_backed`. Only baseline PASS results are replayed. The directory must be outside the repository and the report directory and, on Unix, owner-only; otherwise the review exits 3 before any container starts. Entries are integrity-checked, not authenticated. Reviews of an updated pull request that share the directory replay the baseline runs whose key did not change, such as those of changed baseline tests, impacted tests and identical generated tests; a differential fuzzing harness changes on every review, so its baseline is not replayed in practice. See [Execution cache](docs/EXECUTION_CACHE.md).

### Parallel initial checks

`--parallel N` (review only, 1 to 4) runs up to N of the initial checks (test, typecheck, build) at the same time; everything else still runs one at a time. The limit is also capped by the Docker server's room for sandboxes of `sandbox.cpus` CPUs and `sandbox.memory_mb` MiB (one `docker info` call), and a group of checks starts together only while the remaining runtime budget covers each one's full per-run timeout. The rules for each check's timeout, classification and budget charge are those of `--parallel 1`, and the report records the checks in configured order; `execution.parallelism` gives the requested and effective values and why they differ. The outcomes can still differ: concurrent sandboxes share the Docker host, so a check can take longer than it would alone, reach its timeout and be charged more, which leaves less of `sandbox.max_runtime_seconds` for later stages; and with `--deadline`, checks that run together can all end `TIMEOUT` at the deadline, where one at a time the later ones would be `SKIPPED` as not started. See the [specification](../specs/probe-v0.4-spec.md#f7b-parallel-initial-checks), the [CI guidance](docs/CI.md#parallel-initial-checks-in-ci) and the [measured costs](docs/PERFORMANCE.md#parallel-initial-checks).
<!-- F7:end -->

<!-- F3:begin -->
### Changed baseline tests on candidate code

`probe review --base-tests` runs the baseline version of the Go test functions of each changed Go test file that the change modified, removed or affected through the rest of the file (other declarations, imports, build constraints, a move to another directory) on two trees: the baseline, and a hybrid tree, which is the candidate with that test's package test files and `testdata` reverted to the baseline. Each test gets one `base_test_differential` evidence record, re-derived by `Finalize`:

- `FAILS_ON_CANDIDATE`: it passed on the baseline and failed on the hybrid tree, in one recorded run each. This is possibly a behavior change accompanied by a test edit, possibly flakiness, for a human to judge; it is not a reproduced issue.
- `PASSES_ON_CANDIDATE`: it passed in both runs. This does not show that behavior is preserved or that the edited test is equivalent.
- `UNVERIFIED`: no result was drawn, for example because the baseline test does not compile against the candidate code after an API change.

Selection is static (Go syntax; comment and layout edits select nothing unless they change a build constraint or a compiler directive). The `generated_test` template must be a verifiable Go template such as `["go", "test", "{package}"]`, and the stage has a 180 s sub-cap inside the shared runtime budget. It never produces exit 1: `FAILS_ON_CANDIDATE`, `UNVERIFIED` and a stage that did not run request review (exit 2 with `--ci`). In `lint` and `review`, lexical signals flag risky test edits in Go, JavaScript/TypeScript and Python (`test_assertion_removed`, `test_case_removed`, `test_skip_added`, `test_expectation_relaxed`, and the high `test_focus_added`); they are heuristics, never evidence. With a Vitest or Jest template, the `test()` and `it()` calls of changed TypeScript and JavaScript test files are selected the same way, and their hybrid tree reverts only the test file and its snapshot file ([TypeScript and JavaScript tests](docs/BASE_TESTS.md#typescript-and-javascript-tests)). See [changed baseline tests](docs/BASE_TESTS.md).
<!-- F3:end -->

<!-- F6:begin -->
### Impact analysis

`lint` and `review` build a static index of the repository's own Go packages from committed Git objects, on the host, without running repository code or loading imports from outside the repository; `--impact=false` disables it. For each changed Go function or method, the report lists its callers in unchanged, non-test code (resolution `static`, or `interface` for possible dispatch) and the existing Go tests that reach it within 3 references. Callers become low `impacted_caller` review targets, at most 10 per function and 100 per run, and one medium `analysis_limited` signal states what was left out or why the index is limited. The reviewer's `find_references`, `inspect_symbol` and `find_callers` answer from the index and fall back to lexical search. Everything here is approximate: an absent caller is not proof that none exists, and a reaching test is not evidence that it asserts the changed behavior. See [impact analysis](docs/IMPACT.md).

With `review --impacted-tests`, the listed reaching tests whose file the change did not modify (at most 16, from at most 4 packages or test files) run on the baseline and on the candidate with the verifiable `generated_test` template, inside a 180 s sub-cap: Go tests with a `go test {package}` template, TypeScript and JavaScript tests with a Vitest or Jest template, whose JSON reports then record each test's result. Each test gets one `impacted_test_differential` evidence record: `FAILS_ON_CANDIDATE` (it passed on the baseline and failed on the candidate, one recorded run each), `PASSES_ON_CANDIDATE` or `UNVERIFIED`. A failure is an outcome difference for a human to judge, not a reproduced issue: the static link is approximate, and the failure may come from any part of the change or from flakiness. `FAILS_ON_CANDIDATE`, `UNVERIFIED`, a selected test without a result (including tests over the limits) and a stage that did not run request review (exit 2 with `--ci`), never exit 1, and add no signal kind. See [impacted tests](docs/IMPACT.md#impacted-tests---impacted-tests).
<!-- F6:end -->

<!-- F2:begin -->
### Differential fuzzing

With a `fuzz` object in the trusted base-branch policy (`"fuzz": {}` takes the defaults), `review` runs the same seeded inputs through each changed function whose signature is unchanged and whose parameters can be generated, on the baseline and on the candidate, in the unchanged sandbox and through the reviewed `generated_test` command. With a Go template (`["go", "test", "{package}"]`) these are package-level Go functions taking basic types, package-local named basic types, and slices, arrays or variadics of them. With a verifiable Vitest or Jest template (`["vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"]`) they are exported TypeScript and JavaScript functions taking `number`, `string`, `boolean` or arrays of them, read lexically from TypeScript annotations or JSDoc `@param` types, without a type checker. No model is involved and nothing asserts an expected value. A one-file harness per Go package or TS/JS module records bounded encodings of results, panics or thrown and rejected values, and slice or array arguments after the call; a first-pair difference gets one confirmation run per revision. Each function is:

- `diverged`: the revisions recorded different values for at least one input, each repeating its own value in a second run. The smallest divergent input tried is shown with both values; it does not say which revision is correct, and the change may be intended.
- `not_diverged`: equal recorded encodings for every compared input. This does not establish equivalent behavior, even for those inputs.
- `inconclusive`: a timeout, a crash, nondeterminism, a candidate that does not build, a budget cut, and so on.

Other changed functions are listed as not fuzzed, with a reason (a TS/JS function without a Vitest or Jest template, or a Go function with one, among them). `Finalize` derives every outcome again from the recorded checks and observation streams. Fuzzing never produces exit 1: a divergence, an inconclusive function or a stage that did not run requests review (exit 2 with `--ci`); only a baseline-side harness failure or an infrastructure failure of a fuzz run is exit 4 (a cut log never is). `--fuzz=false` disables it for one run; `fuzz.max_runtime_seconds` is a sub-cap inside the shared sandbox budget. See [differential fuzzing](docs/FUZZ.md).
<!-- F2:end -->

<!-- F4:begin -->
### Mutation of added lines

With a `mutation` object in the trusted base-branch policy, `review` makes deterministic single-change mutants of the added lines of changed non-test Go files (for example `<` to `<=`, `if c` to `if !(c)`, a returned error to `nil`) and runs the policy's `go test -json` command for the file's package once per mutant, after one passing unmutated control run, in a private copy of the candidate inside the unchanged sandbox:

```json
"mutation": { "command": ["go", "test", "-json", "-count=1", "-failfast", "{package}"], "max_mutants": 20, "timeout_seconds": 60, "max_runtime_seconds": 300 }
```

A mutant with which no test that the command ran for its package failed (`SURVIVED`) becomes a medium `surviving_mutant` signal with its patch retained; it may be semantically equivalent and is not a defect. Killed mutants are counted, never listed. Mutation creates no evidence, never produces exit 1 and computes no score; an `incomplete` or `not_run` section requests review under `--ci`. With a Vitest or Jest command (`{file}` and `{results_out}` instead of `{package}`), changed TypeScript and JavaScript sources are mutated instead, lexically, with one control run per source file and outcomes read from the JSON reports ([TypeScript and JavaScript](docs/MUTATION.md#typescript-and-javascript)). See [mutation of added lines](docs/MUTATION.md).
<!-- F4:end -->

<!-- F1:begin -->
### Observation experiments

A generated test may record values instead of asserting a guessed one: Go tests call `t.Attr("probe.<key>", value)` (Go 1.25 or later in the sandbox image) and Vitest tests set `task.meta.probe`; Jest reports carry no per-test metadata. When the named test passes on both revisions, `run_generated_test` compares the recorded values key by key and records a `differential_observation` evidence record. A key whose candidate value differs triggers exactly one live baseline repeat. The record is `DIVERGED` when two baseline runs agreed and the candidate recorded a different value, `NOT_DIVERGED` when every compared value was equal, and `UNVERIFIED` otherwise (unstable, redacted, duplicated or one-sided values, a failing run). Every validated divergence is listed in `divergences` and in the Behavior Divergences section with both values, cited or not; a human decides which value is intended. A divergence requests review (exit 2 with `--ci`) and never produces exit 1; `NOT_DIVERGED` supports no hypothesis status, and a both-pass generated test whose recorded values differ, or cannot be shown equal (redacted, unconverted or unreadable), no longer supports `NOT_REPRODUCED`. There is no policy key and no flag. See [observation experiments](docs/OBSERVATIONS.md).
<!-- F1:end -->

<!-- F5:begin -->
### Intent criteria and candidate-only intent tests

The Markdown list items of `--intent` / `--intent-file` (only those under an "Acceptance criteria" heading when there is one) become criteria `AC-1`, `AC-2`, … with the SHA-256 of the recorded (redacted) intent; the intent must be UTF-8 without NUL (else exit 3), and a pasted Probe PR comment is removed from it. With criteria, the reviewer may write a test for one criterion with `create_intent_test` and run it with `run_intent_test`, on the candidate only: there is no baseline control, and intent tests use at most half of the generated-test budget. The `intent_test` record is `INTENT_TEST_FAILED` only when the named test failed on an assertion of its own file and names a changed declaration (matched by name) whose name is on an added line; `INTENT_TEST_PASSED` says nothing about whether the criterion holds. An accepted `INTENT_TEST_FAILED` hypothesis is listed in `intent_test_failures` and the Intent Test Failures section, apart from reproduced issues, and requests review (exit 2 with `--ci`, never 1). `intent_judgment` is model judgment, kept only on `DIVERGED` hypotheses and never read by any status or exit code. See [intent criteria](docs/INTENT.md). `--jira`, `--linear` and `--notion` supply the intent from a Jira or Linear issue or Notion pages ([Jira issues](docs/JIRA.md), [Linear issues](docs/LINEAR.md), [Notion pages](docs/NOTION.md)).
<!-- F5:end -->

<!-- F9:begin -->
### Evidence-only exports (SARIF and PR comment)

`--format sarif,pr-comment` (lint, review and report) also writes `confidence-report.sarif` (SARIF 2.1.0) and `PR_COMMENT.md`. Both list only findings backed by recorded sandbox evidence, re-derived from the recorded checks when they are rendered: reproduced hypotheses, changed baseline tests and impacted tests that fail on candidate code, fuzz and observed divergences, intent-test failures and surviving mutants, each with a fixed rule ID and level (`error` only for a reproduced high/critical hypothesis, the only finding that can set exit 1).

- Never findings: signals, review ranges, coverage, unverified, not-reproduced or dismissed hypotheses, passing or negative results, killed mutants and model judgments. Unverified areas, checks that did not pass and stages that did not run appear as status (SARIF notifications, the comment's status block), and "No finding is not approval."
- Locations are emitted only in changed, non-deleted files; a model-chosen line outside the recorded diff becomes file-level, and a finding without such a location is listed only in the comment.
- The comment sits between `<!-- probe:pr-comment:begin v1 -->` and `<!-- probe:pr-comment:end -->`, escapes every untrusted string, is at most 60 000 bytes, and links only the validated `--report-url`. Post it as a comment, never into the PR description.
- Rendering never changes the exit code. Probe publishes nothing; upload SARIF only when the exit code is 0, 1 or 2, `executionSuccessful` is true and no `no_execution`, `stage_not_run` or `omitted_findings` notification is present, and treat files from fork runs as forgeable. See [exports](docs/EXPORTS.md) and [CI integration](docs/CI.md#publishing-evidence-backed-findings-sarif-and-pr-comment).
<!-- F9:end -->

## Boundaries and development

For the coding-to-deployment workflow, see the [agent loop](docs/AGENT_WORKFLOW.md),
[reusable PR workflow](docs/CI.md) and [PulsarCD integration](docs/PULSARCD.md).

Probe never exits 1 except for a reproduced high/critical hypothesis. Divergences, baseline versions of changed tests failing on candidate code, and intent-test failures request human review; surviving mutants are reported for review. No equivalence, completeness or mutation-score claims. Impacted tests that fail on candidate code, inconclusive fuzz results and configured stages that did not run also request human review (exit 2 with `--ci`).

Containers run non-root, without network by default, with read-only source/root mounts, no added capabilities and CPU/RAM/PID/time limits. The Docker socket, working checkout and API keys are never mounted. The optional `prepare` container is the one documented exception: it has a writable root filesystem, may run as root when the trusted policy says so, and has network only when the policy and `--allow-prepare-network` both allow it ([dependency preparation](docs/PREPARE.md)). See [security boundaries](docs/SECURITY.md).

Generated tests cannot overwrite source. Every executed run uses a fresh environment; with `--cache-dir`, a baseline run of byte-identical inputs may instead be replayed from recorded live runs, and the report marks each replay. Go experiments select the generated test names and verify their actual execution from structured test events; TypeScript/JavaScript experiments verify them from the Jest-compatible JSON report written to `{results_out}`. Other frameworks can execute experiments but remain `UNVERIFIED` until equivalent execution validation exists. Reproductions retain test source and hashed artifacts. Reviewers still judge whether a test's assertion reflects intended behavior.

Execution snapshots currently reject symlinks/submodules. Large inputs fail explicitly or emit analysis-limit signals. There is no dependency installation from candidate content, semantic TypeScript engine, complete call graph, coverage proof, coverage threshold gate, mutation score, formal verification, automatic merge or PR comment publishing: SARIF and PR-comment exports are files that a separate job may publish. Changed-line execution is measured for Go only when a coverage command is present in the trusted policy; repositories whose `.probe.json` predates this release measure nothing until that policy is updated by hand. An executed line is an observation, not proof that it is tested.

```sh
go test ./...
go vet ./...
go test -race ./...                 # supported native C toolchain required
go test ./internal/linter -bench . -benchmem
```

Tests use real temporary Git repositories, CLI/report integration, simulated providers, evidence validation and sandbox-policy checks. For real Docker integration, preload an appropriate Go image and set `PROBE_TEST_DOCKER_IMAGE` to its name before running `go test ./... -run Docker`; the TypeScript/JavaScript scenarios (Vitest and Jest) run only when `PROBE_TEST_TS_IMAGE` names a preloaded image with Node, Vitest and Jest, and skip otherwise.

See [CI integration](docs/CI.md), [validation results](docs/VALIDATION.md), [performance measurements](docs/PERFORMANCE.md), the [report schema](schema/confidence-report.schema.json), the [current CLI specification](../specs/probe-v0.4-spec.md) and the [v0.4.0 release notes](docs/releases/v0.4.0.md) (unreleased). Contributions should include reproducible counterexamples for new rules. Licensed under the AGPL-3.0 with an attribution term, see [LICENSE](../LICENSE) and [NOTICE](../NOTICE).
