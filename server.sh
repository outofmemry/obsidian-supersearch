#!/bin/sh
# Run the server by hand (the plugin normally starts its own copy).
#   ./server.sh                          # serve the default vault (foreground)
#   ./server.sh "/path/to/vault"
#   ./server.sh -d                       # serve in the background (detached)
#   ./server.sh -k                       # stop the background server
#   ./server.sh -q "some words"          # one-off search from the terminal, then exit
# Extra flags pass through, e.g.  ./server.sh -ocr-workers 1
# OCR language, ignored folders etc. come from the plugin settings (data.json).
set -eu
cd "$(dirname "$0")"

DETACH=0
KILL=0
QUERY=""
VAULT_ARG=""
PASS=""
while [ $# -gt 0 ]; do
	case "$1" in
		-d|--detach) DETACH=1 ;;
		-k|--kill) KILL=1 ;;
		-q) QUERY="${2:-}"; [ -n "$QUERY" ] || { echo "-q needs a query string" >&2; exit 1; }; shift ;;
		-*) PASS="$PASS $1"; case "$1" in -listen|-ocr-workers) PASS="$PASS ${2:-}"; shift ;; esac ;;
		*) if [ -z "$VAULT_ARG" ]; then VAULT_ARG="$1"; else PASS="$PASS $1"; fi ;;
	esac
	shift
done

PIDF="server/supersearch-server.pid"
LOGF="server/supersearch-server.log"

is_running() { # is_running <pid>: true if it's a live supersearch-server
	kill -0 "$1" 2>/dev/null && ps -p "$1" -o comm= 2>/dev/null | grep -q supersearch-server
}

if [ "$KILL" = 1 ]; then
	[ "$DETACH" = 0 ] || { echo "-d and -k can't be combined" >&2; exit 1; }
	[ -z "$QUERY" ] || { echo "-q and -k can't be combined" >&2; exit 1; }
	if [ ! -f "$PIDF" ]; then echo "no background server running"; exit 0; fi
	PID=$(cat "$PIDF")
	if ! is_running "$PID"; then
		rm -f "$PIDF"
		echo "no background server running (stale pid $PID removed)"
		exit 0
	fi
	kill "$PID"
	for _ in 1 2 3 4 5 6 7 8 9 10; do is_running "$PID" || break; sleep 1; done
	if is_running "$PID"; then kill -9 "$PID"; fi
	rm -f "$PIDF"
	echo "stopped background server (pid $PID)"
	exit 0
fi

DEFAULT_VAULT="$HOME/Library/Mobile Documents/com~apple~CloudDocs/Notes"
VAULT="${VAULT_ARG:-${VAULT:-$DEFAULT_VAULT}}"
[ -d "$VAULT" ] || { echo "vault not found: $VAULT" >&2; exit 1; }

./build-helper.sh
# sqlite_fts5: full-text search in the C SQLite driver. Stripped: a third smaller.
(cd server && CGO_ENABLED=1 go build -tags sqlite_fts5 -trimpath -ldflags="-s -w" -o supersearch-server .)

if [ -n "$QUERY" ]; then
	# shellcheck disable=SC2086
	exec server/supersearch-server -vault "$VAULT" -query "$QUERY"
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

# Fixed local port so manual runs (and phone/Tailscale setups) keep the
# same address. Override per-run with ./server.sh -listen 127.0.0.1:PORT
# (a later -listen flag wins) or via SUPERSEARCH_LISTEN in .env.
LISTEN="${SUPERSEARCH_LISTEN:-127.0.0.1:54999}"

if [ "$DETACH" = 1 ]; then
	if [ -f "$PIDF" ] && is_running "$(cat "$PIDF")"; then
		echo "background server already running (pid $(cat "$PIDF")); stop it first: ./server.sh -k" >&2
		exit 1
	fi
	rm -f "$PIDF"
	LOGSZ=0
	[ -f "$LOGF" ] && LOGSZ=$(wc -c <"$LOGF")
	# shellcheck disable=SC2086
	nohup server/supersearch-server -vault "$VAULT" -listen "$LISTEN" $PASS >>"$LOGF" 2>&1 &
	echo $! > "$PIDF"
	PID=$(cat "$PIDF")
	slept=0
	while ! tail -c +$((LOGSZ + 1)) "$LOGF" 2>/dev/null | grep -q "^LISTENING "; do
		sleep 1
		slept=$((slept + 1))
		if ! is_running "$PID"; then
			echo "background server failed to start; last log lines:" >&2
			tail -n 10 "$LOGF" >&2
			rm -f "$PIDF"
			exit 1
		fi
		if [ "$slept" -ge 15 ]; then
			echo "background server did not report LISTENING within 15s; check $LOGF" >&2
			rm -f "$PIDF"
			exit 1
		fi
	done
	echo "background server started (pid $PID)"
	echo "vault: $VAULT"
	echo "listen: $LISTEN"
	echo "log: $LOGF"
	echo "stop it with: ./server.sh -k"
	exit 0
fi

# The server prints "LISTENING <port>" on start. Then:
#   curl -H "Authorization: Bearer $SUPERSEARCH_TOKEN" "http://127.0.0.1:54999/search?q=hello"
echo "vault: $VAULT"
echo "listen: $LISTEN"
if [ -f .env ]; then echo "token: loaded from .env"; else echo "token: from environment"; fi
# shellcheck disable=SC2086
exec server/supersearch-server -vault "$VAULT" -listen "$LISTEN" $PASS
