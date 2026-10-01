#!/bin/sh
# Points the site's links to the web application at PROBE_APP_DOMAIN before
# nginx starts. The pages are built with the production hostname, and the same
# image runs in QA, where PulsarCD prefixes the domain (qa.app.probe.technology):
# the hostname can only be known when the container starts.
#
# Unset or equal to the built hostname: the pages are served as built.
set -eu

built=app.probe.technology
domain=${PROBE_APP_DOMAIN:-$built}
html=/usr/share/nginx/html

if [ "$domain" = "$built" ]; then
    exit 0
fi
# A hostname only: anything else would be written into every page.
if ! printf '%s' "$domain" | grep -Eq '^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$'; then
    echo "app domain: invalid PROBE_APP_DOMAIN, links keep $built" >&2
    exit 0
fi

# The links, and the hostname shown as the text of a link.
find "$html" -name '*.html' -exec sed -i \
    -e "s#https://app\.probe\.technology#https://$domain#g" \
    -e "s#>app\.probe\.technology<#>$domain<#g" {} +
echo "app domain: links point to https://$domain"
