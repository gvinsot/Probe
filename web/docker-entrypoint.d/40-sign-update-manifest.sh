#!/bin/sh
# Signs /download/latest.json before nginx starts, with the private key Swarm
# mounts at /run/secrets/PROBE_UPDATE_SIGNING_KEY (PulsarCD turns the
# variable of devops/.env into that secret), or PROBE_UPDATE_SIGNING_KEY
# itself when it is set directly.
#
# Without a key the site still serves every download, but no signature:
# Probe Desktop then installs nothing, which is the safe failure.
set -eu

dir=/usr/share/nginx/html/download
key=/run/secrets/PROBE_UPDATE_SIGNING_KEY
rm -f "$dir/latest.json.sig"

if [ ! -f "$dir/latest.json" ]; then
    echo "update manifest: no latest.json, nothing to sign"
    exit 0
fi
if [ -s "$key" ]; then
    probe-manifest sign -key "$key" "$dir/latest.json" > "$dir/latest.json.sig"
elif [ -n "${PROBE_UPDATE_SIGNING_KEY:-}" ]; then
    probe-manifest sign "$dir/latest.json" > "$dir/latest.json.sig"
else
    echo "update manifest: no signing key, Probe Desktop will not update from this site"
    exit 0
fi
echo "update manifest: latest.json signed"
