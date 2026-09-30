# Probe

**Spend review time on the changes that need your judgment.**

![Probe demo: an agent reports a change as done, and Probe shows what still needs review](.github/assets/probe-demo.gif)

Probe is a Go CLI for reviewing AI-assisted pull requests: it maps Git
changes to risk signals, runs isolated checks and adversarial tests, and
produces a focused review plan with traceable evidence.

## Repository layout

| Directory | Contents |
| --- | --- |
| [`app/`](app/README.md) | The Probe CLI (Go module `github.com/gvinsot/Probe/app`) and its documentation, examples and report schema. |
| [`desktop/`](desktop/README.md) | Probe Desktop (Go module `github.com/gvinsot/Probe/desktop`): the Windows and macOS application that watches the Word, Excel and PowerPoint files of synchronized folders (OneDrive, SharePoint, Google Drive, Dropbox…) and of Google Drives read through their API. |
| [`hub/`](hub/README.md) | The Probe Hub web application (Go module `github.com/gvinsot/Probe/hub`): forge sign-in, policy bootstrap and the live report viewer. |
| [`web/`](web/) | The static promotional website, served by nginx. |
| [`devops/`](devops/) | PulsarCD / Docker Swarm deployment of the website. |
| [`specs/`](specs/README.md) | Current CLI, Hub, website and deployment specifications. |

A root `go.work` includes `app/` and `hub/`, so Go commands also work from the
repository root with workspace patterns (`go test ./app/...`, `go test ./hub/...`). Probe
reviews its own pull requests with the root [`.probe.json`](.probe.json).

## Quick start

Download a binary from [GitHub Releases](https://github.com/gvinsot/Probe/releases),
or build from source:

```sh
cd app
go build -o probe ./cmd/probe
```

See [app/README.md](app/README.md) for usage, configuration and CI integration.

## Website

```sh
docker build -t probe-web web
docker run --rm -p 8080:80 probe-web   # http://localhost:8080
```

Deployment goes through PulsarCD with `devops/docker-compose.swarm.yml`; copy
`devops/.env.example` to `devops/.env` to set the public domains.

## Web application

The hub complements the website: sign in with GitHub or GitLab, let it create a
`.probe.json` policy in the repositories that have none, and read a
severity-filtered report for every new commit — clicking an alert unfolds the
modifications it concerns. It is deployed from the same stack as the website and
served on [app.probe.technology](https://app.probe.technology), linked from every
page of the site.

```sh
docker build -f hub/Dockerfile -t probe-hub .
docker run --rm -p 8080:8080 \
  -e PROBE_HUB_BASE_URL=http://localhost:8080 \
  -e PROBE_HUB_GITHUB_CLIENT_ID=... \
  -e PROBE_HUB_GITHUB_CLIENT_SECRET=... \
  probe-hub
```

It runs as a single container with no database, so a company can deploy it
internally against its own GitHub Enterprise or GitLab instance. Sign-in needs
an OAuth application per forge; until one is configured the deployment still
serves, and its sign-in page says that no forge is available. Images are
published to Docker Hub after each production deployment
(`devops/docker-compose.post.sh`), or built by hand with
`hub/scripts/postbuild.sh`; see
[hub/README.md](hub/README.md) for the configuration and the security model.

Licensed under the GNU AGPL-3.0 with an attribution term (section 7(b)), see [LICENSE](LICENSE) and [NOTICE](NOTICE). Releases up to v0.3.0 remain available under the MIT license.
