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
| `changes` | Up to 12 areas, one per purpose of the change ("Add agent sorting", "Test agent sorting"), each with a summary, the code it covers (`refs`), and the linter signals (`signal_ids`) and hypotheses (`hypothesis_ids`) that point at that code. Unknown IDs are dropped, an ID belongs to the first area that cites it, and signals and hypotheses no area cites stay ungrouped. |
| `behavior_changes` | Up to 8 statements (`text`, `refs`) of what behaves differently for users, callers or operators, with the code that causes it. |
| `risks` | Up to 8 risks (`text`, `severity`, `signal_ids`, `hypothesis_ids`, `refs`), most severe first. `severity` (`low` to `critical`) is the model's estimate of how serious the risk would be if real, raised by Probe to the highest severity of the findings the risk cites or that sit on the lines it cites; it is a reading, not evidence, and changes no exit code. A risk the review recorded cites its linter signals and hypotheses by ID, and the renderings show the recorded status of each finding it cites (the model is told not to write a status itself); a risk the diff shows cites the code. A risk citing neither is dropped. The review's own state (checks run, budget, unverified areas, verdict) is not restated: the report holds it. |
| `review_focus` | Up to 8 pointers (`text`, `severity`, `refs`) to where a human reviewer should look first, each with a quoted citation, most severe first. `severity` is rated like a risk's: the model's estimate, raised by Probe to the most severe signal or hypothesis on the cited lines. |
| `testing` | Up to 4 statements (`text`, `refs`) on the tests the change adds or modifies. What Probe executed is never model output: the renderings state it from the report's checks. |
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

The areas make the summary and the computed findings one report: `CONFIDENCE_REPORT.md` and `PR_SUMMARY.md` list, under each area of **Changes**, the signals and hypotheses it cites with their recorded status, severity, title and location, and [Probe Hub](../../hub/README.md) lays its alert list out by area. An alert no area cites but a risk does unfolds whole under that risk, and only the alerts the summary cites nowhere come last under **Other alerts**: no alert is shown twice, and none is left out. The model only groups and describes: every alert keeps what Probe recorded, the severity filter still applies, and the grouping changes no severity, status or exit code.

`PR_SUMMARY.md` renders these fields as a title, an overview and the sections Changes, Behavior changes, Risks, Where to look first and Testing. Each statement ends with the code it cites as `path:start-end`, and a risk with the location and status of the findings it cites; the Testing section ends with the checks Probe executed, counted from the report. It ends with a line naming the model and the review verdict, and the marker `<!-- probe:pr-summary v1 -->`.

## How it is written

- **When.** The summary is written **after** the review is finalized: the checks, experiments, investigation (single reviewer or [swarm](SWARM.md)), evidence validation and the exit code are all settled first. The summary can therefore describe what the review found, and it cannot change it. It changes no status, severity, review target or exit code.
- **One call, one correction.** It is one Chat Completions request to the configured reviewer provider, with no tools, the reviewer's timeout and up to 8192 output tokens. The answer is first **repaired** without the model: Probe reads the first JSON object around any prose or code fence, escapes the newlines and the backslashes that quoted code often leaves invalid, drops trailing commas, and reads line numbers written as strings or refs written as `"path:12-14"`; an answer cut off by the token limit is cut back to its last complete member and closed. Only what still fails is sent back to the model once, with the reason: an invalid answer, citations that did not match the diff, or a cut-off answer (then without the cut-off text, asking for a shorter one). When the correction would not fit `reviewer.max_input_bytes` with the previous answer, it is sent without it. A valid first answer stands if the correction fails. Each completion is audited as `pr_summary_completion`.
- **Input.** The sanitized report: the intent and acceptance criteria (including those from [Jira](JIRA.md), [Linear](LINEAR.md), [Notion](NOTION.md) or [Google Docs](GOOGLE_DOCS.md)), the commit messages of the change (at most 30), the changed files with their hunks, and the finalized review (verdict, reproduced issues, findings with their final status, review targets, unverified areas, checks, linter signals, the reviewer's closing text). When the hunks would not fit `reviewer.max_input_bytes`, measured as the request carries them (escaped inside a JSON string) with the prompt and some room to spare, only the file list is sent.
- **Untrusted text.** The input is data, never instructions. The answer is redacted like every report string, bounded field by field, and rendered as plain text: links are broken and Markdown and HTML are escaped. The only links are those the renderers build from validated refs and IDs.
- **Failures.** If the provider fails or no valid summary comes out, Probe prints `Pull request summary not written: …`, records the reason as a `pr_summary` audit event with status `ERROR`, and writes the report without the summary; [Probe Hub](../../hub/README.md) shows that reason in place of the AI report. A missing summary is never a review outcome.

## Configuration

| Setting | Default | Effect |
| --- | --- | --- |
| `--pr-summary` | on when the reviewer runs | `--pr-summary=false` skips the extra provider call. An explicit `--pr-summary` exits 3 on `lint` and with `--reviewer=false`, because only the reviewer model writes it. |
| `--format pr-summary` | written with `markdown` when a summary exists | Writes `PR_SUMMARY.md` even without `markdown`. Without a summary it states that none was generated. |

The summary uses the reviewer's provider settings (`reviewer.model` or `PROBE_REVIEWER_MODEL`, endpoint, credential, timeout) and costs one or two completions per review. [Probe Hub](../../hub/README.md) shows it as the report: its title, overview, behavior changes, risks, review focus and testing above the alerts, and its change areas heading the alert list. Each cited ref is a link that unfolds the diff of that file under the statement, with the cited lines highlighted; a checked citation is marked `✓` and shows its quote on hover; a risk links to the alerts it cites, which open in the alert list. A button copies the summary as Markdown, and its [MCP](../../hub/README.md#mcp-server-for-coding-agents) `get_findings` tool returns it.

## What it is not

The summary is model output. It can be wrong, incomplete or out of date with the code, and a well-written summary is no sign of a good change. It is not an approval: the prompt forbids claiming the change is correct, safe or tested beyond what the review executed, and the report's verdict always comes from evidence. Probe publishes nothing. Posting `PR_SUMMARY.md` to a pull request is a CI decision; treat it like any artifact of a pull-request run (see [CI integration](CI.md)).
