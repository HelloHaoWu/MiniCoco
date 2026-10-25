#!/usr/bin/env bash
# Prove the default module graph and binary need no third-party downloads.
set -euo pipefail
project_root="$(cd "$(dirname "$0")" && pwd)"
check_dir="$(mktemp -d "${TMPDIR:-/tmp}/minicoco-default-deps.XXXXXX")"
trap 'rm -rf "$check_dir"' EXIT
export GOPATH="$check_dir/gopath" GOMODCACHE="$check_dir/modules"
export GOPROXY=off GOSUMDB=off GOWORK=off GOFLAGS= GOTOOLCHAIN=local
cd "$project_root/backend"
test "$(go list -m all)" = minicoco
test "$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' . | sed '/^$/d')" = minicoco
CGO_ENABLED=0 go build -o "$check_dir/backend" .
if go version -m "$check_dir/backend" | awk '$1 == "dep" {found=1} END {exit !found}'; then
    echo 'Default binary unexpectedly contains third-party modules.' >&2
    exit 1
fi
echo 'Default profile: empty module cache, downloads disabled, standard library only, CGO_ENABLED=0 build passed.'
