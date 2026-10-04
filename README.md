# Supersearch

Fast search for your Obsidian vault that covers **everything**: notes, PDFs,
**text inside images**, Word/PowerPoint/Excel files, ebooks, code, and even
**spoken words in audio recordings**. It stays fast on big vaults and doesn't
slow Obsidian down.

Search plugins like Omnisearch keep their whole index and run OCR inside
Obsidian itself, so large vaults make the app slow and laggy. Supersearch moves
all of that work into a small local server that the plugin starts and stops
for you. Obsidian holds no index at all: the plugin is just a search box.

- Results in **under 1 ms for specific searches and about 5 ms for the broadest ones** (tested on a vault with 469 notes, 1,763 images and a 265-page PDF)
- Reads text inside images: **Apple's built-in OCR** on a Mac (about 24 images per second on an M2), **Tesseract** on Windows and Linux
- Runs **100% on your own computer**: no cloud, no accounts, nothing uploaded
- **iPhone, iPad and Android** search the same index through the `supersearch` command running on one of your computers

**Where it runs**

| Platform | How | Image OCR · PDFs · audio · Ask |
|---|---|---|
| **macOS 26+, Apple silicon** | Plugin starts its own server | Apple Vision · PDFKit · Apple speech · Apple Intelligence (nothing to install) |
| **Windows 10/11** | Plugin starts its own server | Tesseract · Poppler · whisper.cpp · Ollama |
| **Linux** | Plugin starts its own server | Tesseract · Poppler · whisper.cpp · Ollama |
| Older or Intel Macs | Plugin starts its own server | Same as Linux (via Homebrew) |
| **iPhone, iPad, Android** | Plugin connects to a computer running `supersearch` | Whatever that computer has |

On a Mac, Apple's frameworks are more accurate and faster than the open-source
tools (Vision read slide tables that Tesseract garbled, at about twice the speed),
and nothing extra is installed. Everywhere else the installer sets up the
open-source tools for you. Each one is optional: without it, only its own file
types go unread, and notes, Office files, ebooks and code are always searched.

---

## Contents

- [Features](#features)
- [Installation](#installation)
- [Usage](#usage)
- [Settings](#settings)
- [Architecture](#architecture)
- [Updating, uninstalling and troubleshooting](#updating-uninstalling-and-troubleshooting)
- [The supersearch command (phones, tablets, other computers)](#the-supersearch-command-phones-tablets-other-computers)
- [Development](#development)

---

## Features

**Search everything**

| What | Files | How it's read |
|---|---|---|
| Notes | `.md` | Split at headings: each section is its own result and opens at that line; remote `![](https://…)` images are fetched (up to 32 at once, across notes) and each is OCR'd as soon as it arrives, text cached by URL so edits only pay for new images |
| Images | `.png .jpg .jpeg .webp .heic .gif .tiff .bmp` | OCR: Apple Vision on macOS, Tesseract elsewhere (HEIC needs `heif-convert` there) |
| PDFs | `.pdf` | Text layer page by page: PDFKit on macOS, Poppler's `pdftotext` elsewhere (no OCR; scanned pages with no text contribute no content) |
| Audio and video | `.m4a .mp3 .wav .aac .flac .aiff .caf .mp4 .mov` | On-device speech-to-text: Apple's on macOS, whisper.cpp elsewhere |
| Office | `.docx .pptx .xlsx .doc .rtf .odt` | Built-in parsers; `.doc` uses macOS `textutil`, or `antiword` elsewhere |
| Web and ebooks | `.html .svg .epub` | Tags stripped |
| Text and code | `.txt .canvas .csv .json .yaml .go .py .ts .js …` | As is |

**Find it fast**

- **Results as you type**, with the matching words highlighted
- **Prefix matching**: `sched` finds "scheduling"
- **Typo tolerance**: `schedulng` shows results for "scheduling"
- **Filters**: by folder, file type or kind (see [Usage](#usage))
- **Smart ranking**: notes first; matches in headings and `#tags` beat body text; recently edited notes get a small boost
- **Image text opens the right note**: a match inside an image shows the note that embeds it, with a thumbnail, and opens that note scrolled to the image
- **PDFs open at the matching page**

**More**

- **Ask your vault**: ask a question and get an answer written from your own notes, with clickable sources, using a local language model (Apple's on-device one on macOS, [Ollama](https://ollama.com) on Windows and Linux)
- **Search in current note**, including the images inside it
- **Sidebar panel**: results stay visible while you click through them
- **Always up to date**: new, edited, renamed and deleted files are picked up within about a second. Nothing ever needs a manual reindex.
- **Light on your computer**: background OCR runs at low priority, uses 3 workers when plugged in and 1 on battery, and can be paused

---

## Installation

You need **Obsidian 1.4+**, **Git**, and nothing else up front: the installer
checks for everything it needs and offers to install what's missing.

### 1. Get the code

```bash
git clone <this-repo-url> supersearch
```

```bash
cd supersearch
```

### 2. Run the installer

**macOS and Linux**

```bash
./install.sh
```

**Windows** (in PowerShell, from the `supersearch` folder)

```powershell
powershell -ExecutionPolicy Bypass -File install.ps1
```

The installer walks through five steps:

1. **Your vault.** It lists the vaults Obsidian knows about; pick one by number
   or type a path (the folder that contains `.obsidian`). You can also pass it
   directly: `./install.sh "/path/to/vault"` or `install.ps1 -Vault "C:\path\to\vault"`.
2. **Your system.** macOS 26+ on Apple silicon uses Apple's frameworks; Windows,
   Linux and older Macs use the open-source tools.
3. **Requirements.** It checks each tool and offers to install the missing ones
   with your package manager (Homebrew, apt, dnf, pacman, zypper, apk, or winget
   on Windows). It asks before installing anything.
4. **The plugin.** It builds the server and the plugin and copies them into
   `<vault>/.obsidian/plugins/supersearch/`.
5. **The `supersearch` command.** It installs the CLI (see
   [below](#the-supersearch-command-phones-tablets-other-computers)) to
   `~/.local/bin` (on Windows, `%LOCALAPPDATA%\Programs\supersearch`, added to your PATH).

Add `-y` (`-Yes` on Windows) to accept every default without questions.

**What gets checked**

| Tool | Needed for | macOS 26+ | Windows / Linux / older Macs |
|---|---|---|---|
| Go 1.21+ | building the server | required | required |
| Node.js 18+ and npm | building the plugin | required | required |
| Xcode Command Line Tools | Apple OCR, PDF, speech, AI | required | n/a |
| Tesseract | text in images | built in | recommended |
| Poppler (`pdftotext`) | PDFs | built in | recommended |
| whisper.cpp + ffmpeg + a speech model (142 MB) | audio recordings | built in | optional; the installer offers it |
| `antiword` | old `.doc` files | built in | optional |
| A C compiler | about 2× faster index queries | built in | optional (a pure-Go SQLite is used without one) |
| [Ollama](https://ollama.com) with a model (`ollama pull llama3.2`) | Ask your vault | Apple Intelligence instead | optional |

On macOS, "Ask your vault" needs **Apple Intelligence** turned on (System
Settings → Apple Intelligence & Siri). Everything else works without it.

> An iCloud vault lives at
> `~/Library/Mobile Documents/com~apple~CloudDocs/<VaultName>`.
> Not sure where yours is? In Obsidian, open the vault switcher (bottom left),
> then use "Reveal vault in Finder" (or "Show in system explorer").

### 3. Turn it on in Obsidian

1. Open **Settings → Community plugins**.
2. If you see **Restricted mode**, click **Turn on community plugins**.
3. Find **Supersearch** in the list of installed plugins and switch it **on**.
   If it's missing, click the reload icon next to "Installed plugins".

That's it. The search server starts by itself. Your notes are searchable within
seconds, and images, PDFs and recordings are read in the background. The status
bar shows `Supersearch: indexing, N left` until everything is done, plus
`· M remote images` while images embedded from the web are still being
downloaded and read (a vault
with about 1,800 images takes about 1–2 minutes when plugged in).

### 4. Add hotkeys (recommended)

In **Settings → Hotkeys**, search for "Supersearch" and set:

- **Search everything**, e.g. <kbd>Cmd</kbd>+<kbd>Shift</kbd>+<kbd>F</kbd>
- **Ask your vault**, e.g. <kbd>Cmd</kbd>+<kbd>Shift</kbd>+<kbd>A</kbd>

---

## Usage

Open search with your hotkey, the magnifier icon in the left ribbon, or
<kbd>Cmd</kbd>+<kbd>P</kbd> → "Supersearch: Search everything". Press
<kbd>Enter</kbd> to open a result, or <kbd>Cmd</kbd>+<kbd>Enter</kbd> to open it in a new tab
(<kbd>Ctrl</kbd> instead of <kbd>Cmd</kbd> on Windows and Linux).

### Search syntax

| Type | Finds |
|---|---|
| `sched` | Words starting with "sched" |
| `DynamicArrays` | Spaceless words are split (`dynamic` + `arrays`), each typo-tolerant |
| `round robin` | Both words, anywhere |
| `"round robin"` | The exact phrase |
| `-passing` | Excludes results containing "passing" |
| `path:"Operating System"` | Only files whose path contains that text |
| `-path:Archive` | Skips files whose path contains that text |
| `ext:pdf` | Only that file extension |
| `in:note` `in:image` `in:pdf` `in:doc` `in:audio` `in:ocr` | Only that kind of result |

Combine them: `fcfs in:image path:"Operating System"`.

### Commands

| Command | What it does |
|---|---|
| **Search everything** | The main search box |
| **Search in current note (including its images)** | Search only the open note and the images inside it |
| **Open search panel** | The same search in the right sidebar |
| **Ask your vault** | Ask a question; the answer cites your notes as [1], [2], … |
| **Pause / resume background indexing (OCR)** | Stop background OCR for now, e.g. on battery |
| **Delete search index (rebuilds from the vault)** | Wipe all stored search data and rebuild it |

---

## Settings

**Settings → Supersearch**

| Setting | What it does |
|---|---|
| **Ignored folders** | One folder per line (e.g. `Templates`). Nothing inside is indexed. |
| **OCR languages** | e.g. `en-US,hi-IN`. Empty means English; `auto` means detect. Changing this re-reads images and recordings (not remote-image text; it refreshes when its note changes, or after Delete search index). |
| **Delete search index** | Same as the command. Your notes are never touched. |
| **Server URL / token** | Use a server started with the `supersearch` command (see [below](#the-supersearch-command-phones-tablets-other-computers)). Leave empty normally. |
| **Fall back to local server** | Desktop only: when the remote server is unreachable, run the local index instead and switch back automatically. |

---

## Architecture

```
 ┌───────────────────── Obsidian ──────────────────────┐
 │  Supersearch plugin (TypeScript)                    │
 │  search box · sidebar · ask · settings · status bar │
 │  forwards file changes; holds NO index              │
 └────────────┬────────────────────────────────────────┘
              │ HTTP on 127.0.0.1, random port, secret token
 ┌────────────▼────────────────────────────────────────┐
 │  supersearch-server (Go, one binary)                │
 │  • scans the vault at start and every 60 s          │
 │  • fast lane: notes, PDF text, docs                 │
 │  • slow lane: OCR and audio (3 workers on AC, 1 on  │
 │    battery)                                         │
 │  • SQLite FTS5 index: BM25 ranking, snippets,       │
 │    prefix search, typo correction                   │
 └────────────┬────────────────────────────────────────┘
              │ long-running child processes
 ┌────────────▼────────────────────────────────────────┐
 │  macOS: supersearch-helper (Swift)                  │
 │    Vision (OCR) · PDFKit · SpeechAnalyzer           │
 │    (speech-to-text) · FoundationModels (Ask)        │
 │  Windows / Linux: tesseract · pdftotext ·           │
 │    whisper-cli + ffmpeg · Ollama (Ask)              │
 └─────────────────────────────────────────────────────┘
```

The same Go server runs on every platform; only the bottom layer differs
(`server/backend_apple.go` vs `server/backend_portable.go`). Builds without a C
compiler (typical on Windows) use a pure-Go SQLite instead of the C one.

**Why it stays fast**

- **Nothing heavy runs inside Obsidian.** The index lives on disk in SQLite,
  not in Obsidian's memory, so startup and typing stay smooth whatever the vault size.
- **No uploads.** The server reads files straight from the vault folder.
- **Each file is read once.** Files are tracked by modified time and size.
  Renaming a file moves its entry without re-reading it, and OCR is never
  repeated unless the file changes.
- **Edits jump the queue.** Notes have their own lane, so a big OCR backlog
  never delays a note you just changed.
- **Models stay loaded while there's work.** On macOS, OCR keeps Vision's model
  in a long-running helper, about 2× faster than starting a process per image.
  Idle helpers stop after 30 seconds, so the server sits at about 30 MB.
- **Snippets only for what you see.** Results are ranked first, and the
  highlighted excerpts are built only for the rows that are shown.

**Where the data lives**

`<vault>/.obsidian/plugins/supersearch/index.nosync/index.db`

- A single SQLite file, usually 10–40 MB. It holds only extracted text, never copies of your files.
- It's a cache: delete it any time and it rebuilds.
- The `.nosync` suffix keeps iCloud from syncing it. Each device builds its own.
  If your vault is in git, add `index.nosync/` to `.gitignore`.

**Lifecycle and security**

- The plugin starts the server when Obsidian opens and stops it when Obsidian
  closes. If Obsidian crashes, the server and helpers notice and exit on their own.
- The server only listens on `127.0.0.1` (your own computer), on a random port, and
  every request needs a secret token that is generated at each start.
- Paths sent to the server are checked, so it can't read outside the vault.

**Code map**

| Path | What |
|---|---|
| `plugin/main.ts` | Obsidian plugin: UI, settings, starting and stopping the server |
| `server/main.go` | HTTP API, settings, startup |
| `server/index.go` | SQLite schema, vault scan, change tracking, job queue |
| `server/query.go` | Query parsing, filters, ranking, typo correction |
| `server/extract.go` | Reading each file type, image OCR and PDF text |
| `server/backend_apple.go` | macOS: OCR, PDF, speech and Ask through the Swift helper |
| `server/backend_portable.go` | Windows/Linux: tesseract, pdftotext, whisper.cpp, Ollama |
| `server/cli.go` | The `supersearch` command |
| `server/ask.go` | Ask your vault |
| `helper/main.swift` | Vision OCR, PDFKit, speech-to-text, on-device LLM |
| `install.sh`, `install.ps1`, `build.sh` | Installers (macOS/Linux, Windows) and the build |

---

## Updating, uninstalling and troubleshooting

**Update**: pull the latest code, run the installer again, then switch
Supersearch off and on in Community plugins.

**Uninstall**: Settings → Community plugins → Supersearch → uninstall. That
deletes the plugin folder, including the index. To remove the CLI too, delete
`~/.local/bin/supersearch`, `~/.local/share/supersearch` and
`~/.config/supersearch` (on Windows: `%LOCALAPPDATA%\Programs\supersearch` and
`%USERPROFILE%\.config\supersearch`).

**Troubleshooting**

| Problem | Try |
|---|---|
| "server binary not found" | Run `./install.sh` with the right vault path |
| "supersearch-helper is missing" (macOS) | Re-run `./install.sh`; check `swiftc --version` works |
| "some files can't be read until these are installed: tesseract …" | Install the named tools (re-run the installer), then restart Obsidian so it sees them |
| Windows: tools installed but still "missing" | Restart Obsidian: it only sees PATH changes made before it started |
| Linux: Obsidian installed as a Flatpak | Its sandbox can't see tesseract and friends. Run `supersearch` and set the plugin's Server URL to `http://127.0.0.1:54999` with the token it prints |
| Images in another language aren't read (Windows/Linux) | Install that Tesseract language (e.g. `tesseract-ocr-hin`, `tesseract-langpack-hin`) and set **OCR languages** to `en-US,hi-IN` |
| "Ask is not available" (Windows/Linux) | Install [Ollama](https://ollama.com), then `ollama pull llama3.2`. Another model: set `SUPERSEARCH_OLLAMA_MODEL` |
| No results right after installing | Wait for the status bar to clear, then search again |
| Results look wrong or stale | Run **Delete search index**; it rebuilds in a minute or two |
| "Ask" says Apple Intelligence isn't available | Turn it on in System Settings → Apple Intelligence & Siri |
| Fan spins during the first index | Normal for a minute or two while images are read. Use **Pause** or run on battery (1 worker). |
| Drive image text not found | Use a direct image link (`uc?export=view`, `thumbnail`), not the viewer page; the link must serve image bytes without login |
| Is it still running after quitting? | `pgrep -lf supersearch` (Windows: `tasklist \| findstr supersearch`) prints nothing once Obsidian has quit |

Server errors appear in Obsidian's developer console
(<kbd>Cmd</kbd>+<kbd>Option</kbd>+<kbd>I</kbd>, or <kbd>Ctrl</kbd>+<kbd>Shift</kbd>+<kbd>I</kbd>), prefixed with `supersearch-server:`.

---

## The supersearch command (phones, tablets, other computers)

Obsidian on iPhone, iPad and Android can't run the server, so the plugin there
connects to one running on a computer you own that has the same vault (synced
with iCloud, Obsidian Sync, Syncthing or anything else). The installer puts a
`supersearch` command on that computer for this:

```bash
supersearch
```

```
Supersearch is running

  Vault   /Users/me/Notes
  URL     http://127.0.0.1:54999
  Token   3f9c…e81a
  Index   ~/.config/supersearch/vaults/Notes-1a2b3c4d
```

- The **token** is generated once and saved in `~/.config/supersearch/token`:
  every run prints the same one. `supersearch token -new` replaces it.
- The **index** lives in `~/.config/supersearch`, separate from the plugin's own,
  and survives restarts. The vault is remembered, so later runs are just `supersearch`.
- It rescans every minute, so changes synced from other devices show up.

Other commands:

| Command | What it does |
|---|---|
| `supersearch /path/to/vault` | Serve another vault (remembered from then on) |
| `supersearch -lan` | Listen on your home network too, and print those URLs |
| `supersearch -listen 127.0.0.1:8765` | Another port |
| `supersearch status` | Indexing progress of the running server |
| `supersearch search fcfs in:image` | Search from the terminal |
| `supersearch token` | Print the token |
| `supersearch vault` | Print (or, with a path, set) the default vault |

**Connect a phone or tablet.** The safest way is [Tailscale](https://tailscale.com)
(private and encrypted, works away from home). With Tailscale on both devices:

```bash
tailscale serve --bg 54999
```

Then in Obsidian on the phone, open **Settings → Supersearch**, set **Server URL**
to `https://<your-computer>.<your-tailnet>.ts.net` (the `supersearch` banner
prints it when Tailscale is running), and paste the token into **Server token**.
On your home Wi-Fi only, `supersearch -lan` and its `http://192.168…:54999` URL
also work.

**Use it on the same computer.** Desktop Obsidian can use the CLI's server
instead of starting its own: set **Server URL** to `http://127.0.0.1:54999`.
That's also the fix for Flatpak installs of Obsidian on Linux.

> Don't expose the port to the internet: the token is the only protection.

---

## Development

```bash
make test                      # Go tests, including real OCR, PDF, audio and Ask
```

```bash
make test-portable             # the Windows/Linux backend and pure-Go SQLite, run on this machine
```

```bash
make cross                     # server binaries for Linux, Windows and Intel Macs into dist/
```

```bash
./server.sh -q "some words"    # one-off search from the terminal
```

```bash
./server.sh                    # run the server by hand (the plugin normally does this)
```

```bash
./server.sh -d                 # same, but detached in the background
```

```bash
./server.sh -k                 # stop the background server
```

Don't leave `./server.sh` running while Obsidian is open: two servers on one
vault do the same work twice.

Design notes, measurements and known limits are in [PLAN.md](PLAN.md).

---

## License

[MIT](LICENSE)
