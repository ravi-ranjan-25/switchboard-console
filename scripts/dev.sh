#!/usr/bin/env bash
# Run Switchboard locally against a mock GitHub API seeded from testdata/seed. No real repo, cluster or secret is touched.
#   scripts/dev.sh            canned fake cluster data (DEV_DEMO_CLUSTER=1)
#   DEMO=0 scripts/dev.sh     empty cluster data (DEV_NO_CLUSTER=1 only)
set -euo pipefail
cd "$(dirname "$0")/.."
TMP=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$TMP"' EXIT
openssl genrsa -out "$TMP/key.pem" 2048 2>/dev/null
go build -o "$TMP/mockgithub" ./cmd/mockgithub
go build -o "$TMP/switchboard" .
"$TMP/mockgithub" -addr 127.0.0.1:9099 -seed testdata/seed &
sleep 0.5
export REPO=example-owner/citadel
export GITHUB_APP_ID=1 GITHUB_INSTALLATION_ID=2 GITHUB_PRIVATE_KEY_PATH="$TMP/key.pem" GITHUB_API_URL=http://127.0.0.1:9099
export GITHUB_REPO_TOKEN=dev-mock-token  # fake: only the mock GitHub ever sees it
export DEV_NO_AUTH=1 DEV_NO_CLUSTER=1 DEV_DEMO_CLUSTER="${DEMO:-1}" LISTEN="${LISTEN:-127.0.0.1:8080}"
[ "${DEMO:-1}" = 0 ] && unset DEV_DEMO_CLUSTER
"$TMP/switchboard"
