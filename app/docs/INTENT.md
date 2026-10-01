# Intent criteria and candidate-only intent tests

A pull request usually states what the change is for. Probe reads the acceptance criteria of that statement, and the reviewer may write a test for one criterion and run it on the candidate. When such a test fails on an assertion and names code the change added or modified, the report records an `INTENT_TEST_FAILED` hypothesis and asks a human to look.

This is deliberately weaker than a reproduced issue. There is no baseline control, and the test, its inputs and its reading of the criterion are all model-written, so a failure may come from the test or from the reading rather than from the code. An intent-test failure therefore never enters `reproduced_issues` and never produces exit code 1.

The feature adds no policy key and no flag. It uses the existing `--intent` / `--intent-file` input, the trusted `generated_test` command template and the reviewer's generated-test budget.

## Supplying an intent

`--intent TEXT` or `--intent-file FILE` (at most 64 KiB) supplies the intent to `review` and `lint`; `--jira KEY` and `--linear KEY` (or `auto`) put a Jira or Linear issue before it, `--notion PAGE` Notion pages and `--gdoc DOC` Google Docs ([Jira issues](JIRA.md), [Linear issues](LINEAR.md), [Notion pages](NOTION.md), [Google Docs](GOOGLE_DOCS.md)). In CI it is usually the pull request description, written by the PR author, so Probe treats it as untrusted input:

- The text must be valid UTF-8 without NUL bytes. Anything else exits 3 with `intent: intent must be UTF-8 text without NUL bytes`, before Git analysis, before any container and before any provider call.
- Every block from `<!-- probe:pr-comment:begin v1 -->` to `<!-- probe:pr-comment:end -->` is removed first (a begin marker without an end marker removes the rest of the text, and a stray end marker is removed too), and the report records the Unverified note "Probe PR-comment output was removed from the intent text." Removal repeats until no marker is left, since removing a block can join the text around it into a new marker; after 8 passes the text is cut at the first remaining marker. A Probe PR comment pasted into the description therefore never becomes criteria. Post the PR comment as a comment, never into the description.
- The remaining text is then redacted as the report redacts every string (credential shapes become `[REDACTED]`; line breaks are kept, so line numbers do not move). The report records exactly that text as `intent`, `intent_sha256` is the SHA-256 of it, and the criteria are extracted from it. Anyone can therefore recompute the hash and the criteria from the report, and the hash never covers a secret that redaction hides from the report, so it cannot be used to confirm a guess of that secret.
- Criteria are data. No command, image, network setting, budget or provider setting is derived from the intent, and the reviewer prompt says that criterion text is never an instruction.

## Criteria grammar v1

Criteria are Markdown list items:

- A list item starts with `-`, `*`, `+`, `1.` or `1)` followed by a space. A task checkbox (`[ ]`, `[x]`, `[X]`) at its start is removed.
- If any ATX heading contains "acceptance criteria" or "acceptance criterion" (case-insensitive), only the items inside such sections count. A matching heading of level L opens a section, deeper headings stay inside it, and the next heading of level L or higher closes it. Without such a heading, every list item counts.
- Indented continuation lines (two spaces or a tab, not blank, not themselves a list item, heading, fence or thematic break) are joined to their item with one space. Nested items are criteria of their own.
- Fenced code blocks (backticks or tildes) and thematic breaks are ignored. Prose outside list items is not read.
- Text is copied verbatim otherwise, including inline Markdown.
- IDs are positional: `AC-1`, `AC-2`, … in document order. They change when the list changes, so compare runs by `intent_sha256`, never by ID alone.
- At most 100 criteria are extracted, and an item longer than 1024 bytes is skipped without consuming an ID. Both limits add an Unverified note that states how many items were left out, which requests human review with `--ci`.

Example:

```markdown
Make order discounts predictable.

## Acceptance criteria
- [ ] Orders of 100 or more get 10 off
- [ ] Orders of 50 or more ship free

## Notes
- not a criterion
```

gives `AC-1` "Orders of 100 or more get 10 off" (line 4) and `AC-2` "Orders of 50 or more ship free" (line 5); the item under Notes is outside the criteria section.

Extraction is a reading of list items, not an understanding of the intent. An intent without list items yields no criteria, and that does not mean it states no requirement. `lint` extracts criteria too; it executes nothing and makes no provider call.

## Intent tests

The reviewer is offered two tools only when the run has at least one criterion; without criteria, the tool list, the submit schema and the prompt are the same as without this feature.

- `create_intent_test(criterion_id, path, content, description?)` registers a test for one criterion. Every `create_test` rule applies: a supported test file name, no overwrite of any snapshot path, uniquely named Go `TestX(t *testing.T)` functions, or static top-level `test("title", …)` / `it("title", …)` titles for JavaScript and TypeScript. In addition:
  - `criterion_id` must name a criterion that occurs exactly once;
  - the trusted `generated_test` template must let Probe check which named tests ran: a Go template such as `["go", "test", "{package}"]`, or a Jest-compatible runner (Jest, Vitest) with `{file}` and `{results_out}`. Without one the call is refused and nothing is created, because no evidence could be recorded;
  - intent tests share `reviewer.max_generated_tests` with generated tests and may use at most half of it, rounded up (5 of the default 10). Deleting a test does not return its slot.
- `run_intent_test(test_id)` stages the test in the candidate snapshot only, runs the selected named tests in one sandbox container (check kind `generated_test_intent`), removes the file again, and records one `intent_test` evidence record. The base snapshot is never touched and there is no baseline run. The run is charged to `sandbox.max_runtime_seconds` like any reviewer experiment, and it is never served from the execution cache.

`create_intent_test` is audited without the test source. `run_generated_test` refuses an intent test, and `run_intent_test` refuses a differential test.

### Evidence statuses

An `intent_test` record always carries `check_id`, `criterion_id`, `runner` (`go_test_json` or `jest_json`), `path` and `test_names`, and never a `base_check_id`. Its status is:

| Status | When |
| --- | --- |
| `INTENT_TEST_FAILED` | The named test started and failed (exit 1 to 124, output not truncated), the failure is an assertion of the test's own file, and the test names a declaration the change added or modified (matched by name) whose name also occurs on an added line of a changed non-test file. |
| `INTENT_TEST_PASSED` | Every named test started and passed (exit 0, output not truncated), and the test names at least one declaration the change added or modified. |
| `UNVERIFIED` | Anything else, with the reason in the description: a panic, a thrown error or a runtime error, a failure outside the test's own file, no changed declaration named (whether the test failed or passed), no referenced symbol on an added line, a setup failure, a skip, a timeout, truncated output, an unrelated failure, or a skipped run. Some of these also make the check ERROR, which exits 4 (see Retention and setup failures). |

What counts as an assertion failure:

- **Go** (`go test -json`): a named test ends with a `fail` event, one of its output events (or a subtest's) is a `<file>:<line>: <message>` line with a non-empty message whose file is the intent test's own file, as `t.Errorf`, `t.Fatalf` and `t.Log` write, and no output event of the run contains `panic:`.
- **Jest-compatible report**: in the report entry for exactly the intent test's file, whose file-level `message` is empty, a named top-level test is `failed` with a non-empty failure message, and every non-empty failure message of a failed named test starts (terminal color codes removed) with an assertion-error header: `AssertionError:` or `AssertionError [` (Vitest, Chai, `node:assert`), or Jest's matcher header `Error: expect(` or `Error: expect.`. A plain `Error`, a custom error class, a runtime error (`TypeError`, `ReferenceError`, …), a thrown non-error value or a test timeout, whether the code under test or the test threw it, is not an assertion failure. Other Jest-compatible runners whose assertion messages start otherwise record `UNVERIFIED`.

Code executing in the sandbox writes both channels, and code under test can throw an error whose message starts like an assertion. The rule tells kinds of failure apart; it does not authenticate them.

### Referenced symbols

`referenced_symbols` lists, at most 32, the changed declarations the intent test names:

- **Go**: every identifier and selector name of the test file (`go/ast`), intersected with the top-level declarations (functions, methods by method name, types, variables, constants) of the candidate's changed non-test Go files whose source range contains an added line.
- **JavaScript and TypeScript**: identifiers read lexically from the test (outside strings and comments), intersected with top-level declarations found lexically at column 0 (`function`, `class`, `const`, `let`, `var`, `interface`, `type`, `enum`, `const enum`, `namespace`, with `export`, `default`, `declare`, `abstract` and `async` prefixes). The record's description and the Intent Test Failures entry say the match is lexical.

Names are matched, not resolved: a local variable that shares a changed declaration's name also matches. When the intersection is empty, the record is `UNVERIFIED` with "the intent test references no symbol the change added or modified", whether the test failed or passed: a test that touches no changed declaration says nothing about this change.

Test files never link a test to the change: `_test.go` files, JavaScript and TypeScript files named `<name>.test.<ext>` or `<name>.spec.<ext>` for `.js`, `.jsx`, `.mjs`, `.cjs`, `.ts`, `.tsx`, `.mts` and `.cts`, and JavaScript and TypeScript files under a `__tests__` directory. Their declarations and added lines are not read.

An `INTENT_TEST_FAILED` record additionally needs at least one referenced symbol to occur as a whole word on an added line of a changed, non-deleted, non-test file of the recorded diff. This is what `report.Finalize` can check again from the report alone. It has a consequence: when a change only edits the body of a multi-line function and the function's name is not on an added line, a failing intent test for that function stays `UNVERIFIED` ("no symbol the intent test references is named on an added line …"). Files whose path is secret-bearing or would be altered by redaction are not read for this rule, and lines are read redacted, so a name that only a redacted secret contains does not count.

The harness computes the changed declarations and the words of the added lines once per run and reuses them for every intent test, and `Finalize` reads the added lines once per verification, so the host-side cost stays linear in the size of the diff whatever the number of intent tests or referenced symbols. The harness stops this work when the run's time limit or `--deadline` has passed; the intent test is then not started either.

### Retention and setup failures

A test that records `INTENT_TEST_FAILED` is retained as a hashed `intent_test` artifact, and the reviewer can no longer delete it. If it cannot be retained, the record is `UNVERIFIED` instead.

Under the inherited generated-test rules, an intent check becomes ERROR whenever the named test did not run and end with a pass or a failure that the runner output confirms:

- the test does not compile or cannot start (for example "[build failed]" in the Go output);
- the output of the run contains one of the inherited setup-failure markers, such as `permission denied` or `command not found`, wherever it comes from;
- the named test was skipped, or only an unrelated test failed;
- the `go test -json` framing is broken, a run or terminal event of the named test is missing, or the output was truncated;
- the Jest-compatible report is missing, duplicated for the file, or unreadable.

The record is then `UNVERIFIED`. Tests written against new API often fail this way. Unless the output was truncated, such an ERROR is a failure of the model's test, not of the run: the check records `error_cause: "test"`, and it requests review (exit 2 with `--ci`) instead of exit 4. A Docker failure, an exit code of 125 or above, a lost log or a truncated output remain failures of the run (exit 4). Code under test writes the same log, so it can also make its intent check ERROR (for example by printing `permission denied`), which leaves the record unverified; it cannot turn anything into exit 1.

## Hypotheses

The reviewer may submit `INTENT_TEST_FAILED` with the evidence ID of an `intent_test` record and the `criterion_id` that test was created for. `report.Finalize` re-derives every `intent_test` record from its recorded check with the same function the harness used, and a stored status counts only when the recomputation agrees and the check resolves exactly once, has kind `generated_test_intent`, has a command and was executed rather than replayed. The hypothesis keeps `INTENT_TEST_FAILED` only when a cited record re-derives as `INTENT_TEST_FAILED` and its criterion equals the hypothesis's, and that criterion occurs exactly once in `intent_criteria`. Anything else becomes `UNVERIFIED`.

- An accepted `INTENT_TEST_FAILED` hypothesis is copied to `intent_test_failures`, adds a review target, and requests human review: exit 2 with `--ci`, 0 without, never 1, at any severity.
- `intent_test` evidence supports no other status: not `REPRODUCED`, `NOT_REPRODUCED`, `DISMISSED` or `DIVERGED`. `INTENT_TEST_PASSED` supports nothing at all.
- Any hypothesis may carry a `criterion_id` naming the criterion it concerns; the report quotes the criterion from `intent_criteria`, labelled "model-selected". An unknown `criterion_id` is dropped.
- `intent_judgment` (`expected_change` or `unexpected_change`) is accepted only on a `DIVERGED` claim that also names a known criterion. It is the model's opinion about whether the criterion asks for the recorded difference, rendered after the hypothesis as "Model judgment (not evidence): expected change" (or unexpected change), and never read by any status, list, severity, review target or exit code. `Finalize` drops a judgment on any other final status, and a dropped link adds one Unverified note naming the hypothesis, which requests review with `--ci`.

`probe report` runs `Finalize` again, so a saved report whose intent-test failure is edited, forged or stripped of its check becomes `UNVERIFIED` on re-render.

## Report output

From a real run: a Go change adds `Discount`, which takes 10 off only above 100; the intent above; a scripted reviewer writes a test for `AC-1` (`Discount(100)` must be 90), which fails, and one for `AC-2` (`FreeShipping(50)` must be true), which passes. It cites both as failures and adds a `DIVERGED` claim with a judgment and an invented evidence ID. `review --ci` exited 2 (0 without `--ci`), and printed:

```text
Intent: 2 acceptance criteria extracted; 1 intent-test failure (a model-written test failed on the candidate; no baseline control; weaker than a reproduced issue).
```

When intent tests ran but no failure was accepted (another real run, whose three intent tests were all inconclusive), and when no intent test ran (always the case for `lint`), the line reads respectively:

```text
Intent: 2 acceptance criteria extracted; no intent-test failure was accepted, which says nothing about whether the criteria hold.
Intent: 2 acceptance criteria extracted.
```

The Markdown report:

```text
## Investigation Summary

- **INTENT\_TEST\_FAILED / high** Orders of exactly 100 get no discount (hypothesis-1): The intent test for AC-1 failed on an assertion: Discount\(100\) returned 100.
  Criterion: AC-1 (intent line 4): "Orders of 100 or more get 10 off"
- **UNVERIFIED / medium** Free shipping threshold (hypothesis-2): Cites a passing intent test.
  Related criterion (model-selected): AC-2 (intent line 5): "Orders of 50 or more ship free"
- **UNVERIFIED / low** Discount behaviour changed as asked (hypothesis-3): No such evidence exists.
  Related criterion (model-selected): AC-1 (intent line 4): "Orders of 100 or more get 10 off"

…

## Intent Test Failures

Candidate-only experiments. Each entry cites a test the reviewer wrote for one acceptance criterion: the test failed on an assertion on the candidate and names a symbol that a declaration the change added or modified also has, matched by name and not resolved. No baseline run acts as a control, and the test, its inputs and its reading of the criterion are model-written, so the test or the reading may be wrong. This is weaker than a reproduced issue and never sets exit code 1.

- **high** Orders of exactly 100 get no discount — shop.go:14
  Criterion AC-1 (intent line 4): "Orders of 100 or more get 10 off"
  Evidence: evidence-1
  Test probe\_intent\_ac1\_test.go (TestProbeIntentDiscountAt100) failed on an assertion; names it shares with declarations the change added or modified (matched by name, not resolved): Discount.

## Intent Criteria

Acceptance criteria are Markdown list items copied verbatim from the supplied intent (criteria grammar v1; SHA-256 5c4c1f90dde9611f57e5f2769d42ff3f2ead698476e2add010192c0056a3649d). IDs are positional and belong to exactly this intent text. Extraction is not understanding of the intent. An intent test runs on the candidate only, with no baseline control; a test that ran without failing says nothing about whether its criterion holds.

- AC-1 (line 4): "Orders of 100 or more get 10 off" — failed on an assertion on the candidate: evidence-1
- AC-2 (line 5): "Orders of 50 or more ship free" — ran without failing: evidence-2 (a test that ran without failing says nothing about whether the criterion holds)
```

The Unverified Areas section of the same report lists "The intent link of hypothesis hypothesis-3 was discarded: …", and Recorded Evidence prints "Candidate check: check-1 (candidate-only; no baseline control)" under each intent record. The `intent_test` record in `confidence-report.json`:

```json
{
  "id": "evidence-1",
  "kind": "intent_test",
  "description": "Orders of exactly 100 get 10 off (candidate-only intent test for AC-1; no baseline control)",
  "path": "probe_intent_ac1_test.go",
  "check_id": "check-1",
  "criterion_id": "AC-1",
  "referenced_symbols": ["Discount"],
  "status": "INTENT_TEST_FAILED",
  "runner": "go_test_json",
  "test_names": ["TestProbeIntentDiscountAt100"]
}
```

The sections appear only when an intent was supplied. With an intent but no criteria, Intent Criteria says so and that this does not mean the intent states no requirement. Each criterion lists at most 10 evidence IDs, then a count.

## Exit codes

| Source | Without `--ci` | With `--ci` |
| --- | --- | --- |
| Hypothesis `INTENT_TEST_FAILED` | 0 | 2, never 1 |
| Intent link discarded by `Finalize` (Unverified note) | 0 | 2 |
| Extraction limits reached (Unverified note) | 0 | 2 |
| `INTENT_TEST_PASSED`; a retained `intent_judgment` | 0 | 0 |
| Intent not UTF-8 or containing NUL | 3 | 3 |
| Intent check ERROR: the named test did not run and end with a pass or a failure (compile or setup failure, setup-failure marker in the output, skip, unrelated failure, broken framing, truncated output, missing Jest report) | 4 | 4 |

## What is and is not claimed

- `INTENT_TEST_FAILED` records that a model-written test for one quoted criterion, which references changed code, failed on an assertion on the candidate. It is not a reproduction, a defect, a regression or a contradiction of the intent, and it is not proof that the code is wrong: the test, its inputs and its reading of the criterion are model-written, the criterion may be ambiguous or outside this change, and there is no baseline control.
- `INTENT_TEST_PASSED` says nothing about whether a criterion holds. A criterion is never presented as satisfied, met, implemented, accepted or tested, and criteria are never counted as a ratio.
- `intent_judgment` is model judgment, not evidence. It never hides, demotes or dismisses anything, and never changes a status, a severity, a review target or the exit code; an attacker-written intent can at most earn a labelled opinion next to a divergence.
- Criteria extraction is not understanding of the intent, and "no criteria" does not mean "no requirements".
- The Go test log and the Jest-compatible report that an intent status is read from are written by code executing in the sandbox. The assertion rule tells kinds of failure apart and does not authenticate them, so neither status is tamper-proof.

## Limitations

- Only Go named tests and Jest-compatible reports can support a status; other runners are refused at `create_intent_test`.
- For Jest-compatible reports, only failure messages with the assertion-error headers listed above count as assertions; a runner or assertion library that words them otherwise records `UNVERIFIED`.
- JavaScript and TypeScript symbols are matched lexically.
- A change confined to a function body whose name is not on an added line cannot support `INTENT_TEST_FAILED` (see Referenced symbols).
- There is no baseline run of an intent test, so Probe never says that a change delivered a criterion.
- Criteria IDs are positional; editing the list renumbers them.

## See also

- [Jira issues](JIRA.md), [Linear issues](LINEAR.md), [Notion pages](NOTION.md) and [Google Docs](GOOGLE_DOCS.md): tickets, pages and documents as intent.
- [Observation experiments](OBSERVATIONS.md): the DIVERGED hypotheses that may carry an `intent_judgment`.
- [Exports](EXPORTS.md): the `intent_test_failed` class and the PR comment that must never be pasted into the intent.
- [Agent workflow](AGENT_WORKFLOW.md) and [CI integration](CI.md).
- [Security boundaries](SECURITY.md).
