# syntax=docker/dockerfile:1
# Tests PulsarCD runs between the build and the deployment: its test stage
# builds and runs the "test" service of docker-compose.swarm.yml, and a
# failure stops the pipeline. Production, and through it every installed
# Probe Desktop, only receives a commit that passed them.
#
# The Docker sandbox tests (PROBE_TEST_DOCKER_IMAGE) and the browser test of
# the hub are left to GitHub: they need a Docker daemon or a browser, and skip
# without them.
#
# Build context is the repository root.

FROM golang:1.26-bookworm
WORKDIR /src

# Dependencies first, so that they are downloaded again only when they change.
# app/ uses the standard library only; hub/ needs the PostgreSQL driver.
COPY go.work ./
COPY app/go.mod app/
COPY hub/go.mod hub/go.sum hub/
RUN go mod download
COPY desktop/go.mod desktop/go.sum desktop/
RUN cd desktop && GOWORK=off go mod download

COPY . .

COPY <<'EOF' /usr/local/bin/probe-test
#!/bin/sh
set -eu
unformatted() {
    files=$(gofmt -l "$@")
    if [ -n "$files" ]; then
        echo "gofmt needed for:"
        echo "$files"
        return 1
    fi
}
echo "== app"
cd /src/app
go vet ./...
go test -race ./...
echo "== hub"
cd /src
go vet ./hub/...
go test -race ./hub/...
unformatted hub
echo "== desktop"
cd /src/desktop
export GOWORK=off
go vet ./...
GOOS=windows go vet ./...
go test -race ./...
unformatted .
echo "== all tests passed"
EOF
RUN chmod +x /usr/local/bin/probe-test

CMD ["probe-test"]
