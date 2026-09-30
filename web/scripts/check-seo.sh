#!/bin/sh
# Checks the search and sharing basics of every indexable page of the
# website, so a new or edited page cannot silently lose them. The image runs
# it at build time, and it runs anywhere with a POSIX shell:
#   web/scripts/check-seo.sh web/public
#
# Every page must have a lang attribute, one <title> of at most 60 characters,
# a meta description of at most 160, a canonical link on SITE_URL matching the
# og:url, the Open Graph and Twitter card tags with an existing image, a
# JSON-LD block, exactly one <h1>, and images with alt text and dimensions.
# A page marked noindex (404.html) only needs its title and noindex.
set -eu

DIR=${1:-web/public}
SITE_URL=${2:-https://probe.technology}
SITE_URL=${SITE_URL%/}
failed=0

fail() { echo "FAIL $1: $2" >&2; failed=1; }

# meta prints the content of the first <meta> whose key attribute ($2, name or
# property) is $3.
meta() {
  tr '\n' ' ' < "$1" | grep -o "<meta [^>]*$2=\"$3\"[^>]*>" | head -n 1 |
    sed -n 's/.*content="\([^"]*\)".*/\1/p'
}

# chars counts the characters of a UTF-8 string, an entity counted as one: the
# continuation bytes are dropped, so the count does not depend on the locale.
chars() { printf '%s' "$1" | sed 's/&[a-z#0-9]*;/_/g' | LC_ALL=C tr -d '\200-\277' | wc -c | tr -d ' '; }

for page in "$DIR"/*.html; do
  name=${page##*/}
  flat=$(tr '\n' ' ' < "$page")
  title=$(printf '%s' "$flat" | grep -o '<title>[^<]*</title>' | sed 's/<[^>]*>//g' || true)
  [ -n "$title" ] || fail "$name" "no <title>"

  if printf '%s' "$flat" | grep -q '<meta name="robots" content="[^"]*noindex'; then
    continue
  fi

  printf '%s' "$flat" | grep -q '<html lang="[a-z]' || fail "$name" "no lang attribute on <html>"
  [ "$(printf '%s' "$flat" | grep -o '<title>' | wc -l)" -eq 1 ] || fail "$name" "more than one <title>"
  [ "$(chars "$title")" -le 60 ] || fail "$name" "title longer than 60 characters: $title"

  desc=$(meta "$page" name description)
  if [ -z "$desc" ]; then
    fail "$name" "no meta description"
  elif [ "$(chars "$desc")" -gt 160 ]; then
    fail "$name" "description longer than 160 characters"
  fi

  canonical=$(printf '%s' "$flat" | grep -o '<link rel="canonical" href="[^"]*"' | sed 's/.*href="\([^"]*\)"/\1/' || true)
  case "$canonical" in
    "$SITE_URL"/*) ;;
    *) fail "$name" "canonical link missing or not on $SITE_URL: '$canonical'" ;;
  esac
  [ "$(meta "$page" property og:url)" = "$canonical" ] || fail "$name" "og:url differs from the canonical link"

  for tag in og:title og:description og:image og:image:width og:image:height og:image:alt og:type og:site_name; do
    [ -n "$(meta "$page" property "$tag")" ] || fail "$name" "no $tag"
  done
  for tag in twitter:card twitter:title twitter:description twitter:image twitter:image:alt; do
    [ -n "$(meta "$page" name "$tag")" ] || fail "$name" "no $tag"
  done
  image=$(meta "$page" property og:image)
  case "$image" in
    "$SITE_URL"/*) [ -f "$DIR/${image#"$SITE_URL"/}" ] || fail "$name" "og:image $image is not in $DIR" ;;
    *) fail "$name" "og:image not on $SITE_URL: '$image'" ;;
  esac

  printf '%s' "$flat" | grep -q '<script type="application/ld+json">{' || fail "$name" "no JSON-LD structured data"

  h1=$(printf '%s' "$flat" | grep -o '<h1[ >]' | wc -l)
  [ "$h1" -eq 1 ] || fail "$name" "$h1 <h1> headings, want exactly one"

  printf '%s' "$flat" | grep -o '<img [^>]*>' | while IFS= read -r img; do
    case "$img" in *' alt="'*) ;; *) echo "FAIL $name: image without alt: $img" >&2; echo x ;; esac
    case "$img" in *' width="'*' height="'*|*' height="'*' width="'*) ;; *) echo "FAIL $name: image without width and height: $img" >&2; echo x ;; esac
  done | grep -q x && failed=1

done

[ "$failed" -eq 0 ] && echo "check-seo: every page passed"
exit "$failed"
