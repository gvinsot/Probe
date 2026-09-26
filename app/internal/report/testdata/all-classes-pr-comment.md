<!-- swiftproof:pr-comment:begin v1 -->
## SwiftProof: 7 evidence-backed findings

**Status (not a finding)**

- Exit code 1: a reproduced high or critical hypothesis was recorded.
- Unverified areas: 0 (0 recorded notes and 0 hypotheses that stayed UNVERIFIED).
- Checks that did not pass: 4 (the mutation ledger is not counted): candidate-1 generated\_test\_candidate FAIL; check-2-4 base\_test\_hybrid FAIL; check-2-7 impacted\_test\_candidate FAIL; check-1-19 generated\_test\_intent FAIL.
- Stages that did not run: 0.
- Not findings, listed only in the full report: 0 risk signals and 0 suggested review ranges.
- No finding is not approval.

Only findings backed by recorded sandbox evidence are listed; signals, review ranges, unverified hypotheses and model judgments are never findings.

### Reproduced hypotheses (1)

A generated test passed on the baseline and failed on the candidate. This does not confirm a defect: a human judges whether the test's assertion is the intended behavior.

- **high** severity, assigned by the reviewer model. Reviewer-model title: Regression. Location: auth.go:3 (model-chosen location).
  - Evidence: experiment-1. Checks: base-1, candidate-1.
  - Hypotheses: h1.
  - Generated test: guest\_test.go \(TestRegression\)
  - Runs: baseline check base-1 PASS; candidate check candidate-1 FAIL \(go\_test\_json\)
  - Retained generated\_test artifact: sha256 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa (artifacts/0a1b2c3d-generated-test-1-guest\_test.go)

### Changed baseline tests that fail on candidate code (1)

The baseline version of a test the change edited passed on the baseline and failed on candidate code. The change may intend this; it does not show that the test edit is wrong or deliberate.

- Location: clamp\_test.go:10–13 (candidate test range).
  - Evidence: evidence-1-4. Checks: check-1-4, check-2-4.
  - Test: TestClampUpper
  - Baseline declaration: clamp\_test.go:​10–14
  - Change to the test: modified
  - Runs: baseline check check-1-4 PASS; hybrid-tree check check-2-4 FAIL

### Impacted tests that fail on candidate code (1)

An unchanged test that the approximate static index links to a changed function passed on the baseline and failed on the candidate. The failure may come from any part of the change or from flakiness.

- Location: none in the changed files.
  - Evidence: evidence-1-7. Checks: check-1-7, check-2-7.
  - Test: TestTotal
  - Test declaration: cart/cart\_test.go:​20
  - Package: example.test/shop/cart
  - Linked changed functions \(approximate\): Discount \(cart/price.go:​30, depth 2, interface link\); Price \(cart/price.go:​5, depth 1, static link\)
  - Runs: baseline check check-1-7 PASS; candidate check check-2-7 FAIL

### Differential fuzzing divergences (1)

Identical seeded inputs gave different recorded values on the two revisions. This does not establish which revision is correct.

- Location: calc/calc.go:10 (changed function).
  - Evidence: evidence-1-10. Checks: check-1-10, check-2-10, check-3-10, check-4-10.
  - Function: Percent
  - Input: Percent\(1, 3\)
  - Baseline value: 33
  - Candidate value: 34
  - Input: Percent\(2, 3\)
  - Baseline value: 66
  - Candidate value: 67
  - More rows: 1 further diverging row is in confidence-report.json

### Observed behavior divergences (1)

A model-written test recorded different values on the two revisions for the same inputs. This does not establish which revision is correct.

- Location: discount.go:4 (model-chosen location).
  - Evidence: evidence-1-15. Checks: check-1-15, check-2-15, check-3-15.
  - Hypotheses: h1.
  - Generated test: discount\_obs\_test.go \(TestObserveDiscount\)
  - Recorded key: Discount\(5,33\)
  - Baseline value: 4
  - Candidate value: 3
  - Retained generated\_test artifact: sha256 eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee (artifacts/0a1b-generated-test-2-discount\_obs\_test.go)

### Intent test failures (1)

Each test below is model-written and ran on the candidate only.

- A model-written test for AC-2 failed on the candidate; there is no baseline control, and the test or its reading of the criterion may be wrong. Reviewer-model title: Empty cart total. Location: cart.go:8 (model-chosen location).
  - Evidence: evidence-1-19. Checks: check-1-19.
  - Hypotheses: h1.
  - Criterion AC-2: A total is never negative.
  - Intent test: cart\_intent\_test.go \(TestIntentNonNegative\)
  - Run: candidate check check-1-19 FAIL \(candidate only; no baseline control\)
  - Changed symbols the test references: Total
  - Retained intent\_test artifact: sha256 ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff (artifacts/0a1b-intent-test-1-cart\_intent\_test.go)

### Surviving mutants (1)

The package's tests passed with this single change to an added line, as they did without it. The mutant may be equivalent to the original code.

- Location: discount.go:6 (mutated added line).
  - Checks: mutation-check-1, mutation-check-2 (mutation ledger).
  - Mutant: mutant-1
  - Operator: comparison
  - Original: if pct &gt; 50 {
  - Mutated: if pct &gt;= 50 {
  - Package: ./
  - Runs: control check mutation-check-1 PASS; mutant check mutation-check-2 PASS with 2 passing tests
  - Retained mutant\_patch artifact: sha256 9999999999999999999999999999999999999999999999999999999999999999 (artifacts/0a1b-mutant-1.patch)

Full report: [CONFIDENCE\_REPORT.md and confidence-report.json](https://example.invalid/runs/1)

---

Generated by SwiftProof v1.2.3 (https://github.com/gvinsot/SwiftProof).
<!-- swiftproof:pr-comment:end -->
