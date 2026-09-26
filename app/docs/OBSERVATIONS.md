# Observation experiments

A generated test normally asserts what the changed code should return, and the reviewer model guesses that expected value. When the guess is wrong, the experiment says more about the model than about the change. An observation experiment removes the guess: the generated test records the values the code returns, under named keys, and SwiftProof compares what the baseline and the candidate recorded for the same inputs. A human then decides which value is intended.

Observation experiments add no policy key, no flag, no tool, no container channel and no environment variable. They run inside the reviewer's existing `create_test` and `run_generated_test` tools whenever the trusted policy has a `generated_test` template whose named execution SwiftProof can establish (Go `["go", "test", "{package}"]`, or a Vitest template with `{file}` and `{results_out}`).

## Recording values

**Go** (the sandbox image needs a Go 1.25 or later toolchain for `testing.T.Attr`). In the top-level test function, record one attribute per input under a key that starts with `swiftproof.`:

```go
func TestSwiftProofDiscountObservations(t *testing.T) {
	for _, in := range [][2]int{{100, 10}, {5, 33}, {0, 50}} {
		t.Attr(fmt.Sprintf("swiftproof.Discount(%d,%d)", in[0], in[1]), fmt.Sprintf("%#v", Discount(in[0], in[1])))
	}
}
```

`go test -json` turns each call into an `attr` event in the recorded check log. SwiftProof reads only `attr` events of exactly the generated top-level test names; subtests, other tests and keys without the prefix are ignored. An older toolchain fails to compile the test, which leaves the experiment `UNVERIFIED`.

**Vitest** (the channel is the Jest-compatible JSON report that the template writes to `{results_out}`):

```ts
test("observe discount", ({ task }) => {
  (task.meta as any).swiftproof = { "discount(5,33)": discount(5, 33), "discount(100,10)": discount(100, 10) };
});
```

Vitest copies `task.meta` into the report; console output cannot reach it. SwiftProof keeps each value as canonical JSON text with its type: a string is recorded quoted (`"4"`), a number as written (`4`), an object with sorted keys. The number `4` and the string `"4"` are therefore different values. JSON drops `undefined` and turns `NaN` into `null`, so record such values with `String(x)`. A value whose canonical text exceeds 1024 bytes, or a key longer than 200 bytes, is kept only as a fixed stand-in that carries its length and a sha256 of its redacted text; a stand-in is never compared.

**Jest** reports carry no per-test metadata, so a Jest template cannot record observations. A generated test that declares observations but records none gets an `UNVERIFIED` observation record that says so.

Rules for keys and values:

- keys are unique per test, 1 to 200 bytes, valid UTF-8, without whitespace or control characters;
- at most 32 keys per test run; more makes the whole experiment `UNVERIFIED`;
- values are single-line and deterministic, at most 1024 bytes each; record errors and recovered panics as values;
- never record timestamps, random values, pointers, memory addresses, stack traces or `file:line` positions.

The `create_test` and `run_generated_test` tool descriptions and the reviewer's system prompt carry the same recipe.

## What SwiftProof compares

`run_generated_test` runs the generated test on the baseline and on the candidate as before, and records the ordinary `differential_test` evidence. When either run recorded observations, or the test source declares them, it also records one `differential_observation` evidence record that cites the same two checks. Values are compared only when the named test passed on both revisions with exit code 0, under the same command, and the named executions were validated from the runner's structured output.

Each key gets one row status:

| Row status | When |
| --- | --- |
| `EQUAL` | The baseline and the candidate recorded the same value, and the baseline repeat, if one ran, recorded it too. |
| `DIVERGED` | The baseline, the baseline repeat and the candidate each recorded the key exactly once; the two baseline runs recorded the same value and the candidate a different one. |
| `UNSTABLE` | The baseline repeat did not record the value of the first baseline run. |
| `INCOMPARABLE` | The key is invalid, or was recorded more than once in one run, or on one revision only; a value contains `[REDACTED]`, would be altered by redaction, exceeds 1024 bytes or is the stand-in of a longer Vitest value; a Go value was too long for the test runner to convert; or two differing values look like a memory address or a source position. |

When at least one key differs and nothing else prevents a comparison, SwiftProof runs the staged generated test **once more on the baseline**, live, as a check of kind `generated_test_base_repeat`. That run is never served from the execution cache and is charged to the sandbox runtime budget like any other run. Only this one repeat runs per experiment.

The record's status is:

- `DIVERGED` when any row is `DIVERGED`, whatever the other rows are;
- `NOT_DIVERGED` when at least one row was compared and every row is `EQUAL`;
- `UNVERIFIED` otherwise, with a fixed reason (a run did not pass, the repeat was skipped or did not pass, a channel error, only unstable or incomparable differences, or nothing was recorded).

Values are compared in full, as recorded in the check log (Go) or in the check's structured results (Vitest). The rows shown in reports carry display cuts of 256 bytes, UTF-8 safe, with `truncated` set.

A `DIVERGED` experiment retains the generated test source as a hashed `generated_test` artifact, and the reviewer can no longer delete that test. If the source cannot be retained, the record is `UNVERIFIED` instead.

## Hypotheses and the report

The reviewer may submit a hypothesis with status `DIVERGED` that cites a `differential_observation` record. `report.Finalize` re-derives every observation record from the recorded checks with the same function the harness used, and a stored status counts only when the recomputation agrees. A `DIVERGED` hypothesis is kept only when a cited record is re-derived as `DIVERGED`, its repeat check was executed in this run (never replayed), and the record is listed in `divergences`; otherwise the hypothesis is `UNVERIFIED`. A `DIVERGED` claim citing the `differential_test` of the same runs is `UNVERIFIED`. A `NOT_DIVERGED` record may rest on a replayed baseline only when the opt-in execution cache entry was stored after two live runs that agreed on the pass/fail status and the exit code. The replayed baseline values come from one recorded live run, not from two runs that recorded the same values, and the conclusion is replay-backed: the report lists it in `execution.replay_backed`.

`divergences[]` in `confidence-report.json` lists every validated `DIVERGED` observation record, cited or not, with its test path and names, the three check IDs (baseline, candidate, repeat), the citing hypotheses and the `DIVERGED` rows only (at most 32). An entry is anchored at the path and line of the lowest-numbered citing hypothesis whose path is a changed, non-deleted file (`anchor_source: "hypothesis"`, a model-chosen location); otherwise it has no anchor.

The Markdown report always has a `## Behavior Divergences` section. From a real run (a rewrite of `Discount` from `total - total*percent/100` to `total * (100 - percent) / 100`, recorded on three inputs):

```text
## Behavior Divergences

- **evidence-2** differential\_observation — price/price.go:5 (model-chosen location); test price/swiftproof\_observe\_test.go (TestSwiftProofDiscountObservations); hypotheses: hypothesis-1
  - Discount\(5,33\): baseline 4; candidate 3

Recorded values differ between revisions for the same inputs: the candidate differed from baseline runs that agreed with each other. This does not establish which revision is correct.
```

The same run printed this console line:

```text
1 recorded behavior divergence (baseline and candidate recorded different values; a human decides which is intended).
```

and recorded this entry in `confidence-report.json` (abridged to the divergence):

```json
{
  "evidence_id": "evidence-2",
  "kind": "differential_observation",
  "path": "price/price.go",
  "line": 5,
  "anchor_source": "hypothesis",
  "test_path": "price/swiftproof_observe_test.go",
  "test_names": ["TestSwiftProofDiscountObservations"],
  "check_ids": ["check-1", "check-2", "check-3"],
  "hypothesis_ids": ["hypothesis-1"],
  "observations": [
    {"test": "TestSwiftProofDiscountObservations", "key": "Discount(5,33)", "status": "DIVERGED", "base": "4", "candidate": "3", "base_recorded": true, "candidate_recorded": true}
  ],
  "note": "Recorded values differ between revisions for the same inputs: the candidate differed from baseline runs that agreed with each other. This does not establish which revision is correct."
}
```

When no experiment diverged, the section says whether values were compared and found equal (every observation or fuzz record re-derived as `NOT_DIVERGED`) or whether some records are inconclusive, in which case nothing is said about their values.

## A difference is never hidden behind a passing test

The `differential_test` of the same runs passes on both revisions, so on its own it could support `NOT_REPRODUCED` even though the recorded values differ. `report.Finalize` therefore withdraws a re-derived `NOT_REPRODUCED` whose runs may have recorded a difference. Only full values recorded readably on both revisions, with no redaction marker, can show that nothing differs. The rule fails closed and counts each of these as a possible difference:

- a key recorded on both revisions with different full values;
- a key recorded on one revision only;
- a key whose value could not be read on either revision (for example a Go value too long for the test runner to convert);
- a key or value that holds `[REDACTED]`, because equal redacted texts do not establish equal original values;
- observations that could not be read at all on either revision (a channel error, or Vitest observations replaced by a fixed marker).

This holds whatever the observation record's own status is (for example when the only differing key was unstable), and the values are read again from the checks of the `differential_test` record itself, so removing the observation record from a saved report does not lift the rule. Two stand-ins of long Vitest values count as different when their lengths or hashes differ. The withdrawn record is shown as "as stored; not accepted as evidence", and a hypothesis resting on it becomes `UNVERIFIED`.

## What is and is not claimed

- `DIVERGED` and a `divergences[]` entry record that, for the recorded inputs and the test's own serialization, two baseline runs agreed and the candidate recorded a different value. They do not say which revision is correct, and they are not a defect, a breaking change or a reason for exit 1. The values are lossy, bounded, redacted serializations, not the function's full output, and nothing is claimed about determinism beyond the two baseline runs and the one candidate run.
- `NOT_DIVERGED` records that the compared values were equal in the recorded runs. It does not establish equivalent behavior, even for the recorded inputs, and it supports no hypothesis status: not `NOT_REPRODUCED`, not a dismissal, not a severity change.
- `UNSTABLE` and `INCOMPARABLE` rows, and `UNVERIFIED` records, say nothing about the values.
- The link between a divergence and a changed line is the model's choice of location, labelled "model-chosen location".
- Observations only add review requests. They never delete a signal, lower a severity, support a dismissal or mark anything resolved.

## Trust and tampering

Both channels can be written by code executing in the sandbox. They are kept apart from ordinary log text so that unrelated output cannot impersonate an observation, and nothing more:

- In Go, a plain line such as `=== ATTR  TestX swiftproof.k v` printed by the code under test stays an ordinary output event. Only a line carrying the test runner's framing byte (`\x16`) becomes an `attr` event. Code under test that prints such a framed line for a key the test also records makes that key recorded twice, hence `INCOMPARABLE`: a forged line can turn a divergence into `UNVERIFIED`, and can add a key, but it cannot produce `NOT_DIVERGED` for a key it duplicates.
- In Vitest, console output cannot reach `task.meta`; the test code and the code it calls can.
- Code that sabotages its own test process (for example by writing directly to the process's standard output descriptors) can forge anything a passing test can. This is the same limit as for `NOT_REPRODUCED`, and it is why these records are observations, not proof.

Values are redacted before they are parsed, compared, displayed or sent to a provider. Equal redacted values prove nothing and are `INCOMPARABLE`; a value that redaction would alter after decoding (for example a JSON-escaped credential) is `INCOMPARABLE` too. A normalized Vitest meta that redaction would alter is replaced by a fixed marker. When the metas would make an otherwise readable runner report unreadable, or push it over the payload limit or the results budget, they are all replaced by a marker instead. Observations therefore never change whether the pass or fail of the test itself is established. Reports remain unsigned.

## Budgets and limits

- At most one extra baseline run per experiment, only when a key differs. It consumes runtime budget, not the `max_generated_tests` budget. When the budget is exhausted, the repeat is recorded as `SKIPPED` and the record is `UNVERIFIED`.
- 32 keys per run, keys up to 200 bytes, values compared up to 1024 bytes, 256-byte display cuts, 20 rows per divergence in Markdown and 32 in JSON.
- Go values longer than the test runner's line limit (about 4 KiB) are not converted into `attr` events; the key is `INCOMPARABLE`, and the both-pass `NOT_REPRODUCED` of the same test is withdrawn, because equality is then unknown.
- Each Go observation appears twice in the log (an `attr` event and its echoed output line), so it costs about twice its size in `sandbox.max_output_bytes`. A truncated log makes the check an ERROR (exit 4), as for any generated test.
- Vitest observations travel inside the structured results, which share the report's results budget. Values over 1024 bytes and keys over 200 bytes are kept as stand-ins of about 100 bytes. When the kept observations would still push a report over the per-run payload limit or over what remains of its side's results budget, every observation in that report is replaced by a fixed marker (the record is then `UNVERIFIED`), and the report is accepted or rejected on the rest of its content.

## Exit codes

| Record | Without `--ci` | With `--ci` |
| --- | --- | --- |
| Hypothesis `DIVERGED`, or any `divergences[]` entry | 0 | 2, never 1 |
| Hypothesis `UNVERIFIED` (for example an unsupported `DIVERGED` claim) | 0 | 2 |
| A `NOT_DIVERGED` or `UNVERIFIED` observation record by itself | 0 | 0 |
| A repeat check that did not pass (`SKIPPED`, `TIMEOUT`, `FAIL`), like any check that did not pass | 0 | 2 |
| A repeat check that ends in ERROR, like any ERROR check | 4 | 4 |

## Limitations

- Go (1.25 or later) and Vitest only. Jest, Python and other runners record no observations.
- No observations from failing runs: a candidate failure keeps the ordinary `REPRODUCED` path, and the observation record is `UNVERIFIED`.
- No subtest observations, no candidate-side repeat, no tolerance for floating-point noise, no order-insensitive collections and no normalization of error text. A reordered struct field or a reworded error is a real, recorded difference that a human may judge uninteresting.
- A value that is identical in both baseline runs but depends on the build (for example a heap address on a toolchain that does not randomize it) can still diverge; the address and source-position heuristics catch only common forms.
- An older binary that re-renders a newer report silently drops `divergences`; render with the binary that produced the report. Consumers that validate reports against an older schema reject the new fields.
