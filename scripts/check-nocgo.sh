#!/bin/sh
# Fails if the binaries don't build without cgo. The app loads native code
# only through purego, and the Makefile builds with CGO_ENABLED=0, so a
# dependency that needs cgo would break the build; one with an optional cgo
# file and a pure fallback is fine.
set -eu
cd "$(dirname "$0")/.."
if ! out=$(CGO_ENABLED=0 go build -o /dev/null ./cmd/... 2>&1); then
	echo "$out" >&2
	echo "the binaries need cgo: find the import with go list -deps" >&2
	exit 1
fi
echo "builds without cgo"
