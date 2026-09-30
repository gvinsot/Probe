# Google Docs as review context

`--gdoc` gives Probe the design and requirement documents a change implements, written in Google Docs. `review`, `lint` and `plan` then read the code change against them. The documents become part of the intent, like [Notion pages](NOTION.md) and [Jira](JIRA.md) or [Linear](LINEAR.md) issues.

This is not the desktop application. [Probe Desktop](../../desktop/README.md) compares versions of Office documents. Here a document is not what is reviewed: it is context for understanding a code change.

```sh
export PROBE_GOOGLE_CREDENTIALS=/secure/probe-reader.json   # a service-account key

probe review --base main --gdoc https://docs.google.com/document/d/1AbC…xyz/edit
probe review --base main --jira SHOP-123 --gdoc 1AbC…xyz,1DeF…uvw
probe plan --gdoc https://docs.google.com/document/d/1AbC…xyz/edit
```

## Credentials

Probe uses the Google Drive API (`files.get` and `files.export`) with read-only access. It takes the first credential available:

| Credential | Variables | Reads |
| --- | --- | --- |
| OAuth access token | `PROBE_GOOGLE_ACCESS_TOKEN`, or its `_FILE`, or `/run/secrets/PROBE_GOOGLE_ACCESS_TOKEN` | Documents the token's user or account can read. Useful with `gcloud auth print-access-token` or workload identity federation in CI (scope `drive.readonly`). The token is not refreshed. |
| Service-account key (JSON) | `PROBE_GOOGLE_CREDENTIALS` (a file path), else `/run/secrets/PROBE_GOOGLE_CREDENTIALS`, else `GOOGLE_APPLICATION_CREDENTIALS` | Documents shared with the service account's email. Probe signs a JWT with the key and exchanges it for a one-hour `https://www.googleapis.com/auth/drive.readonly` token. |
| API key | `PROBE_GOOGLE_API_KEY`, or its `_FILE`, or Docker secret | Only documents shared publicly ("Anyone with the link"). |

For a service account: create it in a Google Cloud project with the Drive API enabled, create a JSON key, and share each document (or its shared drive) with the account's email as a viewer. It needs no IAM role.

The token endpoint is `https://oauth2.googleapis.com/token` and the API root is `https://www.googleapis.com`. `PROBE_GOOGLE_TOKEN_URL` and `PROBE_GOOGLE_API_URL` replace them (a proxy or a test server), and `PROBE_GOOGLE_ALLOW_INSECURE_HTTP=true` permits `http` for a local test server. The `token_uri` inside a key file is ignored, so a key file cannot send its signed assertion anywhere else. No credential ever comes from the repository.

## Choosing documents

`--gdoc` takes up to 5 comma-separated documents. Each one is a Google Docs URL (`https://docs.google.com/document/d/ID/…`), a Drive URL (`https://drive.google.com/file/d/ID/…`, `https://drive.google.com/open?id=ID`) or a bare file ID. Duplicates count once. Anything else exits 3 before Git analysis. Only Google Docs are read. A Sheets, Slides, PDF or Word file is refused with `not a Google Doc`; convert it to Google Docs first.

There is no `auto` mode, because branch names and commits do not name documents.

## What a document becomes

Drive exports the document as Markdown, which keeps headings, lists, tables, links and code. Images are embedded in the export as base64 data; Probe removes them and keeps `[image]` or `[image: alt text]`. If Drive refuses the Markdown export, Probe falls back to the plain-text export. The result is:

```markdown
Google Doc: Checkout design

Last modified: 2026-09-01T10:00:00Z · Link: https://docs.google.com/document/d/…/edit

## Document content

### Checkout design

Prices are integer cents.

• Services talk over gRPC

#### Acceptance criteria

* Totals never use floats
```

- A design document is context, not a ticket, so **its list items are not acceptance criteria**. They are rendered as prose (`•`, `(1)`), except the items under one of the document's own headings that contains "Acceptance criteria". Those stay list items and become `AC-1`, `AC-2`, …, as [intent criteria](INTENT.md) describes.
- Headings are nested two levels down under `## Document content`.
- The order in the intent is: the Jira issue, the Linear issue, the Notion pages, the Google Docs in the order given, then `--intent` / `--intent-file`. Together they are at most 64 KiB. A document that does not fit whole is cut on a character boundary and ends with `[Probe truncated the Google document to fit the 64 KiB intent limit.]`; a document that does not fit at all is left out, with a message. A single export is limited to 4 MiB.
- The report records the rendered text as `intent` (redacted like any intent) with its SHA-256, so the documents as they were at review time stay in the report.

On success Probe prints `Google Docs: document "title" joins the intent.` on standard error for each document.

## Failures

Every Google failure exits 3 with `probe: gdoc: …` before any container starts and before any provider call:

- an invalid document reference, or more than 5 documents;
- `--gdoc` without a credential, or an unusable variable, key file or secret;
- a service-account token the endpoint refuses;
- a document that does not exist or the credential cannot read (`document not found or not shared with the configured Google account`);
- a file that is not a Google Doc;
- HTTP 401 or 403 (for example, the Drive API is not enabled in the project), and other API errors.

Requests never follow redirects and time out after 30 seconds.

## Trust

- A document is untrusted text, exactly like a pull request description: it goes through the same UTF-8 check, PR-comment removal, redaction and criteria extraction, and no command, image, network setting, budget or provider setting is derived from it.
- `lint` contacts Google only with `--gdoc`; it still never calls a provider and never executes repository code.
- A document states an intention, and it may be out of date: the last modification time is part of the intent. A mismatch the reviewer reports is a hypothesis for a human, not a reproduced issue.

## See also

- [Notion pages](NOTION.md), [Jira issues](JIRA.md) and [Linear issues](LINEAR.md) as review context.
- [Intent criteria and candidate-only intent tests](INTENT.md).
- [Plans and scope drift](PLAN.md).
