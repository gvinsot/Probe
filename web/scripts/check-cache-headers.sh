#!/bin/sh
# Checks the cache headers the website really serves: every page and asset is
# revalidated on each load (Cache-Control: no-cache, no max-age or Expires),
# carries a validator, and an unchanged file answers 304 to a conditional
# request. Run it against the built image, e.g.
#   docker run -d -p 8080:80 probe-web:ci
#   web/scripts/check-cache-headers.sh http://127.0.0.1:8080
set -eu

BASE=${1:-http://127.0.0.1:8080}
PATHS="/ /fr/ /de/index.html /es/desktop.html /index.html /desktop.html /download.html /getting-started.html /docs.html /styles.css /site.js /robots.txt /sitemap.xml /logo.png /title.png /og-image.png /apple-touch-icon.png /badge/verified-by-probe.svg /badge/verified-by-probe.json"
failed=0
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

fail() { echo "FAIL $1: $2" >&2; failed=1; bad=1; }

# header prints the value of one response header, case-insensitively.
header() { grep -i "^$1:" "$tmp" | head -n 1 | cut -d: -f2- | tr -d '\r' | sed 's/^ *//'; }

for path in $PATHS; do
  bad=0
  # Ask for gzip like a browser does: nginx weakens the ETag of compressed
  # responses, and revalidation must still work in that case.
  curl -s -o /dev/null -D "$tmp" -H 'Accept-Encoding: gzip' "$BASE$path"
  status=$(head -n 1 "$tmp" | cut -d' ' -f2)
  [ "$status" = 200 ] || { fail "$path" "status $status"; continue; }
  cache=$(header Cache-Control)
  [ "$cache" = "no-cache" ] || fail "$path" "Cache-Control '$cache', want 'no-cache'"
  [ -z "$(header Expires)" ] || fail "$path" "unexpected Expires '$(header Expires)'"
  etag=$(header ETag)
  [ -n "$etag" ] || { fail "$path" "no ETag"; continue; }

  curl -s -o /dev/null -D "$tmp" -H 'Accept-Encoding: gzip' -H "If-None-Match: $etag" "$BASE$path"
  status=$(head -n 1 "$tmp" | cut -d' ' -f2)
  [ "$status" = 304 ] || fail "$path" "revalidation status $status, want 304"
  [ "$(header Cache-Control)" = "no-cache" ] || fail "$path" "304 without Cache-Control: no-cache"
  [ "$bad" = 1 ] || echo "ok   $path  $etag"
done

exit "$failed"
