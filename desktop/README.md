# Probe Desktop

Probe Desktop is the Probe for office documents. It watches the Word, Excel and
PowerPoint files of a OneDrive or Google Drive folder synchronized on the
computer. When a document changes, it compares the new version with the last
reviewed one and flags the modifications that deserve a look.

The findings are computed on the computer by fixed rules. An AI provider
(Anthropic or OpenAI, or a local OpenAI-compatible model) can be configured to
explain a report in plain language and to raise extra findings the rules
missed. Those are listed apart as "Raised by AI"; they never remove a rule
finding and never change the severity of the report. When the model states
that the modifications may have a legal or financial impact ("Cette
modification peut avoir une incidence juridique et financière"), the severity
of the document is raised to high for one of them and to critical for both;
the model can raise a severity, never lower it. When the document is saved
again, the explanation is kept, marked as written for an earlier version: the
AI findings about elements modified again are removed, the others stay, and
the raised severity stays only if none of the earlier modifications changed.

## What it flags

| Format | Examples of findings |
| --- | --- |
| Excel (`.xlsx`, `.xlsm`) | formula replaced by a typed value, range reduced in a formula (`SUM(A1:A3)` → `SUM(A1:A2)`), broken reference (`#REF!`), computed result that moved by more than 10%, sheet deleted or made "very hidden", rows or columns hidden, data validation or protection removed, named range redirected |
| Word (`.docx`, `.docm`) | amount or percentage changed, date changed, obligation softened (`shall` → `may`, `doit` → `peut`), negation added or removed, clause about payment, liability or termination removed or reworded, track changes turned off, pending tracked changes resolved without trace, comments deleted |
| PowerPoint (`.pptx`, `.pptm`) | figures changed on a slide or in its notes, slide deleted or hidden |
| All | macros added, link to an external file added, embedded object added |

Macs write the same formats, so Office for Mac documents are covered.
Apple iWork files (Pages, Numbers, Keynote) and legacy `.doc`/`.xls` files are
not read yet.

## How it works

```
probe-desktop                      probe-desktop --window <url>
┌──────────────────────────────┐   ┌───────────────────────────┐
│ tray icon (menu bar)         │   │ native window             │
│ watcher: scan, diff, rules   │◄──┤ WebView2 / WKWebView      │
│ HTTP server on 127.0.0.1     │   │ (or the default browser)  │
└──────────────────────────────┘   └───────────────────────────┘
```

- One executable, two processes. The **engine** keeps the tray icon, scans
  the folders and serves the interface on the loopback interface. The
  **window** is only a client: closing it does not stop the watching.
- **No console, no Dock icon.** The Windows executable is built with
  `-H=windowsgui`; the macOS bundle declares `LSUIElement`. Errors go to
  `desktop.log` in the data directory.
- **Baselines.** The first scan records each document as its reviewed
  version (a private copy in the data directory). After a change, *Mark as
  reviewed* makes the current version the new baseline; the file is read
  again and must still match the report, so nobody approves unseen content.
- **Single instance.** Launching the application again opens the window of
  the running engine.
- **Start at login** (tray menu) registers `probe-desktop --background` in the
  Run registry key (Windows) or a LaunchAgent (macOS).

### Security of the local server

The server holds document excerpts and uses the API key, so it only answers
the window the engine launched:

- random port on `127.0.0.1`, chosen at each start;
- the `Host` header must be that address (no DNS rebinding);
- the window opens a single-use launch URL that sets an `HttpOnly`,
  `SameSite=Strict` session cookie;
- requests that change something need the `X-Probe-Request` header and a
  same-origin `Origin`;
- a strict Content-Security-Policy, no external resource;
- API keys live in the system keychain (Credential Manager, Keychain), never in
  `settings.json`, the logs or the pages.

Only the file name, the findings and the changed excerpts are sent to the AI
provider, and only once a provider is configured: each changed document is
then explained automatically when the change is detected, one at a time and
most severe first, and again when it changes once more. A failed explanation
is not retried until the document or the settings change; the user can still
ask for one from the document. The OpenAI provider
accepts a custom endpoint, so a compatible server run on premises (vLLM,
Ollama…) keeps everything inside the company.

### Online-only files

OneDrive "Files On-Demand" and Google Drive streaming keep some files in the
cloud only. Reading them downloads them, so by default Probe does not record a
baseline for them; enable *Download online-only files* in the settings to do
it anyway. Once a file has a baseline, a later change is read (and downloaded)
normally. Reading the version history of OneDrive/SharePoint and Google Drive
through their APIs is the next source to add; it will feed the same reports.

## Data directory

| System | Location |
| --- | --- |
| Windows | `%AppData%\Probe Desktop` |
| macOS | `~/Library/Application Support/Probe Desktop` |

It holds `settings.json`, `state.json` (documents and reports), `baselines/`
(the reviewed copies), `desktop.log` and the web view profile.
`PROBE_DESKTOP_HOME` overrides it, for tests or a portable install.

## Updates

On Windows, Probe Desktop updates itself from `https://probe.technology/download/`.
A minute after start, then every six hours, it reads `latest.json` and
checks its Ed25519 signature (`latest.json.sig`) against the keys built into
the executable (`internal/update/update.go`). When the manifest names a newer
stable version, the executable for this system is downloaded next to the
running one, checked against the size and SHA-256 of the signed manifest, and
started once with `version` to confirm it runs. As soon as no window is open,
the engine moves itself to `probe-desktop….exe.old`, puts the new version at
its path and restarts into it; `.old` is removed on the next check. If the new
engine has not taken over within thirty seconds, the previous executable comes
back and restarts, and that version is recorded in `update-skip` so it is not
downloaded again.

A development build (`dev`) never updates, nor does an older version ever
replace a newer one. `PROBE_DESKTOP_UPDATE=off` disables updates;
`PROBE_DESKTOP_UPDATE_URL` points to another site, for tests. The executable
must be in a folder the user can write to (not `Program Files`): otherwise
the engine logs it and keeps running its version.

The website image (`web/Dockerfile`) builds the downloads and `latest.json` at
every PulsarCD build and signs the manifest when its container starts, with
the `PROBE_UPDATE_SIGNING_KEY` of `devops/.env`. Installed applications
receive the version production serves, which PulsarCD deploys only after the
`test` service of `devops/docker-compose.swarm.yml` passed (vet, race-checked
tests and formatting of the three modules, `devops/test.Dockerfile`). To replace
the key, add the new public key to `trustedKeys`, ship a release, then change
the signing key.

## Build

The module is independent from the repository `go.work` (it needs a newer Go
and GUI dependencies), so commands run from `desktop/` with `GOWORK=off`.

```sh
cd desktop
GOWORK=off go test ./...
```

Windows (from any system, no C compiler needed):

```powershell
./scripts/build-windows.ps1 -Version v0.6.0    # dist/probe-desktop-v0.6.0-windows-amd64.exe
```

macOS (on a Mac, with the Xcode command line tools; cgo is required):

```sh
bash scripts/build-macos.sh v0.6.0            # dist/probe-desktop-v0.6.0-darwin-universal.zip
```

PulsarCD tests the module on Linux before every deployment (the `test` service
of `devops/docker-compose.swarm.yml`, which also vets the Windows build).
Tests only run on Windows and macOS in `.github/workflows/release.yml`, run by
hand from a version tag, which also publishes the Windows executables (amd64, arm64) and the macOS
application (universal) in the same GitHub release as the CLI, listed in
`SHA256SUMS`. The Windows executables of every production version are also
served by the website, which is where installed applications update from.

### Distribution

- **Windows:** sign the executable (code signing certificate or Azure Trusted
  Signing), otherwise SmartScreen warns on first launch. WebView2 ships with
  Windows 10 and 11; without it the interface opens in the default browser.
- **macOS:** sign with a Developer ID (`CODESIGN_IDENTITY`) and notarize the
  zip, otherwise Gatekeeper blocks the application.

## Layout

| Path | Role |
| --- | --- |
| `cmd/probe-desktop` | entry point: engine, `--background`, `--window` |
| `internal/office` | OOXML reading, diff and risk rules |
| `internal/watch` | folder scan, baselines, document states |
| `internal/server` | loopback HTTP server and its security checks |
| `internal/reviewer` | optional AI explanation (Anthropic SDK, OpenAI-compatible HTTP) |
| `internal/tray`, `internal/window`, `internal/icon` | tray icon, native window, drawn icon |
| `internal/instance`, `internal/autostart`, `internal/platform` | single instance, start at login, OS calls |
| `internal/update` | signed manifest, download and replacement of the executable |
| `internal/config`, `internal/secret` | settings, cloud folder detection, keychain |
| `web/public` | the interface (HTML, CSS, JavaScript, no framework) |
