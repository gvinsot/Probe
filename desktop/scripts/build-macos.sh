#!/usr/bin/env bash
# Builds "Probe Desktop.app" for macOS, as a universal binary (Apple silicon
# and Intel). Must run on a Mac: the tray icon and the web view use Cocoa
# through cgo.
#
# The bundle declares LSUIElement: no Dock icon and no Terminal window, the
# application lives in the menu bar.
#
# Usage: scripts/build-macos.sh [version]
# PROBE_GOOGLE_CLIENT_ID and PROBE_GOOGLE_CLIENT_SECRET, when set, build in the
# OAuth client used to connect Google accounts (see README.md).
# Signing: set CODESIGN_IDENTITY="Developer ID Application: …" to sign; then
# notarize the zip with `xcrun notarytool submit … --wait`.
set -euo pipefail

version="${1:-dev}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
export GOWORK=off CGO_ENABLED=1
mkdir -p dist/build

ldflags="-s -w -X main.version=$version"
if [[ -n "${PROBE_GOOGLE_CLIENT_ID:-}" ]]; then
  pkg=github.com/gvinsot/Probe/desktop/internal/gdrive
  ldflags+=" -X $pkg.DefaultClientID=$PROBE_GOOGLE_CLIENT_ID -X $pkg.DefaultClientSecret=${PROBE_GOOGLE_CLIENT_SECRET:-}"
fi

for arch in arm64 amd64; do
  GOOS=darwin GOARCH="$arch" go build -trimpath \
    -ldflags "$ldflags" \
    -o "dist/build/probe-desktop-$arch" ./cmd/probe-desktop
done

app="dist/Probe Desktop.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
lipo -create -output "$app/Contents/MacOS/probe-desktop" \
  dist/build/probe-desktop-arm64 dist/build/probe-desktop-amd64
# Info.plist wants a numeric version: v0.6.0 becomes 0.6.0, a dev build 0.0.0.
bundle_version=0.0.0
if [[ "$version" =~ ^v?([0-9]+(\.[0-9]+){0,2}) ]]; then bundle_version="${BASH_REMATCH[1]}"; fi
sed "s/__VERSION__/$bundle_version/g" packaging/macos/Info.plist > "$app/Contents/Info.plist"

if [[ -n "${CODESIGN_IDENTITY:-}" ]]; then
  codesign --force --options runtime --timestamp \
    --sign "$CODESIGN_IDENTITY" "$app"
fi

# Named like the release assets: probe-desktop-<version>-<platform>.<ext>.
archive="probe-desktop-$version-darwin-universal.zip"
(cd dist && ditto -c -k --keepParent "Probe Desktop.app" "$archive")
echo "dist/$archive"
