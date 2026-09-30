# Linear issues as review context

`--linear` does for [Linear](https://linear.app) what [`--jira`](JIRA.md) does for Jira: Probe reads the issue a change implements and uses it as the intent of the change, so that `review`, `lint` and `plan` compare the implementation with what was asked.

```sh
export PROBE_LINEAR_API_KEY=lin_api_...          # Settings → Security & access → Personal API keys

probe review --base main --linear ENG-123        # a given issue
probe review --base main --linear auto           # the issue named by the branch or the commits
probe review --base main --linear auto --intent-file PR.md   # the issue, then the PR description
probe plan --linear ENG-123                      # plan an issue before writing code
```

## What the issue becomes

Probe reads the issue's identifier, title, description, status, priority, team, project, parent issue and labels through Linear's GraphQL API, and renders them as:

```markdown
Linear issue ENG-123: Orders of 100 or more get a discount

Team: Checkout · Project: Pricing · Status: In Progress · Priority: High · Parent: ENG-100: Discounts · Labels: backend · Link: https://linear.app/acme/issue/ENG-123

## Description

...the description, as Markdown, its headings nested two levels down...
```

That text is the intent, and everything [intent criteria](INTENT.md) describes applies unchanged:

- Linear descriptions are already Markdown. Their list and checklist items become the criteria `AC-1`, `AC-2`, …; when the description has an "Acceptance criteria" heading, only the items under it count.
- The report records the rendered text as `intent` (redacted like any intent) with its SHA-256; the first line and the `Link:` line identify the issue.
- The reviewer receives the intent and may write candidate-only intent tests; `probe plan` simulates the issue.
- `--intent` / `--intent-file` follow the issue in the same text, and `--jira` may be combined too (Jira first, then Linear, then the explicit intent). Together they are at most 64 KiB; an issue that does not fit is cut on a character boundary and ends with `[Probe truncated the Linear issue to fit the 64 KiB intent limit.]`.

## Choosing the issue

`--linear ENG-123` takes an issue identifier: a team key in capitals, a hyphen, a number without leading zero. Anything else exits 3 before Git analysis.

`--linear auto` searches, in this order, the branch given as `--head` (or the checked-out branch when `--head` is `HEAD`), the CI branch variables `GITHUB_HEAD_REF`, `CI_MERGE_REQUEST_SOURCE_BRANCH_NAME`, `CI_COMMIT_REF_NAME`, `BITBUCKET_BRANCH`, `BRANCH_NAME` and `GIT_BRANCH`, then the messages of the commits under review, oldest first (`plan` has no commits yet). Unlike Jira, matching ignores case, because the branch names Linear suggests are lower case (`ana/eng-123-fix-login`).

Case-insensitive matching finds false candidates such as `hotfix-2`, so:

- tokens such as `UTF-8`, `SHA-256`, `v-2`, `release-2`, `feature-12`, `fix-3` or `rc-1` are never candidates;
- `PROBE_LINEAR_TEAMS=ENG,WEB` keeps only those team keys;
- the first five distinct candidates are tried in order, and one Linear does not know is skipped.

When no candidate is an issue, Probe prints `Linear: no issue found from the branch name or commit messages; continuing without a Linear issue.` and continues with the explicit intent alone; `plan` then needs another intent, else it exits 3. An identifier given explicitly must exist.

## Configuration

Linear is configured by the operator's environment, never by the repository: a pull request cannot redirect the request or the credential.

| Variable | Meaning |
| --- | --- |
| `PROBE_LINEAR_API_KEY` | Required by `--linear`. A personal API key (`lin_api_…`, sent as is) or an OAuth access token (sent as `Bearer`). Also read from `PROBE_LINEAR_API_KEY_FILE`, then from the Docker secret `/run/secrets/PROBE_LINEAR_API_KEY`. A read-only key is enough. |
| `PROBE_LINEAR_TEAMS` | Optional comma-separated team keys `--linear auto` accepts. |
| `PROBE_LINEAR_URL` | GraphQL endpoint, default `https://api.linear.app/graphql` (a proxy or a test server). Must be `https`. |
| `PROBE_LINEAR_ALLOW_INSECURE_HTTP` | `true` permits an `http` endpoint (a local test server). |

Requests never follow redirects, so the key only goes to the configured endpoint. Responses are limited to 4 MiB and 30 seconds.

## Failures

Every Linear failure exits 3 with `probe: linear: …` before any container starts and before any provider call: `--linear` without an API key, an unusable variable or secret, an explicit identifier Linear does not know or the key cannot see (`issue not found or not visible to the configured API key`), HTTP 401/403 or an authentication error (`check PROBE_LINEAR_API_KEY`), other API errors and a response that is not an issue. On success Probe prints `Linear: ENG-123 "title" joins the intent.` on standard error.

## Trust

- The issue is untrusted text, exactly like a pull request description: it goes through the same UTF-8 check, PR-comment removal, redaction and criteria extraction, and no command, image, network setting, budget or provider setting is derived from it.
- `lint` contacts Linear only with `--linear`; it still never calls a provider and never executes repository code.
- The issue says what was asked, not what is right. A mismatch the reviewer reports is a hypothesis for a human, not a reproduced issue.

## See also

- [Notion pages as review context](NOTION.md): product, architecture or requirements pages.
- [Jira issues as review context](JIRA.md).
- [Intent criteria and candidate-only intent tests](INTENT.md).
- [Plans and scope drift](PLAN.md).
