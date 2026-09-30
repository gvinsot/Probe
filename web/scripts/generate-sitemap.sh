#!/bin/sh
# Writes the sitemap of the website to standard output, from the pages it
# really has, so a page added to or removed from web/public/ is listed or
# dropped without anyone editing sitemap.xml. The image runs it at build time:
#   web/scripts/generate-sitemap.sh web/public https://probe.technology
#
# Every *.html page is listed at its <link rel="canonical"> URL, or at
# SITE_URL/<file> when it has none; index.html stands for SITE_URL/. Pages
# with <meta name="robots" content="...noindex..."> are left out, as is any
# page whose name starts with an underscore or is an error page (404.html,
# 50x.html).
set -eu

DIR=${1:-web/public}
SITE_URL=${2:-https://probe.technology}
SITE_URL=${SITE_URL%/}

[ -d "$DIR" ] || { echo "generate-sitemap: no directory $DIR" >&2; exit 1; }

# canonical prints the href of the page's canonical link, if any.
canonical() {
  tr '\n' ' ' < "$1" | grep -o '<link[^>]*rel="canonical"[^>]*>' | head -n 1 |
    sed -n 's/.*href="\([^"]*\)".*/\1/p'
}

# noindex succeeds when the page asks robots not to index it.
noindex() {
  tr '\n' ' ' < "$1" | grep -o '<meta[^>]*name="robots"[^>]*>' | grep -qi 'noindex'
}

# escape makes a URL safe inside an XML element.
escape() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }

echo '<?xml version="1.0" encoding="UTF-8"?>'
echo '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">'
# index.html first, then the other pages in name order.
{ [ -f "$DIR/index.html" ] && echo "$DIR/index.html"
  find "$DIR" -type f -name '*.html' ! -path "$DIR/index.html" | LC_ALL=C sort; } |
while IFS= read -r page; do
  rel=${page#"$DIR"/}
  case "${rel##*/}" in _*|404.html|50x.html) continue ;; esac
  noindex "$page" && continue
  loc=$(canonical "$page")
  if [ -z "$loc" ]; then
    case "$rel" in
      index.html) loc="$SITE_URL/" ;;
      */index.html) loc="$SITE_URL/${rel%index.html}" ;;
      *) loc="$SITE_URL/$rel" ;;
    esac
    echo "generate-sitemap: $rel has no canonical link, listed as $loc" >&2
  fi
  echo "  <url><loc>$(escape "$loc")</loc></url>"
done
echo '</urlset>'
