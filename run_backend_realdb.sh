#!/usr/bin/env bash
# Build the optional drivers. Select STORE_BACKEND=real in the environment or .env.
set -euo pipefail
cd "$(dirname "$0")/backend"
exec go run -modfile=go.realdb.mod -tags realdb .
