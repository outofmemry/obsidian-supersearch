#!/bin/sh
# Build the server for this computer: server/supersearch-server, plus the
# Swift helper (server/supersearch-helper) on macOS.
#   ./build.sh                      # Apple backend on macOS 26+, portable elsewhere
#   SUPERSEARCH_BACKEND=portable ./build.sh   # tesseract/poppler backend, also on a Mac
set -eu
cd "$(dirname "$0")"

# go.mod may ask for a newer Go than the one installed: let Go fetch it once.
# (Some Linux distributions default to GOTOOLCHAIN=local, which would refuse.)
export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"

BACKEND="${SUPERSEARCH_BACKEND:-}"
if [ -z "$BACKEND" ]; then
	BACKEND=portable
	if [ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ] &&
		[ "$(sw_vers -productVersion | cut -d. -f1)" -ge 26 ] 2>/dev/null; then
		BACKEND=apple
	fi
fi

TAGS=""
if [ "$BACKEND" = apple ]; then
	# swiftc takes ~40 s: skip it when the helper is up to date.
	[ server/supersearch-helper -nt helper/main.swift ] || swiftc -O -o server/supersearch-helper helper/main.swift
else
	rm -f server/supersearch-helper
	[ "$(uname -s)" = Darwin ] && TAGS="portable"
fi

# The C SQLite (cgo) is about 2x faster per query. Without a C compiler, or if
# the cgo build fails, the pure-Go SQLite is used instead.
build() { (cd server && CGO_ENABLED="$1" go build -tags "$2" -trimpath -ldflags="-s -w" -o supersearch-server .); }
if command -v cc >/dev/null 2>&1 || command -v gcc >/dev/null 2>&1; then
	if ! build 1 "sqlite_fts5${TAGS:+,$TAGS}" 2>/tmp/supersearch-cgo.log; then
		echo "note: C build failed (see /tmp/supersearch-cgo.log); using the pure-Go SQLite" >&2
		build 0 "$TAGS"
	fi
else
	build 0 "$TAGS"
fi
