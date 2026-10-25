#!/usr/bin/env bash
# Start the MiniCoco backend (Go): durable sessions + single-round model proxy.
#
# Default: DeepSeek Flash, with settings loaded from the project .env.
# Offline demo (no API key):
#   LLM_PROVIDER=mock ./run_backend.sh
#
# With a real OpenAI-compatible model:
#   LLM_PROVIDER=openai LLM_BASE_URL=https://api.deepseek.com/v1 \
#   LLM_API_KEY=your-key LLM_MODEL=deepseek-flash ./run_backend.sh
set -euo pipefail
cd "$(dirname "$0")/backend"
exec go run .
