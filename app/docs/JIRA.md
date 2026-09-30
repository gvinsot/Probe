# Jira issues as review context

A change usually implements a ticket. `--jira` makes Probe read that ticket and use it as the intent of the change, so that `review`, `lint` and `plan` compare the implementation with what was asked, without anyone pasting the ticket into `--intent-file`.

```sh
export PROBE_JIRA_URL=https://acme.atlassian.net
export PROBE_JIRA_EMAIL=ci-bot@acme.com         # Jira Cloud only
export PROBE_JIRA_TOKEN=...                     # API token (Cloud) or personal access token (Server/DC)

probe review --base main --jira SHOP-123        # a given issue
probe review --base main --jira auto            # the issue named by the branch or the commits
probe review --base main --jira auto --intent-file PR.md   # the issue, then the PR description
probe plan --jira SHOP-123                      # plan an issue before writing code
```

## What the issue becomes

Probe reads the summary, description, type, status and labels of the issue, and optionally an acceptance-criteria custom field, and renders them as Markdown:

```markdown
Jira issue SHOP-123: Orders of 100 or more get a discount

Type: Story · Status: In Progress · Labels: checkout · Link: https://acme.atlassian.net/browse/SHOP-123

## Description

...the description, its headings nested two levels down...

## Acceptance criteria

...the configured criteria field, when there is one...
```

That text is the intent. Everything [intent criteria](INTENT.md) describes then applies unchanged:

- The list items of an "Acceptance criteria" section become `AC-1`, `AC-2`, … (without such a section, every list item of the issue counts). When `PROBE_JIRA_CRITERIA_FIELD` names a field, "acceptance criteria" headings inside the description are renamed to "criteria", so the field is the only source of criteria.
- The report records the rendered text as `intent` (redacted like any intent) and its SHA-256, so the ticket as it was at review time stays in the report. The first line and the `Link:` line identify the issue.
- The reviewer receives the intent and may write candidate-only intent tests; `probe plan` simulates the issue.
- With `--intent` or `--intent-file`, the explicit intent follows the issue in the same text. Together they are at most 64 KiB: a longer issue is cut on a character boundary and ends with `[Probe truncated the Jira issue to fit the 64 KiB intent limit.]`.

Rich text is converted as follows: Jira Cloud descriptions (Atlassian Document Format, REST API v3) keep their headings, bullet, numbered and task lists (nested by two spaces, `[x]` for done tasks), code blocks and tables; mentions, emoji, status lozenges and links become their text; media is dropped. Jira Server and Data Center descriptions (wiki markup, REST API v2) keep `h1.`–`h6.` headings, `*`/`#`/`-` lists, `{code}`/`{noformat}` blocks and `[text|url]` links as `text (url)`; other markup stays as text.

## Choosing the issue

`--jira KEY` takes an issue key such as `SHOP-123` (project key in capitals, a hyphen, a number without leading zero). Anything else exits 3 before Git analysis.

`--jira auto` looks for the first issue key, in this order, in:

1. `--head` when it is a branch name rather than `HEAD` (`feature/SHOP-123-discounts`);
2. the checked-out branch, when `--head` is `HEAD`;
3. the CI branch variables `GITHUB_HEAD_REF`, `CI_MERGE_REQUEST_SOURCE_BRANCH_NAME`, `CI_COMMIT_REF_NAME`, `BITBUCKET_BRANCH`, `BRANCH_NAME` and `GIT_BRANCH` (a CI checkout is usually detached);
4. the messages of the commits under review, oldest first (at most 200). `plan` has no commits yet and stops at step 3.

A key must stand alone (`XSHOP-1` and `SHOP-1a` do not match), and tokens such as `UTF-8`, `SHA-256`, `ISO-8601`, `RFC-7231` or `CVE-2024` are not taken as keys. When no key is found, Probe prints `Jira: no issue key found in the branch name or commit messages; continuing without a Jira issue.` and continues with the explicit intent alone; `plan` then needs `--intent` or `--intent-file`, else it exits 3.

## Configuration

Jira is configured by the operator's environment, never by the repository: a pull request cannot redirect the request or the credential.

| Variable | Meaning |
| --- | --- |
| `PROBE_JIRA_URL` | Site root, e.g. `https://acme.atlassian.net` or `https://jira.acme.com/jira`. Required by `--jira`. Must be `https` without credentials, query or fragment. |
| `PROBE_JIRA_EMAIL` | Jira Cloud account. With it, the token is sent as HTTP basic authentication (`email:api-token`). |
| `PROBE_JIRA_TOKEN` | API token (Cloud, with `PROBE_JIRA_EMAIL`) or personal access token (Server and Data Center, sent as `Bearer`). Also read from `PROBE_JIRA_TOKEN_FILE`, then from the Docker secret `/run/secrets/PROBE_JIRA_TOKEN`. Without a token the request is anonymous. |
| `PROBE_JIRA_CRITERIA_FIELD` | Optional custom field holding acceptance criteria, e.g. `customfield_10035`. Rich text, plain text and lists of strings are read. |
| `PROBE_JIRA_ALLOW_INSECURE_HTTP` | `true` permits an `http` site (a local test server). |

Probe first requests `/rest/api/3/issue/KEY` and, when the site answers 404, `/rest/api/2/issue/KEY`, so Cloud and Server/Data Center need no setting. Redirects to another origin are refused, so the credential only goes to the configured site. Responses are limited to 4 MiB and 30 seconds.

## Failures

Every Jira failure exits 3 with `probe: jira: …` before any container starts and before any provider call: `--jira` without `PROBE_JIRA_URL`, an unusable variable or secret, an issue that does not exist or is not visible to the account (`issue not found or not visible to the configured account`), HTTP 401/403 (`check PROBE_JIRA_EMAIL and PROBE_JIRA_TOKEN`), other HTTP errors, and a response that is not a JSON issue. An issue key is resolved after the Git comparison (auto-detection reads the branch and commits), so an invalid comparison still exits 3 first.

On success Probe prints `Jira: SHOP-123 "summary" joins the intent.` on standard error.

## Trust

- The issue is untrusted text, exactly like a pull request description: it goes through the same UTF-8 check, PR-comment removal, redaction and criteria extraction, and no command, image, network setting, budget or provider setting is derived from it. The reviewer prompt treats criterion text as data, never as instructions.
- `lint` contacts Jira only with `--jira`; it still never calls a provider and never executes repository code.
- The ticket says what was asked, not what is right. A change that matches its ticket can still be wrong, and a mismatch reported by the reviewer is a hypothesis for a human, not a reproduced issue.

## See also

- [Notion pages as review context](NOTION.md): product, architecture or requirements pages.
- [Linear issues as review context](LINEAR.md): the same for Linear; `--jira` and `--linear` can be combined.
- [Intent criteria and candidate-only intent tests](INTENT.md).
- [Plans and scope drift](PLAN.md).
- [CI integration](CI.md) and [security boundaries](SECURITY.md).
