# Supersearch

Fast search for your Obsidian vault that covers **everything**: notes, PDFs,
**text inside images**, Word/PowerPoint/Excel files, ebooks, code, and even
**spoken words in audio recordings**. It stays fast on big vaults and doesn't
slow Obsidian down.

Search plugins like Omnisearch keep their whole index and run OCR inside
Obsidian itself, so large vaults make the app slow and laggy. Supersearch moves
all of that work into a small local server that the plugin starts and stops
for you. Obsidian holds no index at all: the plugin is just a search box.

- Keyword results in about **1–3 ms** (tested on a vault with 469 notes, 1,763 images and a 265-page PDF)
- Reads text inside images with **Apple's built-in OCR**, about 24 images per second on an M2
- Runs **100% on your Mac**: no cloud, no accounts, nothing uploaded

> [!IMPORTANT]
> **macOS only (Apple silicon, macOS 26+). There is no Windows or Linux version.**
>
> Supersearch reads images with **Apple's Vision framework** instead of
> Tesseract, the open-source OCR engine most tools use. Vision is built into
> macOS, so there's nothing to install, and it's both more accurate and faster.
> On a real vault it read slide tables that Tesseract garbled, at about twice
> Tesseract's speed. PDFs (PDFKit), audio transcription (SpeechAnalyzer) and
> Ask your vault (Apple's on-device language model) also use Apple frameworks
> that only exist on macOS.

---

## Contents

- [Features](#features)
- [Installation](#installation)
- [Usage](#usage)
- [Settings](#settings)
- [Architecture](#architecture)
- [Updating, uninstalling and troubleshooting](#updating-uninstalling-and-troubleshooting)
- [Search from your phone (optional)](#search-from-your-phone-optional)
- [Development](#development)

---

## Features

**Search everything**

| What | Files | How it's read |
|---|---|---|
| Notes | `.md` | Split at headings: each section is its own result and opens at that line |
| Images | `.png .jpg .jpeg .webp .heic .gif .tiff .bmp` | Apple Vision OCR |
| PDFs | `.pdf` | Text layer page by page; scanned pages are OCR'd; optionally text inside figures |
| Audio and video | `.m4a .mp3 .wav .aac .flac .aiff .caf .mp4 .mov` | Apple on-device speech-to-text |
| Office | `.docx .pptx .xlsx .doc .rtf .odt` | Built-in parsers and macOS `textutil` |
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

- **Ask your vault**: ask a question and get an answer written from your own notes, with clickable sources, using Apple's on-device language model
- **Search in current note**, including the images inside it
- **Sidebar panel**: results stay visible while you click through them
- **Always up to date**: new, edited, renamed and deleted files are picked up within about a second. Nothing ever needs a manual reindex.
- **Light on your Mac**: background OCR runs at low priority, uses 3 workers when plugged in and 1 on battery, and can be paused

---

## Installation

### Requirements

Supersearch runs **only on macOS**; see the note at the top for why.

| | Version | Check with |
|---|---|---|
| macOS | **26 (Tahoe) or later**, Apple silicon | `sw_vers` |
| Xcode Command Line Tools (for `swiftc`) | current | `swiftc --version` |
| Go | 1.27 or later | `go version` |
| Node.js and npm | 18 or later | `node --version` |
| Obsidian | 1.4 or later | |

"Ask your vault" also needs **Apple Intelligence** turned on (System Settings →
Apple Intelligence & Siri). Everything else works without it.

Install anything that's missing:

```bash
xcode-select --install
```

```bash
brew install go node
```

### 1. Get the code

```bash
git clone <this-repo-url> supersearch
```

```bash
cd supersearch
```

### 2. Build and install into your vault

Pass the path of your vault (the folder that contains `.obsidian`):

```bash
./install.sh "/path/to/your/vault"
```

This builds three pieces and copies them into
`<vault>/.obsidian/plugins/supersearch/`:

- `supersearch-helper`: the Swift OCR, PDF, speech and AI helper. The first build takes about 40 seconds.
- `supersearch-server`: the Go search server
- `main.js`, `manifest.json`, `styles.css`: the Obsidian plugin

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
bar shows `Supersearch: indexing, N left` until everything is done (a vault
with about 1,800 images takes about 1–2 minutes when plugged in).

### 4. Add hotkeys (recommended)

In **Settings → Hotkeys**, search for "Supersearch" and set:

- **Search everything**, e.g. <kbd>Cmd</kbd>+<kbd>Shift</kbd>+<kbd>F</kbd>
- **Ask your vault**, e.g. <kbd>Cmd</kbd>+<kbd>Shift</kbd>+<kbd>A</kbd>

---

## Usage

Open search with your hotkey, the magnifier icon in the left ribbon, or
<kbd>Cmd</kbd>+<kbd>P</kbd> → "Supersearch: Search everything". Press
<kbd>Enter</kbd> to open a result, or <kbd>Cmd</kbd>+<kbd>Enter</kbd> to open it in a new tab.

### Search syntax

| Type | Finds |
|---|---|
| `sched` | Words starting with "sched" |
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
| **OCR languages** | e.g. `en-US,hi-IN`. Empty means English; `auto` means detect. Changing this re-reads images and recordings. |
| **OCR figures inside PDFs** | Also read text inside images on PDF pages that already have text. Slower. Changing this re-reads PDFs only. |
| **Delete search index** | Same as the command. Your notes are never touched. |
| **Server URL / token** | Use a server on another machine (see [phone setup](#search-from-your-phone-optional)). Leave empty normally. |

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
 │  supersearch-helper (Swift, macOS frameworks)       │
 │  Vision (OCR) · PDFKit · SpeechAnalyzer             │
 │  (speech-to-text) · FoundationModels (Ask)          │
 └─────────────────────────────────────────────────────┘
```

**Why it stays fast**

- **Nothing heavy runs inside Obsidian.** The index lives on disk in SQLite,
  not in Obsidian's memory, so startup and typing stay smooth whatever the vault size.
- **No uploads.** The server reads files straight from the vault folder.
- **Each file is read once.** Files are tracked by modified time and size.
  Renaming a file moves its entry without re-reading it, and OCR is never
  repeated unless the file changes.
- **Edits jump the queue.** Notes have their own lane, so a big OCR backlog
  never delays a note you just changed.
- **Models stay loaded.** OCR keeps Vision's model in a long-running helper,
  about 2× faster than starting a process per image.
- **Resumable.** Big PDFs are OCR'd 8 pages at a time and saved as they go, so
  closing Obsidian never loses progress.

**Where the data lives**

`<vault>/.obsidian/plugins/supersearch/index.nosync/index.db`

- A single SQLite file, usually 10–40 MB. It holds only extracted text, never copies of your files.
- It's a cache: delete it any time and it rebuilds.
- The `.nosync` suffix keeps iCloud from syncing it. Each device builds its own.
  If your vault is in git, add `index.nosync/` to `.gitignore`.

**Lifecycle and security**

- The plugin starts the server when Obsidian opens and stops it when Obsidian
  closes. If Obsidian crashes, the server and helpers notice and exit on their own.
- The server only listens on `127.0.0.1` (your own Mac), on a random port, and
  every request needs a secret token that is generated at each start.
- Paths sent to the server are checked, so it can't read outside the vault.

**Code map**

| Path | What |
|---|---|
| `plugin/main.ts` | Obsidian plugin: UI, settings, starting and stopping the server |
| `server/main.go` | HTTP API, settings, startup |
| `server/index.go` | SQLite schema, vault scan, change tracking, job queue |
| `server/query.go` | Query parsing, filters, ranking, typo correction |
| `server/extract.go` | Reading each file type, OCR and PDF work |
| `server/ask.go` | Ask your vault |
| `helper/main.swift` | Vision OCR, PDFKit, speech-to-text, on-device LLM |

---

## Updating, uninstalling and troubleshooting

**Update**: pull the latest code, run `./install.sh "/path/to/your/vault"`
again, then switch Supersearch off and on in Community plugins.

**Uninstall**: Settings → Community plugins → Supersearch → uninstall. That
deletes the plugin folder, including the index. Nothing else is left on your Mac.

**Troubleshooting**

| Problem | Try |
|---|---|
| "server binary not found" | Run `./install.sh` with the right vault path |
| "supersearch-helper is missing" | Re-run `./install.sh`; check `swiftc --version` works |
| No results right after installing | Wait for the status bar to clear, then search again |
| Results look wrong or stale | Run **Delete search index**; it rebuilds in a minute or two |
| "Ask" says Apple Intelligence isn't available | Turn it on in System Settings → Apple Intelligence & Siri |
| Fan spins during the first index | Normal for a minute or two while images are read. Use **Pause** or run on battery (1 worker). |
| Is it still running after quitting? | `pgrep -lf supersearch` prints nothing once Obsidian has quit |

Server errors appear in Obsidian's developer console
(<kbd>Cmd</kbd>+<kbd>Option</kbd>+<kbd>I</kbd>), prefixed with `supersearch-server:`.

---

## Search from your phone (optional)

The plugin also loads on Obsidian mobile, but a phone can't run the server.
Instead, run the server on a Mac that stays on and has the same vault (for
example through iCloud), and connect to it over [Tailscale](https://tailscale.com)
so the connection is private and encrypted:

```bash
SUPERSEARCH_TOKEN="choose-a-long-random-secret" ./server/supersearch-server -vault "/path/to/vault" -listen 127.0.0.1:8765
```

```bash
tailscale serve --bg 8765
```

Then on your phone, in **Settings → Supersearch**, set **Server URL** to
`https://<your-mac>.<your-tailnet>.ts.net` and **Server token** to your secret.
The server rescans every minute, so changes synced from other devices show up.

> Don't expose the port to the internet: the token is the only protection.

---

## Development

```bash
make test                      # Go tests, including real OCR, PDF, audio and Ask
```

```bash
./server.sh -q "some words"    # one-off search from the terminal
```

```bash
./server.sh                    # run the server by hand (the plugin normally does this)
```

Don't leave `./server.sh` running while Obsidian is open: two servers on one
vault do the same work twice.

Design notes, measurements and known limits are in [PLAN.md](PLAN.md).
