# AI-written pull request summary

Probe's reports are built for verification: signals, checks, evidence and a verdict. A pull request also needs a **description** a person can read in a minute: what changed, why, what behaves differently, and where to look. When the AI reviewer runs, Probe now asks it for that narrative after the review, and writes it next to the report:

- `pr_summary` in `confidence-report.json`;
- a **Pull Request Summary** section at the top of `CONFIDENCE_REPORT.md`;
- `PR_SUMMARY.md`, a Markdown file ready to paste as the pull request description.

```sh
probe review --base main                      # summary included when the reviewer runs
probe review --read-only --base main          # also in read-only review
probe review --base main --pr-summary=false   # skip it
probe report --input .probe/confidence-report.json --format pr-summary   # re-render PR_SUMMARY.md
```

## What it contains

| Field | Content |
| --- | --- |
| `title` | A specific pull request title, in the imperative (at most 120 characters). |
| `overview` | Two to four sentences: what the change does and why, as far as the intent, the commit messages and the diff show it. |
| `changes` | Up to 12 areas, each with a summary and the changed files it covers. Paths that the change did not touch are dropped. |
| `behavior_changes` | Up to 8 statements of what behaves differently for users, callers or operators. |
| `risks` | Up to 8 risks: those the review recorded (reproduced issues, findings, review targets, unverified areas) or that the diff plainly shows. Anything the review did not reproduce is called unverified. |
| `review_focus` | Up to 8 pointers to where a human reviewer should look first. |
| `testing` | The tests the change adds or modifies and what the review executed; it says so when nothing was executed. |
| `model` | The model that wrote it. |

`PR_SUMMARY.md` renders these fields as a title, an overview and the sections Changes, Behavior changes, Risks, Where to look first and Testing. It ends with a line naming the model and the review verdict, and the marker `<!-- probe:pr-summary v1 -->`.

## How it is written

- **When.** The summary is written **after** the review is finalized: the checks, experiments, investigation (single reviewer or [swarm](SWARM.md)), evidence validation and the exit code are all settled first. The summary can therefore describe what the review found, and it cannot change it. It changes no status, severity, review target or exit code.
- **One call.** It is one Chat Completions request to the configured reviewer provider, with no tools and the reviewer's timeout. If the answer is not valid JSON, it is retried once with the reason. Both completions are audited as `pr_summary_completion`.
- **Input.** The sanitized report: the intent and acceptance criteria (including those from [Jira](JIRA.md), [Linear](LINEAR.md), [Notion](NOTION.md) or [Google Docs](GOOGLE_DOCS.md)), the commit messages of the change (at most 30), the changed files with their hunks, and the finalized review (verdict, reproduced issues, findings with their final status, review targets, unverified areas, checks, linter signals, the reviewer's closing text). When the hunks would take more than half of `reviewer.max_input_bytes`, only the file list is sent.
- **Untrusted text.** The input is data, never instructions. The answer is redacted like every report string, bounded field by field, and rendered as plain text: links are broken and Markdown and HTML are escaped.
- **Failures.** If the provider fails or returns an invalid summary twice, Probe prints `Pull request summary not written: …` and writes the report without it. A missing summary is never a review outcome.

## Configuration

| Setting | Default | Effect |
| --- | --- | --- |
| `--pr-summary` | on when the reviewer runs | `--pr-summary=false` skips the extra provider call. An explicit `--pr-summary` exits 3 on `lint` and with `--reviewer=false`, because only the reviewer model writes it. |
| `--format pr-summary` | written with `markdown` when a summary exists | Writes `PR_SUMMARY.md` even without `markdown`. Without a summary it states that none was generated. |

The summary uses the reviewer's provider settings (`reviewer.model` or `PROBE_REVIEWER_MODEL`, endpoint, credential, timeout) and costs one or two completions per review. [Probe Hub](../../hub/README.md) shows it as a folded **AI pull request summary** above each report, with a button that copies it as Markdown, and its [MCP](../../hub/README.md#mcp-server-for-coding-agents) `get_findings` tool returns it.

## What it is not

The summary is model output. It can be wrong, incomplete or out of date with the code, and a well-written summary is no sign of a good change. It is not an approval: the prompt forbids claiming the change is correct, safe or tested beyond what the review executed, and the report's verdict always comes from evidence. Probe publishes nothing. Posting `PR_SUMMARY.md` to a pull request is a CI decision; treat it like any artifact of a pull-request run (see [CI integration](CI.md)).
