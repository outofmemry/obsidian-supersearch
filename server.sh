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
(cd server && go build -o supersearch-server .)

if [ "${1:-}" = "-q" ]; then
	exec server/supersearch-server -vault "$VAULT" -query "$2"
fi

# The port is random; it's printed as "LISTENING <port>". Then:
#   curl -H "Authorization: Bearer $SUPERSEARCH_TOKEN" "http://127.0.0.1:<port>/search?q=hello"
export SUPERSEARCH_TOKEN="${SUPERSEARCH_TOKEN:-dev}"
echo "vault: $VAULT"
echo "token: $SUPERSEARCH_TOKEN"
exec server/supersearch-server -vault "$VAULT" "$@"
