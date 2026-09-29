# Probe development workflow

The repository has six areas: `app/` (the Go CLI and its docs), `hub/` (the
web application that drives the CLI for a whole account), `desktop/` (the
Windows and macOS application that watches Office documents), `web/` (the
promotional website), `devops/` (PulsarCD deployment) and `specs/` (product
specifications).

Run `go test ./...` and `go vet ./...` from `app/` (or `go test ./app/...` from
the root, through `go.work`) for relevant Go changes, and `go test ./hub/...`
for the web application. `desktop/` is a separate module outside `go.work`:
run `GOWORK=off go test ./...` and `GOWORK=off go vet ./...` from `desktop/`
(also with `GOOS=windows`). The hub never re-derives a verdict: it runs the
trusted binary and renders the report the CLI produced. Tests which need real
Docker require `PROBE_TEST_DOCKER_IMAGE` and a preloaded trusted image.

Before handing over committed code, use an installed trusted Probe binary
to run `probe lint --base origin/main`. Before requesting PR review, also
run `probe review --base origin/main --ci` when the prepared sandbox image
is available. Read `.probe/CONFIDENCE_REPORT.md` and report reproduced
issues, unresolved hypotheses and incomplete checks in the handoff.

Probe only analyzes committed files. Do not create commits just for these
checks without authorization; state when the working changes are outside the
analyzed range. Never alter the baseline/policy to hide findings or claim that
a model assertion or a zero exit code proves correctness. Use
`--reviewer=false` when provider use is not configured or appropriate.

See `app/docs/AGENT_WORKFLOW.md`, `app/docs/CI.md`, and `app/docs/PULSARCD.md`
for the complete agent, PR and deployment integration.
