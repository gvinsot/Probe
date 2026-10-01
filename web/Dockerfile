# Promotional website for Probe: static files served by nginx, and the
# downloads Probe Desktop updates itself from.
#
# Build context is the repository root, because the downloads are built from
# app/ and desktop/:
#   docker build -f web/Dockerfile --build-arg VERSION=0.6.1 -t probe-web .
#
# /download/ holds the CLI archives, the Windows executables of Probe Desktop
# and latest.json, which lists them with their size and SHA-256. The
# container signs latest.json when it starts (docker-entrypoint.d), with the
# key Swarm mounts as a secret: the key is never part of the image.

FROM golang:1.26-bookworm AS build
WORKDIR /src
# Each module builds on its own: go.work would also want hub/.
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOWORK=off
COPY LICENSE NOTICE ./
COPY app/ app/
COPY desktop/ desktop/
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod \
    set -eu; \
    mkdir -p /out/download; \
    cd /src/app; \
    go run ./tools/build -version "$VERSION" -out /tmp/cli \
      -targets linux/amd64,linux/arm64,darwin/amd64,darwin/arm64,windows/amd64,windows/arm64; \
    cp /tmp/cli/*.tar.gz /tmp/cli/*.zip /out/download/; \
    go build -ldflags "-s -w" -o /out/manifest ./tools/manifest; \
    cd /src/desktop; \
    for arch in amd64 arm64; do \
      GOOS=windows GOARCH=$arch go build -ldflags "-H=windowsgui -s -w -X main.version=$VERSION" \
        -o "/out/download/probe-desktop-$VERSION-windows-$arch.exe" ./cmd/probe-desktop; \
    done; \
    /out/manifest write -version "$VERSION" -dir /out/download > /out/download/latest.json; \
    cat /out/download/latest.json

# The translated site: web/public/ (English, the source) and one directory per
# language, generated from the catalogs of web/i18n/. A segment without
# translation is published in English and listed in the build log.
FROM python:3.12-alpine AS site
WORKDIR /src/web
COPY web/public public
COPY web/i18n i18n
COPY web/scripts/i18n.py scripts/i18n.py
RUN python3 scripts/i18n.py build --out /out/site

FROM nginx:1.27-alpine-slim

COPY web/nginx.conf /etc/nginx/conf.d/default.conf
# The base image ships its own index.html and 50x.html. The site replaces them
# entirely: a leftover default page would be served, listed in sitemap.xml and
# rejected by the search checks below.
RUN rm -rf /usr/share/nginx/html && mkdir -p /usr/share/nginx/html
COPY --from=site /out/site /usr/share/nginx/html
# sitemap.xml is generated from the pages the image really serves, so it never
# goes stale when a page is added or removed.
ARG SITE_URL=https://probe.technology
RUN --mount=type=bind,source=web/scripts/generate-sitemap.sh,target=/tmp/generate-sitemap.sh \
    sh /tmp/generate-sitemap.sh /usr/share/nginx/html "$SITE_URL" > /usr/share/nginx/html/sitemap.xml \
    && cat /usr/share/nginx/html/sitemap.xml
# Every indexable page keeps its title, description, canonical link, sharing
# card, structured data and single <h1>: a page that loses one fails the build.
RUN --mount=type=bind,source=web/scripts/check-seo.sh,target=/tmp/check-seo.sh \
    sh /tmp/check-seo.sh /usr/share/nginx/html "$SITE_URL"
COPY --from=build /out/download /usr/share/nginx/html/download
COPY --from=build /out/manifest /usr/local/bin/probe-manifest
COPY --chmod=0755 web/docker-entrypoint.d/30-app-domain.sh web/docker-entrypoint.d/40-sign-update-manifest.sh /docker-entrypoint.d/

EXPOSE 80

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1/ || exit 1
