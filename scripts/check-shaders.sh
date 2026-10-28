#!/usr/bin/env bash
# Verifies shaders/manifest.json matches the sources and compiled outputs.
# Needs only Go, so CI can run it without the shader toolchain.
set -euo pipefail
cd "$(dirname "$0")/.."
go run ./scripts/shadermanifest check
