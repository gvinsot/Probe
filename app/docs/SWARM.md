# Reviewer swarm: specialized agents in parallel

By default, one bounded AI reviewer investigates a change. With `--swarm`, several **specialized agents** investigate it in parallel. Each agent looks at the change with one focus, and on a large change several agents split the files between them. Probe then merges their findings deterministically and checks every one against the recorded evidence, exactly as it checks a single reviewer's.

```sh
probe review --base main --swarm                                  # the default agents
probe review --base main --swarm-agents security,correctness      # chosen agents, in this order
probe review --read-only --base main --swarm                      # also in read-only review
```

```json
{
  "reviewer": {
    "model": "…",
    "swarm": {"agents": ["correctness", "security", "tests"], "max_parallel": 3, "partition_files": 15}
  }
}
```

## Agents

| Agent | Focus |
| --- | --- |
| `correctness` | Logic errors, edge cases (empty, nil, zero, boundaries, large inputs, Unicode), broken invariants, and regressions of existing behavior. |
| `security` | Injection, authentication and authorization bypasses, untrusted input at trust boundaries, exposed secrets or personal data, unsafe deserialization, SSRF, misused cryptography, permissive defaults. |
| `tests` | Whether tests exercise the changed behavior: unreached changed code, weakened or deleted assertions, tests that cannot fail, mocks hiding the change. Where the tools allow it, it writes differential tests for the riskiest untested behavior. |
| `compatibility` | Contracts: exported APIs and signatures, callers in unchanged code, serialized formats, configuration, schemas and migrations, and contracts with other components or [other repositories](../README.md#cross-repository-context-and-repository-clusters). It uses the impact, [graph](GRAPH.md) and context tools. |
| `reliability` | Error handling, concurrency (races, deadlocks, leaked goroutines, threads or promises), resource leaks, timeouts and retries, performance regressions. |
| `intent` | Whether the change does what its [intent](INTENT.md) asks: criteria not implemented or implemented differently, and behavior nobody asked for. It uses intent tests where the tools offer them. It runs only when the intent yielded acceptance criteria; the intent can come from [Jira](JIRA.md), [Linear](LINEAR.md), [Notion](NOTION.md) or [Google Docs](GOOGLE_DOCS.md). |

With `--swarm` and no agent list, every agent runs in the order above, except `intent` when there are no acceptance criteria. A listed `intent` agent without criteria is skipped and noted under Unverified Areas.

Every agent is the ordinary reviewer:
- It has the same tools (execution tools in `review`, read-only tools in `--read-only`), the same system instructions and the same untrusted-data rules.
- It has the same budgets: `reviewer.max_iterations`, `reviewer.timeout_seconds` and `reviewer.max_input_bytes` apply **to each agent**.
- A focus paragraph is added to its instructions. It tells the agent to stay within its focus and to report a finding outside it only when it is severe.

## Splitting a large change

When the change has more than `partition_files` changed files (default 15; `0` never splits), the `correctness` agent is replaced by up to 4 agents: `correctness-1`, `correctness-2`, and so on.
- The files are sorted by path, so a directory stays together, and cut into groups of about the same number of changed lines.
- Each agent owns one group. Its input carries the hunks of its own files only, and it can still read any file with the tools.
- The other specialists always examine the whole change.

## Division of work

- **Linter signals.** The first agent alone assesses them (the plain-language reading, see [the reviewer](../README.md#optional-ai-investigation)) and proposes [knowledge](../README.md#codebase-knowledge-base) updates. Other agents receive the signals as input but do not assess them, so this work is not done several times.
- **Experiments.** All agents share one sandbox harness. Their experiments run one at a time, draw on the **same** `reviewer.max_generated_tests` budget, and get unique evidence IDs. Only the model conversations run in parallel, at most `max_parallel` at a time (default 3, at most 8).
- **Deadlines.** A `--deadline` and the reviewer timeout bound the swarm as a whole.

## Merging

Merging uses no model. It is deterministic, in agent order:

- **Hypotheses** are gathered one after another. When two agents submit the same finding (same path, line and title, ignoring case and spacing), it is kept once, with the stronger claimed status: REPRODUCED > INTENT_TEST_FAILED > DIVERGED > NOT_REPRODUCED > DISMISSED > UNVERIFIED. Every agent that submitted it is credited. IDs are renumbered `hypothesis-1`, `hypothesis-2`, ….
- **Evidence is checked afterwards.** Every merged hypothesis is validated against the evidence afterwards, as any hypothesis is. A claim is kept only if the harness evidence supports it, whichever agent made it and however many agents agreed. **Agreement between agents is not evidence**, and a swarm never produces exit code 1 without a differential test.
- **Summaries** are joined as `[agent] summary` (at most 12,000 bytes), and each agent's "Unverified" notes are prefixed with its name.
- **If an agent fails** (provider error, timeout), the others' work is kept. The agent is recorded as `incomplete` and the failure is listed under Unverified Areas. The reviewer counts as incomplete only when every agent failed.

## What the report records

- `reviewer_agents`: each agent's name, focus, owned files (when split), status (`completed` or `incomplete`), number of hypotheses submitted before merging, and a note.
- `hypotheses[].agents`: the agents credited with each finding.
- `audit[].agent`: the agent behind each reviewer call (provider completions and local tools). Sandbox executions are audited by the harness, as before.
- In `CONFIDENCE_REPORT.md`, a **Reviewer Swarm** section lists the agents, and each finding in the Investigation Summary names its agents.

These fields are attribution, not evidence. They appear only when a swarm ran, so reports without a swarm are unchanged.

## Configuration and cost

| Setting | Where | Default | Meaning |
| --- | --- | --- | --- |
| `reviewer.swarm` | trusted policy | absent (one reviewer) | Present: a swarm runs whenever the reviewer runs. |
| `reviewer.swarm.agents` | trusted policy | all | Agents to run, in order (at most 12). |
| `reviewer.swarm.max_parallel` | trusted policy | 3 | Agents investigating at the same time (1..8). |
| `reviewer.swarm.partition_files` | trusted policy | 15 | Split `correctness` for changes with more changed files (0..1000; 0 never splits). |
| `--swarm` / `--swarm=false` | `review` | policy | Turn the swarm on (default agents) or off for this run. |
| `--swarm-agents LIST` | `review` | — | Agents to run for this run; implies `--swarm`. |
| `PROBE_HUB_SWARM` | [Probe Hub](../../hub/README.md) | off | `true` for the default agents or a list of agents; applies to every AI review the hub runs. |

- **Cost.** Provider traffic grows with the number of agents: five agents can cost up to five times a single reviewer. Choose the agents that matter for the repository, and lower `max_iterations` if needed.
- **Mode restrictions.** Lint never runs a reviewer, so `--swarm` and `--swarm-agents` are refused on `lint`, and with `--reviewer=false`.
- **Read-only review.** It ignores the policy's reviewer settings, including `reviewer.swarm`: in read-only review only the operator enables a swarm, with the flags or `PROBE_HUB_SWARM`.
- **Older binaries.** A policy with `reviewer.swarm` is rejected (exit 3) by binaries that predate the key.

## Limits

- Specialization narrows attention; it does not guarantee coverage. An agent can miss what is in its focus, and a finding outside every focus may not be investigated at all.
- Deduplication is exact on path, line and normalized title. Two agents describing one defect differently produce two findings.
- A swarm finds more candidate issues. Only the evidence rules decide what is reproduced. An UNVERIFIED finding from several agents is still unverified.
