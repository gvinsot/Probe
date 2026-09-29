# Probe Desktop

Probe Desktop is the Probe for office documents. It watches the Word, Excel and
PowerPoint files of a OneDrive or Google Drive folder synchronized on the
computer. When a document changes, it compares the new version with the last
reviewed one and flags the modifications that deserve a look.

The findings are computed on the computer by fixed rules. An AI provider
(Anthropic or OpenAI) can be configured to explain a report in plain language,
but it never decides what is flagged.

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
provider, and only when the user asks for an explanation. The OpenAI provider
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

In CI, `.github/workflows/ci.yml` tests and builds the application on Linux,
Windows and macOS. A `v*` tag runs `.github/workflows/release.yml`, which
publishes the Windows executables (amd64, arm64) and the macOS application
(universal) in the same GitHub release as the CLI, listed in `SHA256SUMS`.

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
| `internal/config`, `internal/secret` | settings, cloud folder detection, keychain |
| `web/public` | the interface (HTML, CSS, JavaScript, no framework) |
