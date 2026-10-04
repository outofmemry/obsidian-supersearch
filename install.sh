#!/bin/sh
# Install Supersearch on macOS or Linux (Windows: install.ps1).
#   ./install.sh                       # asks which vault
#   ./install.sh "/path/to/vault"
#   ./install.sh -y "/path/to/vault"   # no questions: also installs missing packages
#
# 1. picks the vault   2. checks the OS   3. checks (and offers to install) what's needed
# 4. builds and installs the plugin into the vault   5. installs the `supersearch` CLI
set -eu
cd "$(dirname "$0")"

YES=0
VAULT=""
for a in "$@"; do
	case "$a" in
		-y|--yes) YES=1 ;;
		-h|--help) sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) VAULT="$a" ;;
	esac
done

if [ -t 1 ]; then B=$(printf '\033[1m'); G=$(printf '\033[32m'); Y=$(printf '\033[33m'); R=$(printf '\033[31m'); N=$(printf '\033[0m')
else B=""; G=""; Y=""; R=""; N=""; fi
step() { printf '\n%s==> %s%s\n' "$B" "$1" "$N"; }
ok() { printf '  %s✓%s %s\n' "$G" "$N" "$1"; }
warn() { printf '  %s!%s %s\n' "$Y" "$N" "$1"; }
die() { printf '%serror:%s %s\n' "$R" "$N" "$1" >&2; exit 1; }
has() { command -v "$1" >/dev/null 2>&1; }
# Questions read the terminal, so they also work under `curl … | sh`.
TTY=0
if [ "$YES" = 0 ] && { : </dev/tty; } 2>/dev/null; then TTY=1; fi
ask() { # ask "question" default(y/n) → 0 for yes; -y takes the default
	if [ "$YES" = 1 ]; then [ "$2" = y ]; return; fi
	if [ "$TTY" = 0 ]; then [ "$2" = y ]; return; fi
	if [ "$2" = y ]; then hint="[Y/n]"; else hint="[y/N]"; fi
	printf '%s %s ' "$1" "$hint" >/dev/tty
	read -r ans </dev/tty || ans=""
	case "$ans" in [Yy]*) return 0 ;; [Nn]*) return 1 ;; *) [ "$2" = y ] ;; esac
}

# ---------------------------------------------------------------- 1. OS
step "Checking your system"
OS=$(uname -s)
ARCH=$(uname -m)
BACKEND=portable
case "$OS" in
	Darwin)
		MACOS=$(sw_vers -productVersion)
		if [ "$ARCH" = arm64 ] && [ "${MACOS%%.*}" -ge 26 ]; then
			BACKEND=apple
			ok "macOS $MACOS on Apple silicon: Apple Vision OCR, PDFKit, on-device speech and AI"
		else
			warn "macOS $MACOS on $ARCH: Apple's OCR/AI need macOS 26 on Apple silicon, so the open-source tools are used"
		fi ;;
	Linux)
		DISTRO=$( (. /etc/os-release 2>/dev/null && echo "${PRETTY_NAME:-Linux}") || echo Linux)
		ok "$DISTRO ($ARCH)" ;;
	MINGW*|MSYS*|CYGWIN*)
		die "on Windows, run install.ps1 in PowerShell instead:  powershell -ExecutionPolicy Bypass -File install.ps1" ;;
	*)
		warn "$OS is untested; trying the Linux setup" ;;
esac

# Package manager, and its package names: required tools first, then optional ones.
PM=""; SUDO=""
[ "$(id -u)" = 0 ] || SUDO="sudo"
if [ "$OS" = Darwin ]; then has brew && PM=brew
elif has apt-get; then PM=apt
elif has dnf; then PM=dnf
elif has pacman; then PM=pacman
elif has zypper; then PM=zypper
elif has apk; then PM=apk
elif has brew; then PM=brew
fi
pkg() { # pkg <tool> → package name(s) for this package manager ("" = not packaged)
	case "$PM:$1" in
		brew:go) echo go ;; brew:node) echo node ;; brew:tesseract) echo tesseract ;; brew:pdftotext) echo poppler ;;
		brew:ffmpeg) echo ffmpeg ;; brew:whisper-cli) echo whisper-cpp ;; brew:antiword) echo antiword ;;
		apt:go) echo golang-go ;; apt:node) echo "nodejs npm" ;; apt:tesseract) echo tesseract-ocr ;;
		apt:pdftotext) echo poppler-utils ;; apt:ffmpeg) echo ffmpeg ;; apt:antiword) echo antiword ;; apt:cc) echo gcc ;;
		dnf:go) echo golang ;; dnf:node) echo "nodejs npm" ;; dnf:tesseract) echo "tesseract tesseract-langpack-eng" ;;
		dnf:pdftotext) echo poppler-utils ;; dnf:ffmpeg) echo ffmpeg-free ;; dnf:antiword) echo antiword ;; dnf:cc) echo gcc ;;
		pacman:go) echo go ;; pacman:node) echo "nodejs npm" ;; pacman:tesseract) echo "tesseract tesseract-data-eng" ;;
		pacman:pdftotext) echo poppler ;; pacman:ffmpeg) echo ffmpeg ;; pacman:cc) echo gcc ;;
		zypper:go) echo go ;; zypper:node) echo "nodejs npm" ;; zypper:tesseract) echo "tesseract-ocr tesseract-ocr-traineddata-english" ;;
		zypper:pdftotext) echo poppler-tools ;; zypper:ffmpeg) echo ffmpeg ;; zypper:antiword) echo antiword ;; zypper:cc) echo gcc ;;
		apk:go) echo go ;; apk:node) echo "nodejs npm" ;; apk:tesseract) echo "tesseract-ocr tesseract-ocr-data-eng" ;;
		apk:pdftotext) echo poppler-utils ;; apk:ffmpeg) echo ffmpeg ;; apk:antiword) echo antiword ;; apk:cc) echo "gcc musl-dev" ;;
	esac
}
pm_install() { # pm_install <packages…>
	case "$PM" in
		brew) brew install "$@" ;;
		apt) $SUDO apt-get update -qq && $SUDO apt-get install -y --no-install-recommends "$@" ;;
		dnf) $SUDO dnf install -y "$@" ;;
		pacman) $SUDO pacman -S --needed --noconfirm "$@" ;;
		zypper) $SUDO zypper --non-interactive install "$@" ;;
		apk) $SUDO apk add "$@" ;;
		*) return 1 ;;
	esac
}

# ---------------------------------------------------------------- 2. vault
step "Choosing the vault"
# Obsidian's own list of vaults, wherever this install keeps it.
list_vaults() {
	for f in "$HOME/Library/Application Support/obsidian/obsidian.json" \
		"${XDG_CONFIG_HOME:-$HOME/.config}/obsidian/obsidian.json" \
		"$HOME/.var/app/md.obsidian.Obsidian/config/obsidian/obsidian.json" \
		"$HOME/snap/obsidian/current/.config/obsidian/obsidian.json"; do
		[ -f "$f" ] || continue
		grep -o '"path" *: *"[^"]*"' "$f" | sed 's/^"path" *: *"//; s/"$//; s#\\/#/#g'
	done | awk '!seen[$0]++' | while IFS= read -r v; do [ -d "$v/.obsidian" ] && printf '%s\n' "$v"; done
}
if [ -z "$VAULT" ]; then
	VAULTS=$(list_vaults || true)
	if [ "$TTY" = 0 ]; then
		COUNT=$(printf '%s' "$VAULTS" | grep -c . || true)
		[ "$COUNT" = 1 ] || die "pass the vault path: ./install.sh /path/to/vault"
		VAULT=$VAULTS
	else
		if [ -n "$VAULTS" ]; then
			echo "  Your Obsidian vaults:"
			printf '%s\n' "$VAULTS" | awk '{ printf "    %d) %s\n", NR, $0 }'
			printf '  Pick a number, or type a path: ' >/dev/tty
		else
			printf '  Path of your vault (the folder that contains .obsidian): ' >/dev/tty
		fi
		read -r VAULT </dev/tty || VAULT=""
		case "$VAULT" in
			''|*[!0-9]*) ;;
			*) VAULT=$(printf '%s\n' "$VAULTS" | sed -n "${VAULT}p") ;;
		esac
	fi
fi
case "$VAULT" in "~"*) VAULT="$HOME${VAULT#\~}" ;; esac
VAULT=${VAULT%/}
[ -n "$VAULT" ] || die "no vault chosen"
[ -d "$VAULT/.obsidian" ] || die "not an Obsidian vault (no .obsidian folder): $VAULT"
VAULT=$(cd "$VAULT" && pwd)
ok "$VAULT"

# ---------------------------------------------------------------- 3. requirements
step "Checking requirements"
go_ok() { # Go 1.21+ fetches the exact toolchain go.mod asks for by itself
	has go || return 1
	v=$(go env GOVERSION 2>/dev/null | sed 's/^go//')
	[ "${v%%.*}" -gt 1 ] 2>/dev/null && return 0
	minor=$(echo "$v" | cut -d. -f2)
	[ "${minor:-0}" -ge 21 ] 2>/dev/null
}
node_ok() { has node && has npm && [ "$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)" -ge 18 ]; }

note() { if eval "$1"; then ok "$2"; else warn "$3"; fi; }
NEED=""   # tools to install
MISSING_REQ=""
check() { # check <tool> <what it's for> <required|optional> <test command>
	if eval "$4"; then ok "$1"; return; fi
	if [ "$3" = required ]; then warn "$1 is missing (needed: $2)"; MISSING_REQ="$MISSING_REQ $1"
	else warn "$1 is missing ($2)"; fi
	NEED="$NEED $1"
}
check go "to build the server" required go_ok
check node "Node.js 18+ and npm, to build the plugin" required node_ok
if [ "$BACKEND" = apple ]; then
	check swiftc "Xcode Command Line Tools, for Apple OCR/PDF/speech" required "has swiftc"
else
	check tesseract "reads text in images" optional "has tesseract"
	check pdftotext "reads PDFs; part of poppler" optional "has pdftotext"
	note "has whisper-cli || has whisper-cpp" "whisper-cli" "whisper-cli is missing (transcribes audio; optional)"
	note "has ffmpeg" "ffmpeg" "ffmpeg is missing (decodes audio for whisper; optional)"
	note "has antiword || has catdoc" "antiword" "antiword is missing (old .doc files; optional)"
	note "has cc || has gcc" "C compiler (faster search index)" "no C compiler: the pure-Go SQLite is used (a bit slower, still fast)"
	note "has ollama" "ollama (Ask your vault)" "ollama is missing (only for Ask your vault: https://ollama.com)"
fi

if [ "$BACKEND" = apple ] && echo "$MISSING_REQ" | grep -q swiftc; then
	xcode-select --install 2>/dev/null || true
	die "install the Xcode Command Line Tools (a window just opened), then run ./install.sh again"
fi

if [ -n "$NEED" ]; then
	PKGS=""
	for t in $NEED; do PKGS="$PKGS $(pkg "$t")"; done
	# shellcheck disable=SC2086,SC2116 # word splitting squeezes the spaces
	PKGS=$(echo $PKGS)
	if [ -n "$PKGS" ] && ask "  Install $PKGS with $PM?" y; then
		# shellcheck disable=SC2086
		pm_install $PKGS || warn "package install failed; continuing with what's there"
	elif [ -z "$PM" ]; then
		warn "no known package manager: install the tools above yourself"
	fi
fi
go_ok || die "Go 1.21 or newer is needed: https://go.dev/dl (your package manager's may be too old)"
node_ok || die "Node.js 18 or newer is needed: https://nodejs.org"

# Audio transcription (Linux / older Macs): whisper.cpp plus a speech model.
CONF="${SUPERSEARCH_CONFIG_DIR:-$HOME/.config/supersearch}"
if [ "$BACKEND" = portable ] && ! ls "$CONF"/models/ggml-*.bin >/dev/null 2>&1; then
	AUDIO_PKGS=""
	has whisper-cli || has whisper-cpp || AUDIO_PKGS=$(pkg whisper-cli)
	has ffmpeg || [ -z "$(pkg ffmpeg)" ] || AUDIO_PKGS="$AUDIO_PKGS $(pkg ffmpeg)"
	if [ -n "$AUDIO_PKGS" ] && { has whisper-cli || has whisper-cpp || [ -n "$(pkg whisper-cli)" ]; } &&
		ask "  Transcribe audio recordings too? Installs$(echo " $AUDIO_PKGS" | sed 's/  */ /g')" n; then
		# shellcheck disable=SC2086
		pm_install $AUDIO_PKGS || warn "could not install the audio tools"
	elif ! has whisper-cli && ! has whisper-cpp; then
		warn "audio search needs whisper.cpp: https://github.com/ggml-org/whisper.cpp (put whisper-cli on PATH, then re-run)"
	fi
	if has whisper-cli || has whisper-cpp; then
		if ask "  Download the speech model for audio search (ggml-base, 142 MB)?" y; then
			mkdir -p "$CONF/models"
			URL=https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.bin
			if { if has curl; then curl -fL --progress-bar -o "$CONF/models/ggml-base.bin.part" "$URL"
				else wget -q --show-progress -O "$CONF/models/ggml-base.bin.part" "$URL"; fi; } &&
				mv "$CONF/models/ggml-base.bin.part" "$CONF/models/ggml-base.bin"; then
				ok "speech model saved"
			else
				warn "model download failed; audio files won't be transcribed"
			fi
		fi
	fi
fi

# ---------------------------------------------------------------- 4. build + plugin
step "Building"
SUPERSEARCH_BACKEND=$BACKEND ./build.sh
ok "server"
(cd plugin && npm install --silent --no-audit --no-fund && npm run build --silent)
ok "plugin"

step "Installing the plugin into the vault"
DEST="$VAULT/.obsidian/plugins/supersearch"
mkdir -p "$DEST"
# rm first: overwriting a running binary in place makes macOS (Apple Silicon
# code-signing cache) kill it and refuse to relaunch it. A new inode is safe.
rm -f "$DEST/supersearch-server" "$DEST/supersearch-helper"
cp server/supersearch-server plugin/main.js plugin/manifest.json plugin/styles.css "$DEST/"
[ ! -f server/supersearch-helper ] || cp server/supersearch-helper "$DEST/"
ok "$DEST"

# ---------------------------------------------------------------- 5. CLI
step "Installing the supersearch command"
SHARE="${XDG_DATA_HOME:-$HOME/.local/share}/supersearch"
BIN="$HOME/.local/bin"
mkdir -p "$SHARE" "$BIN"
rm -f "$SHARE/supersearch" "$SHARE/supersearch-helper"
cp server/supersearch-server "$SHARE/supersearch"
[ ! -f server/supersearch-helper ] || cp server/supersearch-helper "$SHARE/"
ln -sf "$SHARE/supersearch" "$BIN/supersearch"
"$BIN/supersearch" vault "$VAULT"
ok "$BIN/supersearch"
case ":$PATH:" in
	*":$BIN:"*) ;;
	*)
		RC="$HOME/.profile"
		case "${SHELL:-}" in */zsh) RC="$HOME/.zshrc" ;; */bash) RC="$HOME/.bashrc" ;; esac
		if ask "  $BIN is not on your PATH. Add it in $RC?" y; then
			# shellcheck disable=SC2016 # expanded by the shell that reads $RC
			printf '\nexport PATH="$HOME/.local/bin:$PATH"\n' >>"$RC"
			ok "added; open a new terminal for it to take effect"
		else
			warn "add $BIN to your PATH to run supersearch by name"
		fi ;;
esac

# ---------------------------------------------------------------- done
step "Done"
echo "  In Obsidian: Settings → Community plugins → turn on Supersearch"
echo "  (already on? toggle it off and on to load the new version)."
if [ "$OS" = Linux ] && has flatpak && flatpak info md.obsidian.Obsidian >/dev/null 2>&1; then
	warn "Obsidian is a Flatpak here: its sandbox can't see tesseract and friends."
	echo "    Run the server outside it with  supersearch  and set the plugin's Server URL"
	echo "    to http://127.0.0.1:54999 with the token from  supersearch token"
fi
echo
echo "  To search from your phone, tablet or another computer, run:  supersearch"
echo "  It prints the URL and token to paste into the plugin's settings there."
