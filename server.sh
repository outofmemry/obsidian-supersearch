#!/bin/sh
# Run the server by hand (the plugin normally starts its own copy).
#   ./server.sh                          # serve the default vault
#   ./server.sh "/path/to/vault"
#   ./server.sh -q "some words"          # one-off search from the terminal, then exit
# Extra flags pass through, e.g.  ./server.sh -ocr-workers 1
# OCR language, ignored folders etc. come from the plugin settings (data.json).
set -eu
cd "$(dirname "$0")"

DEFAULT_VAULT="$HOME/Library/Mobile Documents/com~apple~CloudDocs/Notes"
case "${1:-}" in
	""|-*) VAULT="${VAULT:-$DEFAULT_VAULT}" ;;
	*)     VAULT="$1"; shift ;;
esac
[ -d "$VAULT" ] || { echo "vault not found: $VAULT" >&2; exit 1; }

./build-helper.sh
# sqlite_fts5: full-text search in the C SQLite driver. Stripped: a third smaller.
(cd server && CGO_ENABLED=1 go build -tags sqlite_fts5 -trimpath -ldflags="-s -w" -o supersearch-server .)

if [ "${1:-}" = "-q" ]; then
	exec server/supersearch-server -vault "$VAULT" -query "$2"
fi

# Auth token comes from .env in the repo root (see .env.example), or from an
# exported SUPERSEARCH_TOKEN which wins. There is no default: the server
# rejects every request without the token (401), so starting without one
# would only look like it works.
if [ -z "${SUPERSEARCH_TOKEN:-}" ] && [ -f .env ]; then
	set -a
	# shellcheck disable=SC1091
	. ./.env
	set +a
fi
if [ -z "${SUPERSEARCH_TOKEN:-}" ]; then
	echo "SUPERSEARCH_TOKEN is not set: put it in ./.env (see .env.example) or export it" >&2
	exit 1
fi
export SUPERSEARCH_TOKEN

# The port is random; it's printed as "LISTENING <port>". Then:
#   curl -H "Authorization: Bearer $SUPERSEARCH_TOKEN" "http://127.0.0.1:<port>/search?q=hello"
echo "vault: $VAULT"
if [ -f .env ]; then echo "token: loaded from .env"; else echo "token: from environment"; fi
exec server/supersearch-server -vault "$VAULT" "$@"
