# supersearch

Fast search for an Obsidian vault across notes, PDFs, images (OCR), office docs,
ebooks, code and audio (speech-to-text) without slowing Obsidian down.

## Goals (the only ones)

1. **Fast**: results in < 50 ms keystroke-to-render; zero measurable impact on
   Obsidian startup, typing, or memory.
2. **Everything is searchable**: every format below, including text inside images.
3. **Seamless**: enable the plugin and it just works. The server auto-starts,
   indexes in the background, and nothing needs a manual reindex.

## Why not Omnisearch

Omnisearch keeps its index and runs OCR **inside Obsidian's renderer process**
(JS heap, IndexedDB, Tesseract WASM). Big vault → big heap, slow startup, jank.
Here Obsidian holds **zero** index state: a Go sidecar owns extraction, OCR and
an on-disk SQLite index; the plugin is a search box making HTTP calls.

## Architecture

```
Obsidian plugin (TS)                     supersearch-server (Go, one binary)
 search modal / sidebar / ask ──HTTP──▶  GET /search  POST /ask  POST /changed
 vault events → /changed      127.0.0.1  POST /pause  GET /status
 settings, status bar         + token    scan at start + every 60 s (mtime/size diff)
 spawns + kills the server               workers → SQLite FTS5
                                              │
                                              ▼
                               supersearch-helper (Swift, macOS frameworks)
                               Vision OCR · PDFKit · SpeechAnalyzer · FoundationModels
```

- **No upload**: the server reads the vault folder directly.
- **Index**: `<vault>/.obsidian/plugins/supersearch/index.nosync/index.db`.
  It's a disposable cache (delete it to rebuild) and iCloud skips `.nosync`.
- **Lanes**: one fast worker (notes, PDF text, docs) and 3 OCR/audio workers
  on mains power, 1 on battery (checked each minute via `pmset`), so a big OCR
  backlog never delays an edited note. Workers claim jobs from one queue.
- **Security**: loopback only, random port, random bearer token, no CORS.
  Paths from the plugin are validated (no `..`, no dot-folders).

## What gets indexed

| Kind | Files | How |
|---|---|---|
| Notes | .md | split at headings: each section is a result that opens at its line; heading + #tags + frontmatter tags rank above body text |
| Text / code | .txt .canvas .csv .json .yaml .go .py .ts … | read as-is |
| PDF | .pdf | PDFKit text per page, no OCR (a scanned page with no text layer contributes no content) |
| Images | .png .jpg .webp .heic .gif .tiff … | Apple Vision OCR (lines rebuilt in reading order, Cyrillic look-alikes folded, transparency flattened) |
| Audio | .m4a .mp3 .wav .aac .flac .mp4 .mov … | Apple on-device speech-to-text |
| Office | .docx .pptx .xlsx · .doc .rtf .odt | zip+XML in Go · macOS `textutil` |
| Web / ebooks | .html .svg · .epub | tags stripped (script/style dropped) |

Image and audio hits are shown as **the note that embeds them** and open that
note scrolled to the embed. Their file names are not indexed (they're noise).

## Using it

Commands: **Search everything**, **Search in current note (including its
images)**, **Open search panel** (sidebar), **Ask your vault**, **Pause / resume
background indexing**.

Query syntax:

| Query | Meaning |
|---|---|
| `sched` | prefix match (finds "scheduling") |
| `DynamicArrays` | spaceless words split into known words, each typo-tolerant |
| `"round robin"` | exact phrase |
| `-passing` | exclude word |
| `path:"Operating System"` / `-path:Archive` | path contains / doesn't |
| `ext:pdf` | extension |
| `in:note` `in:image` `in:pdf` `in:doc` `in:audio` `in:ocr` | kind |

Ranking: notes first, then everything else; within each, BM25 with a small
boost for recently edited files; at most 5 rows per file, grouped together.
A query with no hits is retried with typo correction ("schedulng" →
"scheduling", shown as *Showing results for…*).

Settings: ignored folders, OCR languages (e.g. `en-US,hi-IN`; changing re-reads
images and recordings), fall back to local server (desktop only: when the
remote is unreachable, the local index serves and the remote is re-probed
until it answers),
remote server URL + token. The server reads them from the plugin's `data.json`,
not from flags, so every server on the vault (plugin, `./server.sh`, remote)
uses the same settings. Two servers with different settings used to make each
other re-OCR every image on every start.

**Delete search index** (settings button or command) wipes everything stored
(`POST /reset`: rows deleted, file vacuumed) and rebuilds from the vault in the
background. To remove the data for good, disable or uninstall the plugin: the
index lives in the plugin's folder.

## Ask your vault

Needs no install: the best keyword hits go to Apple's on-device language
model, which answers with [n] citations to clickable sources.

Search by meaning (embeddings) was built and removed. Apple's built-in
embedding models scored unrelated queries as high as related ones on this
vault, and Ollama worked but made the Mac run hot during indexing.

## Remote server (mobile)

The plugin loads on mobile but can't run the server there. Run the server on an
always-on Mac that has the vault (e.g. the same iCloud vault):

```
SUPERSEARCH_TOKEN=<long random> ./server/supersearch-server \
  -vault "/path/to/vault" -listen 127.0.0.1:8765
tailscale serve --bg 8765            # HTTPS on your tailnet
```

Then in the plugin settings on the phone: Server URL
`https://<mac>.<tailnet>.ts.net`, Server token as above. The 60 s rescan picks
up changes synced from other devices. Don't expose the port publicly: the
token is the only auth, and plain HTTP would leak it.

## Build & test

```
./install.sh [vault]   # builds helper (swiftc), server, plugin; copies into the vault
./server.sh -q "words" # one-off search from the terminal
make test              # Go tests incl. real Vision OCR, PDF, audio, on-device LLM
```

## Status

Measured on the real vault (469 notes, 1,763 images, one 265-page PDF):
full text index (notes + the PDF's text layer) 0.8 s; rescan 11 ms; idle server
30 MB and 0.08% CPU, with no helpers loaded (they stop after 30 s idle; three
idle OCR helpers used to hold 280 MB). Queries (median, `server/bench_test.go`):
specific ones 0.1–0.7 ms, "process" 2.3 ms, "the" 5.4 ms, a single letter 7 ms.
"the" was 32 ms and a single letter 54 ms before three changes: snippets only
for the rows shown, a 1-letter prefix index, and the C SQLite library
(mattn/go-sqlite3, 2.3× faster than the pure-Go port on every query; SQLite
cache and mmap pragmas and an FTS5 `optimize` were measured and made no
difference). Vision OCR on an M2
(the model stays loaded in one helper per worker while there is work; one process per
image was 230 ms each): 1 worker 9 images/s, 2 → 17, 3 → 24, 4 → 27. With the
default of 3, all 1,763 images take 77 s. Ask takes about 2–4 s.

Verified by tests or on the real data: everything except behaviour inside the
Obsidian UI (modal, sidebar, thumbnails, jump-to-line, settings restart) and
on a phone, which are type-checked but need a manual look.

## Ceilings (deliberate shortcuts)

| Shortcut | Ceiling | Upgrade path |
|---|---|---|
| macOS-only helper | No Windows/Linux | tesseract + poppler fallback behind the same helper commands |
| PDFs are text layer only | Scanned PDFs with no text layer are not searchable | render pages + Vision OCR behind the same pdftext path |
| No .webm/.ogg audio | Apple's decoder can't read them | ffmpeg conversion step |
| Typo correction scans vocab words by first letter | First-letter typos not corrected | trigram index over vocab |
| Remote mode has token auth only | Needs Tailscale/HTTPS in front | built-in TLS |
