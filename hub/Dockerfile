# Probe Hub: the web application, with the CLI it runs.
#
# Build context is the repository root, because the image needs both modules:
#   docker build -f hub/Dockerfile -t probe-hub .
#
# The CLI is shipped inside the image on purpose. The hub never analyzes a
# change itself: it prepares a disposable checkout and executes this trusted
# binary, so the report a user reads is the one the CLI produced.

FROM golang:1.26-bookworm AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.work ./
COPY app/ app/
COPY hub/ hub/
ARG VERSION=dev
# Both binaries carry the build version: the UI reports the hub's and the CLI's.
RUN go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/probe ./app/cmd/probe \
 && go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/probe-hub ./hub/cmd/probe-hub
# The binaries must run in the final image, which has no Go toolchain.
RUN go vet ./hub/... && /out/probe-hub version && /out/probe version

FROM debian:bookworm-slim
# git fetches the analyzed commits; ca-certificates lets the hub reach the
# forge over TLS. Nothing else is needed: the UI is embedded in the binary.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# The service runs unprivileged and owns only its data directory.
RUN useradd --system --create-home --home-dir /var/lib/probe-hub --uid 10001 probe

COPY --from=build /out/probe /usr/local/bin/probe
COPY --from=build /out/probe-hub /usr/local/bin/probe-hub

ENV PROBE_HUB_ADDR=:8080 \
    PROBE_HUB_DATA_DIR=/var/lib/probe-hub \
    PROBE_HUB_BINARY=/usr/local/bin/probe \
    PROBE_HUB_INSTANCE=public \
    PROBE_HUB_MODE=auto

USER probe
WORKDIR /var/lib/probe-hub
VOLUME ["/var/lib/probe-hub"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/probe-hub", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/probe-hub"]
