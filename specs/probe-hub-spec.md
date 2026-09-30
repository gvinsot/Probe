# Probe Hub, website and deployment

**Status:** implemented source contract, checked against `23a1d2e` on
2026-09-27. The [CLI specification](probe-v0.4-spec.md) defines analysis
and evidence; this document covers the other application surfaces. It describes
repository configuration, not an inspection of the live deployment.

## 1. Architecture and responsibility

The Hub is a Go module with an embedded HTML/CSS/JavaScript UI, filesystem
state and an in-process worker queue. It runs the trusted Probe binary
shipped in its image; it does not recompute evidence statuses or approve code.
The promotional website is a separate static nginx service. Neither requires
a frontend build toolchain or a database.

The Hub stores the CLI's raw version-1 JSON report and derives a presentation
view from its fields. Unknown JSON fields are ignored and other schema versions
are rejected; this is decoding a supported subset, not full JSON Schema
validation. Its verdict maps the recorded CLI `exit_code`: 0 → `clear`,
1 → `blocked`, 2 → `review`, otherwise → `failed`. `clear` is not a correctness
claim. Operational run state (`queued`, `running`, `done`, `failed`) is separate.

## 2. Account and repository flows

1. **Sign in.** OAuth supports GitHub/GitHub Enterprise and GitLab/self-managed
   GitLab. GitHub scopes are `repo`, `read:org`; GitLab uses `api`. Signed OAuth
   state binds the forge and permitted return path. Tokens stay server-side.
   Logout removes the browser session. With `ALLOW_NO_FORGE`, the application
   serves before OAuth is configured and explains that sign-in is unavailable.
2. **Synchronize.** List the account's repositories and probe `.probe.json`
   on each default branch, with bounded concurrency (8 policy probes) and a
   configurable repository limit. Preserve monitoring state and history across
   synchronization. Progress reaches the account over server-sent events.
3. **Bootstrap policy.** Detect a supported language from the default branch
   or accept the user's selection; call the installed CLI's `init` in a temporary
   directory. The UI previews the generated policy before the user commits it.
   The API's preview flag is a separate request, not a server-side approval token.
   Creation commits directly to the default branch, refuses an existing policy,
   and surfaces forge permission/conflict errors. It does not create a PR or
   certify that dependencies and checks are ready.
4. **Monitor.** Installing monitoring creates a forge push webhook with separate
   random routing key, installation token, signature secret and badge key.
   Disabling monitoring invalidates the installation. Legacy installations
   without tokens are marked `hook_outdated`; the UI offers explicit reinstall
   and then exposes the new badge URL and README snippet.
5. **Analyze.** Push deliveries enqueue the pushed tip (`after`) against `before`;
   they do not run every intermediate commit of a multi-commit push. Ignore tag
   pushes and branch deletions. `DEFAULT_BRANCH_ONLY` optionally filters branches.
   Users can also request a commit or the default branch tip manually.
6. **Read results.** Stream repository/report updates to the signed-in account,
   select a repository and commit, filter alerts by low/medium/high/critical,
   expand the concerned modifications and highlighted lines, browse retained
   history and download raw JSON. Diff presentation is bounded to 40,000 lines
   and signals truncation. UI text is rendered as text, not trusted markup.

The view groups hypotheses, non-passing checks, signals and review targets;
reproduced hypotheses rank ahead of less conclusive entries. Coverage, review
surface and unverified areas use the CLI's recorded values. Dedicated F1–F9
sections such as fuzz, mutation, preparation and impact are **not** decoded into
full Hub panels: their raw fields remain available in the downloaded report,
and supported hypotheses/checks/signals may still appear in the alert list.

## 3. Analysis boundary and scheduling

- Default mode is `auto`: `review-read-only` when deployment endpoint and model
  are configured, otherwise `lint`. Partial provider configuration fails startup.
  Both modes execute no repository code. Read-only AI review runs the CLI's
  `review --read-only`, exposing only source inspection tools and recording
  unverified suspicions or validated source dismissals, never reproduced issues.
  Provider, credential name and reviewer budgets cannot come from repository
  policy in this mode. Cached lint reports keep their mode until explicitly rerun.
  Startup rejects full `review` on a public instance. Private review requires an operator
  allowlist of `<provider>:<owner/repo>@sha256:<policy-digest>` entries. At each
  run the Hub hashes the exact policy bytes at the selected base commit; missing
  policy or a digest/repository mismatch falls back to lint.
- Each job uses a disposable Git directory. Fetch the branch or exact commit
  with configurable depth (default 50), then detach at the candidate. Base is
  the push's reachable `before`, otherwise the first parent, otherwise the
  candidate itself (an empty comparison for an initial/unavailable-parent range).
  This fallback must not be described as a complete branch history review.
- Invoke the binary with `--base`, `--head`, `--exact`, `--ci`,
  `--format json,markdown` and a local output directory. Read at most 64 MiB of
  report JSON. Process exits 0/1/2 are analysis outcomes; 3/4 indicate incomplete
  execution. Preserve a readable report even for an operational failure.
- Forge credentials are scoped to the clone URL through `GIT_CONFIG_*`, not
  command arguments or the CLI's environment. CLI environment is restricted
  to execution essentials, selected Docker variables, reviewer endpoint/model
  and `PROBE_API_KEY`. If the deployment endpoint variable is absent, review
  explicitly passes `--reviewer=false`. The Hub does not forward the CLI's
  optional F-stage flags, cache directory, intent or network opt-ins.
- Default workers: 2; shared pending queue: 256; per-account queued/running
  quota: 8; analysis timeout: 10 minutes. Deduplicate jobs by account, repository
  and commit while queued/running. Quota exhaustion returns 429 with
  `Retry-After: 60`; a full shared queue returns 503.
- The queue, deduplication and event subscriptions are in memory. Restart does
  not provide durable job replay or distributed worker coordination.
- When enabled (default), publish commit status for monitored repositories:
  CLI 0 maps to success, 1/2 to failure, operational failure to error. Status
  publishing is best effort and does not make an analysis fail. No automatic
  merge, approval, PR comment or SARIF upload is performed by the Hub.

## 4. Authentication, webhooks and storage

Sessions are signed, expiring cookies without forge credentials: HttpOnly,
SameSite=Lax, Secure when the configured base URL is HTTPS. State-changing
browser calls require the session-bound `X-Probe-CSRF` token and same-origin
validation. Repository reads/writes, reports and SSE are scoped to the owner;
a key belonging to another account resolves to 404.

Forge tokens, webhook secrets and installation tokens are sealed with AES-GCM
before storage. A generated session key is persisted in the data volume;
operators may supply one and previous keys for rotation. Startup reseals stored
credentials under the new key, while old browser sessions expire through key
rotation. Back up the key and volume together. Key loss requires fresh sign-in
and webhook reinstall; setting a common key alone does not provide multi-replica
coordination. State writes use private directories/files and atomic replacement;
retain at most 50 reports per repository.

Webhook processing at `POST /hooks/{key}?token=…` follows this order:

1. Validate the routing key (at most 128 bytes), resolve a monitored installation
   whose owner still exists, and compare its unsealed installation token in
   constant time. Invalid/unverifiable keys or tokens return the same 401 and
   do not consume limiter capacity or read the request body.
2. Count authenticated installations in a fixed one-minute window (default
   30 deliveries/key). Return 429 with `Retry-After: 60` above the limit. The
   limiter holds at most 10,000 keys; at capacity it evicts the least active of
   a random sample of 8, rather than refusing all unseen keys. It is a bounded
   fairness control, not precise traffic shaping or edge flood protection.
3. Read a bounded payload (5 MiB), verify GitHub HMAC-SHA256 or GitLab's
   constant-time secret-token comparison. Authentication failures return 401.
   Handle signed pings/non-push events; parse pushes and reject differing
   repository IDs when both the stored ID and payload ID are present.
4. Apply branch/deletion filters and enqueue with the account quota. Successful
   queueing and ignored events return 202; malformed/mismatched payloads fail.

The badge has a separate random key; webhook keys cannot access it. Unknown or
unpublished badges return the same plain 404. Anonymous `/healthz` discloses only
`{"status":"ok"}`. Anonymous `/api/me` provides sign-in/forge information but
no build, CLI version or execution mode; those appear after authentication.
The static sign-in UI and OAuth routes are also public.

The Hub sets a restrictive CSP without inline scripts, anti-framing and
content-type headers; HTTPS adds HSTS. Request logs exclude query strings.
The edge must also omit query strings, because webhook installation tokens and
OAuth codes use them; application headers cannot configure proxy access logs.
Detailed configuration and key operations: [Hub guide](../hub/README.md).

## 5. HTTP surface

| Method | Route | Contract |
| --- | --- | --- |
| GET | `/healthz`, `/api/me` | Limited anonymous metadata; account details after sign-in. |
| GET | `/auth/{github,gitlab}/start`, `/auth/{github,gitlab}/callback` | OAuth entry and callback. |
| POST | `/auth/logout` | End the browser session. |
| GET | `/api/repos`, `/api/repos/{repo}` | Owner's repositories and selection. |
| POST | `/api/repos/sync` | Start background synchronization. |
| POST | `/api/repos/{repo}/policy` | Preview or commit missing policy. |
| POST / DELETE | `/api/repos/{repo}/monitor` | Install/reinstall or remove monitoring. |
| POST | `/api/repos/{repo}/analyze` | Request an analysis. |
| POST | `/api/repos/{repo}/cancel` | Withdraw an analysis still queued. |
| POST | `/api/repos/{repo}/rerun` | Queue a stored analysis again with its recorded parameters. |
| GET | `/api/repos/{repo}/runs` | Retained per-commit history. |
| GET | `/api/repos/{repo}/reports/{commit}`, `/raw` suffix | Presentation view or raw JSON download. |
| GET | `/api/events` | Account-scoped server-sent events. |
| POST | `/hooks/{key}?token=…` | Installation and forge-authenticated deliveries. |
| GET | `/badge/{badge_key}.svg` | Public latest result for a published badge key. |
| GET | `/`, `/app.html`, static assets | Embedded UI; API authentication still applies. |

## 6. Website and deployment as shipped

`web/public/` provides the home, download, getting-started and CLI reference
pages, shared styles/script and links to the Hub. JavaScript fetches public
GitHub releases for version/download selection, detects platform, supplies
installation tabs and copy buttons, and filters reference tables. This is a
promotional/documentation site, not an analysis API. nginx serves existing
files or 404, compresses text assets, caches CSS/JS for an hour and images for a
week; security headers/WAF/rate limiting depend on the configured edge.
Every indexable page carries a title (at most 60 characters), a meta
description (at most 160), a canonical link, Open Graph and Twitter cards with
the 1200×630 `og-image.png`, JSON-LD structured data and a single `<h1>`;
`web/scripts/check-seo.sh` enforces this when the image is built. Missing paths
get `404.html` (noindex) with a 404 status.
`/badge/verified-by-probe.svg` and its shields.io endpoint
`/badge/verified-by-probe.json` are the static "verified by Probe" README badge
other repositories embed ([BADGE.md](../app/docs/BADGE.md)); their URLs are
stable.

`hub/Dockerfile` builds both Go binaries with the same version and embeds the
UI. The runtime is Debian slim with Git and CA certificates, UID 10001,
a persistent `/var/lib/probe-hub` volume, port 8080 and a binary healthcheck.
The shipped runtime contains neither the Docker client nor a Docker socket;
private review needs an operator-provided execution environment, Docker access
and preloaded trusted test images. Mounting a socket alone does not add the
Docker executable.

| Deployment file | Shipped behavior |
| --- | --- |
| [Combined Swarm stack](../devops/docker-compose.swarm.yml) | nginx website and `probe-app` Hub, Traefik HTTPS routes, external `proxy` network; defaults `probe.technology` and `app.probe.technology`. Public/auto are fixed; no-forge startup defaults on. Hub has one replica and stop-first updates on its state volume. |
| [Standalone Hub stack](../devops/docker-compose.hub.yml) | Configurable domain/instance/mode, one replica, persistent volume, explicit external Docker secrets for the session key and GitHub client secret. Review still needs additional Docker setup. |

The combined stack supplies secrets as environment variables unless deployment
mounts override them; it declares no Docker secrets itself. Hub secret lookup
prefers explicit `_FILE`, then `/run/secrets/<NAME>`, then environment. Do not
claim both stacks keep all credentials out of the environment. Traefik proxy
query-log suppression is operator configuration, not accomplished by labels.

[hub/scripts/postbuild.sh](../hub/scripts/postbuild.sh) builds/tags and optionally
pushes images to Docker Hub or an internal registry; only a `vX.Y.Z` version
moves `latest`. The PulsarCD test stage (`devops/test.Dockerfile`) vets,
race-checks and tests the hub and checks its formatting before every
deployment; [docker-compose.post.sh](../devops/docker-compose.post.sh) then
publishes the deployed image to Docker Hub when credentials are set. The CLI reusable review workflow remains pinned to
v0.1.0 with an archive checksum; current-source F1–F9 capabilities do not imply
that workflow already runs them. Upgrade policy and workflow pins in the order
specified by the CLI contract.

The optional PulsarCD deployment gate is implemented in the companion PulsarCD
repository, not in this Hub; [PULSARCD.md](../app/docs/PULSARCD.md) describes its
integration boundary. This repository's compose files do not implement that gate.

## 7. Traceability, checks and remaining scope

| Behavior | Implementation / existing checks |
| --- | --- |
| Modes, allowlist, defaults and secret loading | [config](../hub/internal/config/) |
| OAuth, account tokens and key rotation | [accounts](../hub/internal/accounts/), [secrets](../hub/internal/secrets/), [auth.go](../hub/internal/server/auth.go) |
| Policy bootstrap, ownership, hooks, metadata and UI endpoints | [server](../hub/internal/server/), especially `TestPolicyBootstrap`, `TestOneAccountCannotReachAnother`, `TestAnonymousAccess`, `TestUnverifiableKeyStormCannotBlockDeliveries` and limiter tests |
| Git comparison, exact invocation, digest gate and quotas | [analysis](../hub/internal/analysis/), including `TestRunCLIPassesTheExactRangeAndReadsTheReport` and `TestReviewOnlyRunsForAValidatedPolicy` |
| Provider API and delivery verification | [forge](../hub/internal/forge/) |
| Atomic/private storage and bounded history | [store](../hub/internal/store/) |
| Exit-code mapping, alert order and diff limits | [report](../hub/internal/report/) |
| Account event streams and frontend | [events](../hub/internal/events/), [embedded UI](../hub/web/public/), `TestEventStreamIsPerAccount` in server tests |
| Website and deployment | [web](../web/), [devops](../devops/), [PulsarCD tests](../devops/test.Dockerfile), [Docker Hub publication](../devops/docker-compose.post.sh) |

Run `go test ./hub/...` and `go vet ./hub/...` from the workspace root; the Hub
workflow also runs race checks and image smoke checks. Unit/HTTP fixture tests
do not demonstrate a live OAuth, webhook or deployment round-trip.

Not implemented: durable/distributed queues or shared-storage coordination,
automatic backlog replay, a dedicated UI for every v0.4 report section,
CLI report signing, PR approval/merge, PR-comment publishing or SARIF uploading.
Forge integration, badges and manual/push analysis already exist and must not
be listed as wholly future work.
