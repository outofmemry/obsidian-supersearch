#!/bin/sh
# Build the server + plugin and copy them into a vault.
#   ./install.sh                     # default vault below
#   ./install.sh "/path/to/vault"
set -eu
cd "$(dirname "$0")"

VAULT="${1:-${VAULT:-$HOME/Library/Mobile Documents/com~apple~CloudDocs/Notes}}"
DEST="$VAULT/.obsidian/plugins/supersearch"
[ -d "$VAULT/.obsidian" ] || { echo "not an Obsidian vault: $VAULT" >&2; exit 1; }

./build-helper.sh
(cd server && go build -o supersearch-server .)
(cd plugin && npm install --silent && npm run build --silent)

mkdir -p "$DEST"
# rm first: overwriting a running binary in place makes macOS (Apple Silicon
# code-signing cache) kill it and refuse to relaunch it. A new inode is safe.
rm -f "$DEST/supersearch-server" "$DEST/supersearch-helper"
cp server/supersearch-server server/supersearch-helper plugin/main.js plugin/manifest.json plugin/styles.css "$DEST/"
echo "installed to $DEST"
echo "reload it in Obsidian: Settings → Community plugins → toggle Supersearch off/on"
