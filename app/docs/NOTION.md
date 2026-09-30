# Notion pages as review context

`--notion` adds [Notion](https://www.notion.so) pages (product notes, architecture decisions, requirements) to the intent of a change, the way [`--jira`](JIRA.md) and [`--linear`](LINEAR.md) add a ticket. `review`, `lint` and `plan` then compare the implementation with what those pages say.

```sh
export PROBE_NOTION_TOKEN=ntn_...                 # the secret of a Notion integration

probe review --base main --notion https://www.notion.so/acme/Checkout-architecture-0123456789abcdef0123456789abcdef
probe review --base main --jira SHOP-123 --notion 0123456789abcdef0123456789abcdef,fedcba9876543210fedcba9876543210
probe plan --notion https://www.notion.so/acme/Refund-requirements-0123456789abcdef0123456789abcdef
```

## Setup

1. In Notion, create an internal integration (Settings → Connections → Develop or manage integrations) with the **Read content** capability only.
2. Share each page Probe should read with the integration (the page's `•••` menu → Connections). A page that is not shared is reported as not found.
3. Give Probe the integration secret in `PROBE_NOTION_TOKEN`.

## Choosing pages

`--notion` takes up to 5 comma-separated pages, each as a page URL (`https://www.notion.so/…`, `https://<workspace>.notion.site/…`) or a page ID with or without dashes. Probe uses the last page ID in a URL's path and ignores the query. Duplicates count once. Anything else exits 3 before Git analysis.

There is no `auto` mode: branch names and commits do not name Notion pages. To read a page for every review, add the flag to your CI command.

## What a page becomes

Probe reads the page's title, link and last edit time, then its blocks in order, including nested blocks, and renders them as Markdown:

```markdown
Notion page: Checkout architecture

Last edited: 2026-09-01T10:00:00.000Z · Link: https://www.notion.so/…

## Page content

Prices are integer cents.

• Services talk over gRPC
  • mTLS inside the cluster

### Acceptance criteria

- [ ] Totals never use floats
```

- Headings, paragraphs, bulleted, numbered and to-do lists, toggles, quotes, callouts, code blocks, equations, tables, columns and synced blocks are read. Bookmarks and embeds become their URL, media their caption. Sub-pages and databases are named (`Sub-page: Runbook`), not read: name them with `--notion` to add them.
- A page is context, not a ticket, so **its list items are not acceptance criteria**. They are rendered as prose (`•`, `(1)`), except the items under one of the page's own headings that contains "Acceptance criteria". Those stay list items and become `AC-1`, `AC-2`, …, as [intent criteria](INTENT.md) describes. An architecture page therefore informs the reviewer without adding criteria, and a requirements page can still state them.
- Headings are nested two levels down under `## Page content`.
- The order in the intent is: the Jira issue, the Linear issue, the Notion pages in the order given, then `--intent` / `--intent-file`. Together they are at most 64 KiB. A page that does not fit whole is cut on a character boundary and ends with `[Probe truncated the Notion page to fit the 64 KiB intent limit.]`; a page that does not fit at all is left out, with a message.
- A page is read up to 2,000 blocks, 6 levels of nesting and 60 API requests. Beyond that the rendered page ends with `[Probe read only part of this Notion page: a size or nesting limit was reached.]`.
- The report records the rendered text as `intent` (redacted like any intent) with its SHA-256, so the pages as they were at review time stay in the report.

On success Probe prints `Notion: page "title" joins the intent.` on standard error for each page.

## Configuration

Notion is configured by the operator's environment, never by the repository: a pull request cannot redirect the request or the credential.

| Variable | Meaning |
| --- | --- |
| `PROBE_NOTION_TOKEN` | Required by `--notion`: the integration secret, sent as `Bearer`. Also read from `PROBE_NOTION_TOKEN_FILE`, then from the Docker secret `/run/secrets/PROBE_NOTION_TOKEN`. |
| `PROBE_NOTION_URL` | API root, default `https://api.notion.com` (a proxy or a test server). Must be `https`. |
| `PROBE_NOTION_ALLOW_INSECURE_HTTP` | `true` permits an `http` API root (a local test server). |

Requests use `Notion-Version: 2022-06-28`, never follow redirects, and are limited to 4 MiB per response and 30 seconds.

## Failures

Every Notion failure exits 3 with `probe: notion: …` before any container starts and before any provider call: an invalid page reference, more than 5 pages, `--notion` without a token, an unusable variable or secret, a page that does not exist or is not shared with the integration (`page not found or not shared with the Notion integration`), HTTP 401 (`check PROBE_NOTION_TOKEN`), other API errors and responses that are not a page.

## Trust

- A page is untrusted text, exactly like a pull request description: it goes through the same UTF-8 check, PR-comment removal, redaction and criteria extraction, and no command, image, network setting, budget or provider setting is derived from it.
- `lint` contacts Notion only with `--notion`; it still never calls a provider and never executes repository code.
- A page describes an intention, not what is right, and it may be out of date: the last edit time is part of the intent. A mismatch the reviewer reports is a hypothesis for a human, not a reproduced issue.

## See also

- [Google Docs as review context](GOOGLE_DOCS.md): design and requirement documents.
- [Jira issues](JIRA.md) and [Linear issues](LINEAR.md) as review context.
- [Intent criteria and candidate-only intent tests](INTENT.md).
- [Plans and scope drift](PLAN.md).
