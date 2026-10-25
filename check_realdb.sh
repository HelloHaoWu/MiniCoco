#!/usr/bin/env bash
# Requires Go >= 1.26 and dedicated local MongoDB/Redis test services.
set -euo pipefail
: "${MINICOCO_TEST_MONGO_URI:?Set an explicit MongoDB test URI}"
: "${MINICOCO_TEST_REDIS_URL:?Set an explicit Redis test URL}"
project_root="$(cd "$(dirname "$0")" && pwd)"
check_dir="$(mktemp -d "${TMPDIR:-/tmp}/minicoco-realdb-check.XXXXXX")"
trap 'rm -rf "$check_dir"' EXIT
cd "$project_root/backend"
export GOWORK=off GOFLAGS=
MINICOCO_MODEL_IS_FUNCTION_CALL=true MINICOCO_DEBUG_PROMPT=0 go test -modfile=go.realdb.mod -tags realdb -race -cover -count=1 ./...
go vet -modfile=go.realdb.mod -tags realdb ./...
CGO_ENABLED=0 go build -modfile=go.realdb.mod -tags realdb -o "$check_dir/backend" .
go version -m "$check_dir/backend" > "$check_dir/modules.txt"
for module in modernc.org/sqlite go.mongodb.org/mongo-driver/v2 github.com/redis/go-redis/v9; do
    awk -v module="$module" '$1 == "dep" && $2 == module {found=1} END {exit !found}' "$check_dir/modules.txt"
done
echo 'Real database profile: tests, vet, CGO_ENABLED=0 build and driver inclusion passed.'
