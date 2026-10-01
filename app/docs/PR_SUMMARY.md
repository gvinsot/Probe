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
| `changes` | Up to 12 areas, each with a summary and the code it covers (`refs`). |
| `behavior_changes` | Up to 8 statements (`text`, `refs`) of what behaves differently for users, callers or operators, with the code that causes it. |
| `risks` | Up to 8 risks (`text`, `signal_ids`, `hypothesis_ids`, `refs`). A risk the review recorded cites its linter signals and hypotheses by ID, and is called unverified unless it was reproduced; a risk the diff shows cites the code. A risk citing neither is dropped. The review's own state (checks run, budget, unverified areas, verdict) is not restated: the report holds it. |
| `review_focus` | Up to 8 pointers (`text`, `refs`) to where a human reviewer should look first, each with a quoted citation. |
| `testing` | Up to 4 statements (`text`, `refs`) on the tests the change adds or modifies. What Probe executed is never model output: the renderings state it from the report's checks. |
| `intents` | Up to 12 developer intentions ("Add agent sorting", "Test agent sorting"), each citing the linter signals (`signal_ids`) and hypotheses (`hypothesis_ids`) it explains. Unknown IDs are dropped, an ID belongs to the first intent that cites it, and an intent left empty is dropped. Signals and hypotheses no intent cites stay ungrouped. |
| `model` | The model that wrote it. |
| `rejected_citations` | How many refs were dropped because their quote is not in the diff; absent when none. |

A ref (`refs`) is `{path, start_line, end_line, side, quote}`: a file the change touched, and optionally lines of it, on the new side unless `side` is `old`. The model is asked to cite code rather than write paths in its sentences. Probe checks every ref against the diff: a file outside the change is dropped, and at most 6 refs per statement are kept.

### Quoted citations

To catch the most common hallucinations, wrong line numbers and invented code, the model quotes the code it cites: `quote` is a short, verbatim piece (one to three lines, at most 200 characters) of the cited lines. Probe checks it deterministically, without a model:

- the quote is searched, whitespace collapsed, in the file's hunk lines on the ref's side, within one hunk;
- found, the ref takes the lines of the occurrence nearest to the lines the model gave, and keeps `quote`: those lines are where the code really is;
- not found, or too short to identify code (fewer than 8 characters, or no letter or digit), or longer than 300 characters, the ref is dropped and counted in `rejected_citations`.

A ref without a quote is kept, with lines clamped to the hunks they overlap (lines the diff does not show leave a reference to the whole file), but its lines are unchecked. A `review_focus` point needs at least one quoted ref, and a risk citing no recorded ID needs one too; otherwise the statement is dropped.

When a valid answer loses citations or statements this way, the reasons are sent back once ("the quote … is not in the new lines of auth.go") and the model answers again; if that second answer fails, the first stands.

A checked citation proves that the cited code is at those lines, not that what the summary says about it is right: the statement remains model output. The renderings say so: `✓` marks a checked citation in `PR_SUMMARY.md`, `CONFIDENCE_REPORT.md` and the hub, next to a note on the citations dropped.

`CONFIDENCE_REPORT.md` lists the intents under **Findings by intent**, each with the titles and locations of the signals and hypotheses it cites; [Probe Hub](../../hub/README.md) groups its alert list the same way, with the uncited alerts last under **Other alerts**. The grouping is a reading aid: it changes no severity, status or exit code.

`PR_SUMMARY.md` renders these fields as a title, an overview and the sections Changes, Behavior changes, Risks, Where to look first and Testing. Each statement ends with the code it cites as `path:start-end`, and a risk with the location and status of the findings it cites; the Testing section ends with the checks Probe executed, counted from the report. It ends with a line naming the model and the review verdict, and the marker `<!-- probe:pr-summary v1 -->`.

## How it is written

- **When.** The summary is written **after** the review is finalized: the checks, experiments, investigation (single reviewer or [swarm](SWARM.md)), evidence validation and the exit code are all settled first. The summary can therefore describe what the review found, and it cannot change it. It changes no status, severity, review target or exit code.
- **One call.** It is one Chat Completions request to the configured reviewer provider, with no tools and the reviewer's timeout. If the answer is not valid JSON, or if citations were dropped, it is retried once with the reasons. Both completions are audited as `pr_summary_completion`.
- **Input.** The sanitized report: the intent and acceptance criteria (including those from [Jira](JIRA.md), [Linear](LINEAR.md), [Notion](NOTION.md) or [Google Docs](GOOGLE_DOCS.md)), the commit messages of the change (at most 30), the changed files with their hunks, and the finalized review (verdict, reproduced issues, findings with their final status, review targets, unverified areas, checks, linter signals, the reviewer's closing text). When the hunks would take more than half of `reviewer.max_input_bytes`, only the file list is sent.
- **Untrusted text.** The input is data, never instructions. The answer is redacted like every report string, bounded field by field, and rendered as plain text: links are broken and Markdown and HTML are escaped. The only links are those the renderers build from validated refs and IDs.
- **Failures.** If the provider fails or returns an invalid summary twice, Probe prints `Pull request summary not written: …` and writes the report without it. A missing summary is never a review outcome.

## Configuration

| Setting | Default | Effect |
| --- | --- | --- |
| `--pr-summary` | on when the reviewer runs | `--pr-summary=false` skips the extra provider call. An explicit `--pr-summary` exits 3 on `lint` and with `--reviewer=false`, because only the reviewer model writes it. |
| `--format pr-summary` | written with `markdown` when a summary exists | Writes `PR_SUMMARY.md` even without `markdown`. Without a summary it states that none was generated. |

The summary uses the reviewer's provider settings (`reviewer.model` or `PROBE_REVIEWER_MODEL`, endpoint, credential, timeout) and costs one or two completions per review. [Probe Hub](../../hub/README.md) shows it as a folded **AI pull request summary** above each report. Each cited ref is a link that unfolds the diff of that file under the statement, with the cited lines highlighted; a checked citation is marked `✓` and shows its quote on hover; a risk links to the alerts it cites, which open in the alert list. A button copies the summary as Markdown, and its [MCP](../../hub/README.md#mcp-server-for-coding-agents) `get_findings` tool returns it.

## What it is not

The summary is model output. It can be wrong, incomplete or out of date with the code, and a well-written summary is no sign of a good change. It is not an approval: the prompt forbids claiming the change is correct, safe or tested beyond what the review executed, and the report's verdict always comes from evidence. Probe publishes nothing. Posting `PR_SUMMARY.md` to a pull request is a CI decision; treat it like any artifact of a pull-request run (see [CI integration](CI.md)).
