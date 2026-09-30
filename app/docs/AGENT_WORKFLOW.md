# Using Probe in an agent coding loop

With Claude Code, the official plugin wraps this loop: `/probe:review`,
`/probe:findings`, `/probe:fix` and `/probe:context` (intent, plan, base), and a
`probe-reviewer` agent Claude can run before a handoff. See
[`plugins/probe`](../../plugins/probe/README.md). The instructions below apply
to any agent.

Add the following instructions to a project's agent guidance after installing a
trusted Probe binary and committing its reviewed `.probe.json` policy:

> Before handing over a committed change, run `probe lint --base origin/main`.
> Inspect `.probe/CONFIDENCE_REPORT.md`, investigate relevant findings, and
> rerun after corrections. Before requesting PR review, run `probe review`
> when the project's prepared Docker image is available. In the handoff, report
> each of these separately:
>
> - reproduced issues;
> - baseline versions of changed tests that fail on candidate code, and impacted
>   tests that fail on candidate code (`FAILS_ON_CANDIDATE`);
> - behavior divergences, with both recorded values: a divergence does not say
>   which revision is right;
> - intent-test failures, listed apart from reproduced issues: the test and its
>   reading of the criterion are model-written, and no baseline run controls them;
> - surviving mutants, which may be equivalent to the original code and are not
>   defects or missing tests by themselves;
> - unresolved hypotheses;
> - incomplete checks, including stages that did not run, inconclusive fuzz
>   results and incomplete mutation runs.
>
> Never treat a model assertion, a passing check or a zero exit code as proof of
> correctness. Do not change the baseline policy to make findings disappear. Do
> not create commits solely to satisfy this check when the task does not
> authorize commits; explain that uncommitted changes are outside Probe's
> analysis.

To share results on a pull request, render them with `--format pr-comment` and
post `PR_COMMENT.md` as a PR **comment**, never into the PR description. The
description is the usual `--intent-file` source: Probe removes its own
marked comment block from the intent, but any other text copied there is read
as intent, and its list items can become acceptance criteria. Put the handoff
list above in a PR comment or the handoff message for the same reason. See
[exports](EXPORTS.md) and [intent criteria](INTENT.md).

To make an agent announce its work before doing it, add:

> Before writing code for a task, run `probe plan --intent-file <task file>`
> and read `.probe/PLAN.md`; raise any flagged category with a human when the
> task requires it. After committing, run `probe lint --plan
> .probe/PLAN.json` (and `review --plan` before PR review) and report every
> plan-conformance difference in the handoff: files outside the plan, unannounced
> exported changes, critical paths and dependency manifests, and the plan gate
> decision with its reasons. Never edit PLAN.json to make the change conform:
> update the plan with `probe plan` and have it approved again.

`plan` needs the configured provider; the plan is the model's proposal and its
assessment comes from fixed rules, so neither is proof that the change is safe.
See [plans and scope drift](PLAN.md).

Use `--reviewer=false` for a provider-free review. A model configured in the
trusted policy or through `PROBE_REVIEWER_MODEL` is used automatically by
the current source version; v0.1.0 requires `--reviewer`.
The CI adapter explicitly supplies this flag to work with either version.
