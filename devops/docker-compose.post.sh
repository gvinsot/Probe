#!/usr/bin/env bash
# PulsarCD runs this script from devops/ after each deployment of the stack.
#
# After a production deployment, it publishes the hub image just deployed to
# Docker Hub (<DOCKERHUB_USERNAME>/probe-hub:vX.Y.Z and :latest), for the
# companies that run docker-compose.hub.yml. It pushes the exact image
# production runs, already tested by the PulsarCD test stage: nothing is
# rebuilt. The image has the platform PulsarCD builds (linux/amd64).
#
# DOCKERHUB_USERNAME and DOCKERHUB_TOKEN come from devops/.env. Without them
# the script does nothing. The Docker Hub login lives in a temporary Docker
# configuration, removed on exit: the token is never left on the host.
#
# PulsarCD exports DEPLOY_ENV (prod or qa) and DEPLOY_VERSION (0.5.16).
set -euo pipefail

if [[ "${DEPLOY_ENV:-}" != prod ]]; then
    echo "post: ${DEPLOY_ENV:-unknown} deployment, Docker Hub left unchanged"
    exit 0
fi
if [[ -z "${DOCKERHUB_USERNAME:-}" || -z "${DOCKERHUB_TOKEN:-}" ]]; then
    echo "post: DOCKERHUB_USERNAME or DOCKERHUB_TOKEN unset, the hub is not published to Docker Hub"
    exit 0
fi
version="${DEPLOY_VERSION:?}"
if [[ ! "$version" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "post: refusing the unusable version '$version'" >&2
    exit 1
fi
version="v${version#v}"

source_image="${DEPLOY_REGISTRY:-registry.methodinfo.fr}/probe-hub:${version#v}"
target="${DOCKERHUB_USERNAME}/probe-hub"

config="$(mktemp -d)"
trap 'rm -rf "$config"' EXIT
printf '%s' "$DOCKERHUB_TOKEN" | docker --config "$config" login docker.io --username "$DOCKERHUB_USERNAME" --password-stdin

# The deployment has just pulled the image on this host.
for tag in "$version" latest; do
    docker tag "$source_image" "$target:$tag"
    docker --config "$config" push --quiet "$target:$tag"
    echo "post: published $target:$tag"
done
