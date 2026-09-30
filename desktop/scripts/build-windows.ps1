# Builds probe-desktop.exe for Windows.
#
# -H=windowsgui marks the executable as a GUI program: Windows opens no
# console window when it starts, whether from the Start menu, a double click
# or the Run registry key used by "Start at login".
#
# Usage: ./scripts/build-windows.ps1 [-Version 0.1.0] [-Arch amd64|arm64]
# PROBE_GOOGLE_CLIENT_ID and PROBE_GOOGLE_CLIENT_SECRET, when set, build in the
# OAuth client used to connect Google accounts (see README.md).
param(
  [string]$Version = "dev",
  [string]$Arch = "amd64"
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
  # The desktop module is not part of the repository go.work.
  $env:GOWORK = 'off'
  $env:GOOS = 'windows'
  $env:GOARCH = $Arch
  $env:CGO_ENABLED = '0'
  New-Item -ItemType Directory -Force dist | Out-Null
  $ldflags = "-H=windowsgui -s -w -X main.version=$Version"
  if ($env:PROBE_GOOGLE_CLIENT_ID) {
    $pkg = 'github.com/gvinsot/Probe/desktop/internal/gdrive'
    $ldflags += " -X $pkg.DefaultClientID=$($env:PROBE_GOOGLE_CLIENT_ID) -X $pkg.DefaultClientSecret=$($env:PROBE_GOOGLE_CLIENT_SECRET)"
  }
  go build -trimpath -ldflags $ldflags -o "dist/probe-desktop-$Version-windows-$Arch.exe" ./cmd/probe-desktop
  if ($LASTEXITCODE -ne 0) { throw "go build failed" }
  Write-Host "dist/probe-desktop-$Version-windows-$Arch.exe"
} finally {
  Pop-Location
}
