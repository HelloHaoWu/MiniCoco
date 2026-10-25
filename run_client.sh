#!/usr/bin/env bash
# Start the MiniCoco client (TypeScript): agent lopp + local tool execution.
# Runs on Node >= 24 native TypeScript support (no build step).
#
#   ./run_client.sh "list the files here" --workspace /path/to/project
#   ./run_client.sh --workspace /path/to/project        # interactive REPL
set -euo pipefail
exec node "$(dirname "$0")/client/src/cli.ts" "$@"
