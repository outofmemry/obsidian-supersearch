package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var kinds = map[string]string{
	".pdf": "pdf",
	".png": "image", ".jpg": "image", ".jpeg": "image", ".webp": "image", ".gif": "image",
	".bmp": "image", ".tif": "image", ".tiff": "image", ".heic": "image", ".heif": "image",
	".docx": "ooxml", ".pptx": "ooxml", ".xlsx": "ooxml",
	".doc": "textutil", ".rtf": "textutil", ".odt": "textutil",
	".html": "html", ".htm": "html", ".xhtml": "html", ".svg": "html",
	".epub": "epub",
	// ponytail: no .webm/.ogg (Obsidian's recorder on some platforms): Apple's
	// decoder can't read them; add an ffmpeg conversion step if they matter.
	".m4a": "audio", ".mp3": "audio", ".wav": "audio", ".aac": "audio", ".aiff": "audio",
	".aif": "audio", ".caf": "audio", ".flac": "audio", ".mp4": "audio", ".mov": "audio",
}

// slow reports kinds that skip the fast lane and go straight to the OCR workers.
func slow(kind string) bool { return kind == "image" || kind == "audio" }

func init() {
	for _, e := range strings.Fields(`.md .markdown .txt .canvas .csv .org .tex .log
		.js .mjs .ts .jsx .tsx .go .py .rb .php .java .kt .swift .c .h .cc .cpp .hpp .rs
		.sh .zsh .bash .sql .css .scss .json .yaml .yml .toml .ini .xml .lua .r .dart .ex`) {
		kinds[e] = "text"
	}
}

func kindOf(p string) string { return kinds[strings.ToLower(filepath.Ext(p))] }

const (
	maxText = 16 << 20 // per file / per zip entry; also bounds zip bombs

	maxRemoteImages = 20              // remote images OCR'd per note
	maxRemoteBytes  = 10 << 20        // per remote image; keeps the vault light, text only
	maxRemoteFetch  = 8               // concurrent downloads per note; one slow image never stalls the rest
	remoteTimeout   = 30 * time.Second
)

// Set by main from the plugin's settings.
var (
	helperPath = "supersearch-helper" // Apple Vision + PDFKit, see helper/main.swift
	ocrLang    = ""                   // comma-separated BCP-47, "" = en-US
)

// mediaConfig covers images and audio (the only kinds that use OCR/speech).
func mediaConfig() string { return "vision1|" + normLang(ocrLang) }

func normLang(l string) string {
	if l = strings.ReplaceAll(l, " ", ""); l == "" {
		return "en-US"
	}
	return l
}

// processOne runs one job in the given status. false = nothing to do.
// h is this worker's helper process for image OCR (nil in the fast lane).
// PDFs always take the text-layer path, even if an old index left them in
// ocr status: Vision OCR never runs on rendered PDF pages, so a short title
// page keeps its exact text instead of being replaced by an OCR misread
// ("Dynamic Arrays" -> "Oganic Armys"). Notes with remote images
// (e.g. Google Drive embeds) take the slow lane so the fetch + OCR never
// delays a note you just edited; only the OCR text is stored, never the
// image bytes, so the vault stays light.
func (ix *Index) processOne(status string, h *helperProc) bool {
	j, ok := ix.next(status)
	if !ok {
		return false
	}
	defer ix.release(j.id)
	abs := filepath.Join(ix.vault, filepath.FromSlash(j.rel))
	var pages []page
	var err error
	next := "done"
	if status == "ocr" {
		switch {
		case j.kind == "pdf":
			pages, err = extract(abs, j.kind)
		case j.kind == "text" && isMarkdown(abs):
			pages, err = ix.noteRemoteOCR(abs, h)
		default:
			pages, err = ocr(abs, j.kind, h)
		}
	} else {
		pages, err = extract(abs, j.kind)
		if err == nil && j.kind == "text" && isMarkdown(abs) && hasRemoteImages(pages) {
			next = "ocr" // re-read in the slow lane with the remote OCR text
		}
	}
	msg := ""
	if err != nil {
		next, msg, pages = "error", err.Error(), nil
		pages = []page{{source: "text"}} // keep the file findable by name even though its content failed
	}
	if err := ix.store(j, pages, next, msg); err != nil {
		fmt.Fprintln(os.Stderr, "store:", j.rel, err)
		time.Sleep(time.Second) // don't spin on a broken db
	} else if next == "ocr" {
		// A fast-lane job chained into the slow lane: sleeping OCR workers
		// never see it otherwise (store, unlike apply, wakes nobody).
		ix.wake()
	}
	return true
}

func isMarkdown(abs string) bool {
	switch ext := strings.ToLower(filepath.Ext(abs)); ext {
	case ".md", ".markdown":
		return true
	}
	return false
}

// worker n processes jobs in status forever. Extra OCR workers (n > 0) only
// run on mains power: on battery OCR drops to one at a time.
func (ix *Index) worker(status string, n int, wake chan struct{}) {
	h := &helperProc{nice: true}
	for {
		if status == "ocr" && n > 0 && onBattery() {
			select { // no event fires when the charger is plugged in: re-check
			case <-wake:
			case <-time.After(time.Minute):
			}
			continue
		}
		if (status == "ocr" && ix.paused.Load()) || !ix.processOne(status, h) {
			<-wake
		}
	}
}

var battery struct {
	sync.Mutex
	on      bool
	checked time.Time
}

// onBattery reports whether the Mac runs on battery, checked at most once a minute.
func onBattery() bool {
	battery.Lock()
	defer battery.Unlock()
	if time.Since(battery.checked) > time.Minute {
		out, _ := exec.Command("pmset", "-g", "batt").Output()
		battery.on, battery.checked = bytes.Contains(out, []byte("Battery Power")), time.Now()
	}
	return battery.on
}

// extract is the cheap first pass: everything except image OCR and audio
// transcription. PDFs are text layer only, page by page.
func extract(abs, kind string) (pages []page, err error) {
	one := func(body string, err error) ([]page, error) {
		return []page{{source: "text", body: body}}, err
	}
	switch kind {
	case "text":
		b, err := readCapped(abs)
		if err != nil {
			return nil, err
		}
		if ext := strings.ToLower(filepath.Ext(abs)); ext == ".md" || ext == ".markdown" {
			return markdownSections(string(b)), nil
		}
		return one(string(b), nil)
	case "html":
		b, err := readCapped(abs)
		if err != nil {
			return nil, err
		}
		var sb strings.Builder
		xmlText(bytes.NewReader(b), &sb)
		return one(sb.String(), nil)
	case "ooxml":
		return one(zipText(abs, func(n string) bool {
			return n == "word/document.xml" || n == "xl/sharedStrings.xml" ||
				strings.HasPrefix(n, "ppt/slides/slide") || strings.HasPrefix(n, "ppt/notesSlides/")
		}))
	case "epub":
		return one(zipText(abs, func(n string) bool {
			n = strings.ToLower(n)
			return strings.HasSuffix(n, ".xhtml") || strings.HasSuffix(n, ".html") || strings.HasSuffix(n, ".htm")
		}))
	case "textutil":
		return one(run(time.Minute, "textutil", "-convert", "txt", "-stdout", abs))
	case "pdf":
		return pdfText(abs)
	}
	return nil, fmt.Errorf("no extractor for kind %q", kind)
}

// pdfText reads the PDF text layer page by page. No OCR is attempted: a
// rendered page fed to Vision replaces exact text with misreads, and a
// scanned page with no text layer simply contributes no content.
func pdfText(abs string) ([]page, error) {
	texts, err := pdfPages(abs)
	if err != nil {
		return nil, err
	}
	var pages []page
	for i, t := range texts {
		if t != "" || i == 0 { // page 1 always exists: it carries the file name
			pages = append(pages, page{n: i + 1, source: "text", body: t})
		}
	}
	return pages, nil
}

func readCapped(abs string) ([]byte, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxText))
}

var (
	headingRe = regexp.MustCompile(`^#{1,6}\s+(.*?)[\s#]*$`)
	tagRe     = regexp.MustCompile(`(?:^|\s)#([\p{L}\p{N}_/-]*\p{L}[\p{L}\p{N}_/-]*)`)
	fmTagsRe  = regexp.MustCompile(`(?m)^tags?:[ \t]*(.*)$((?:\n[ \t]*-[ \t]*.*)*)`)
)

// markdownSections splits a note at its headings so each section is its own
// search result that opens at its own line. The heading and the section's
// #tags go in the title column, which outranks body text; frontmatter tags go
// on the first section.
func markdownSections(text string) []page {
	lines := strings.Split(text, "\n")
	var pages []page
	start, heading, fence := 0, "", false
	flush := func(end int) {
		body := strings.Join(lines[start:end], "\n")
		if start == 0 || strings.TrimSpace(body) != "" {
			pages = append(pages, page{n: start + 1, source: "text", title: heading + tagsOf(body), body: body})
		}
	}
	for i, l := range lines {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = !fence
			continue
		}
		if m := headingRe.FindStringSubmatch(l); m != nil && !fence {
			if i > start {
				flush(i)
			}
			start, heading = i, m[1]
		}
	}
	flush(len(lines))
	if strings.HasPrefix(text, "---\n") {
		if end := strings.Index(text[4:], "\n---"); end >= 0 {
			if m := fmTagsRe.FindStringSubmatch(text[4 : 4+end]); m != nil {
				pages[0].title += " " + strings.NewReplacer("[", " ", "]", " ", ",", " ", "-", " ", "#", "").Replace(m[1]+m[2])
			}
		}
	}
	return pages
}

func tagsOf(body string) string {
	var sb strings.Builder
	for _, m := range tagRe.FindAllStringSubmatch(body, -1) {
		sb.WriteString(" " + m[1])
	}
	return sb.String()
}

// zipText pulls the visible text out of zip-of-XML formats (docx/pptx/xlsx/epub).
func zipText(abs string, want func(string) bool) (string, error) {
	zr, err := zip.OpenReader(abs)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	var sb strings.Builder
	for _, f := range zr.File {
		if !want(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		xmlText(rc, &sb)
		rc.Close()
	}
	return sb.String(), nil
}

// xmlText writes the character data of an XML or HTML document, with a
// newline after block elements. Lenient: malformed input keeps what parsed.
func xmlText(r io.Reader, sb *strings.Builder) {
	dec := xml.NewDecoder(io.LimitReader(r, maxText))
	dec.Strict, dec.AutoClose, dec.Entity = false, xml.HTMLAutoClose, xml.HTMLEntity
	skip := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "script" || t.Name.Local == "style" {
				skip++
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "script", "style":
				skip--
			case "p", "si", "br", "tab", "div", "li", "tr", "td", "th", "h1", "h2", "h3", "h4", "h5", "h6", "text", "title":
				sb.WriteByte('\n')
			}
		case xml.CharData:
			if skip <= 0 {
				sb.Write(t)
			}
		}
	}
}

// ocr is the slow second pass for images and audio.
func ocr(abs, kind string, h *helperProc) ([]page, error) {
	if kind == "audio" {
		body, err := run(30*time.Minute, "nice", "-n", "15", helperPath, "transcribe", abs, ocrLang)
		return []page{{source: "speech", body: body}}, err
	}
	if kind == "image" {
		body, err := ocrFile(abs, h)
		return []page{{source: "ocr", body: body}}, err
	}
	return nil, fmt.Errorf("no OCR for kind %q", kind)
}

// ocrFile runs Vision OCR on one local image file. A nil helper falls back
// to a one-shot helper process.
func ocrFile(abs string, h *helperProc) (string, error) {
	if h != nil {
		// Through the worker's long-running helper: Vision's model stays
		// loaded, which halves the time and CPU per image vs a process each.
		var resp struct{ Answer, Error string }
		err := h.call(map[string]any{"op": "ocr", "path": abs, "langs": ocrLang}, &resp, 3*time.Minute)
		if err == nil && resp.Error != "" {
			err = errors.New(resp.Error)
		}
		return resp.Answer, err
	}
	return run(3*time.Minute, helperPath, "ocr", abs, ocrLang)
}

var (
	mdImgRe   = regexp.MustCompile(`!\[[^\]]*\]\((https?://[^)\s]+)\)`)
	htmlImgRe = regexp.MustCompile(`(?i)<img[^>]+src=["'](https?://[^"']+)["']`)
)

// remoteImageURLs collects the http(s) image URLs embedded in a note's text,
// deduplicated in first-seen order.
func remoteImageURLs(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range append(mdImgRe.FindAllStringSubmatch(text, -1), htmlImgRe.FindAllStringSubmatch(text, -1)...) {
		if u := m[1]; !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// hasRemoteImages reports whether extracted note pages reference any remote
// image, so the note can take the slow lane for fetch + OCR.
func hasRemoteImages(pages []page) bool {
	for _, p := range pages {
		if len(remoteImageURLs(p.title+"\n"+p.body)) > 0 {
			return true
		}
	}
	return false
}

var remoteClient = &http.Client{Timeout: remoteTimeout}

// fetchImage downloads a remote image to a temp file (never into the vault).
// Links that don't serve image bytes (viewer pages, login walls) are skipped
// by the caller: use a direct / thumbnail link instead.
func fetchImage(url string) (string, error) {
	resp, err := remoteClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/") {
		return "", fmt.Errorf("GET %s: not an image (%s)", url, ct)
	}
	f, err := os.CreateTemp("", "ss-remote-*")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxRemoteBytes+1))
	f.Close()
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if n > maxRemoteBytes {
		os.Remove(f.Name())
		return "", fmt.Errorf("GET %s: over the %d MB cap", url, maxRemoteBytes>>20)
	}
	return f.Name(), nil
}

// noteRemoteOCR re-extracts a note's sections and appends one OCR chunk with
// the text read out of its remote images. Failed fetches are skipped: a dead
// Drive link never fails the note itself. OCR text is cached by URL, so an
// edited note only pays for images it hasn't seen before.
func (ix *Index) noteRemoteOCR(abs string, h *helperProc) ([]page, error) {
	b, err := readCapped(abs)
	if err != nil {
		return nil, err
	}
	pages := markdownSections(string(b))
	urls := remoteImageURLs(string(b))
	if len(urls) > maxRemoteImages {
		urls = urls[:maxRemoteImages]
	}
	textOf := ix.cachedRemote(urls)
	var missing []string
	for _, u := range urls {
		if _, ok := textOf[u]; !ok {
			missing = append(missing, u)
		}
	}
	for u, body := range ix.fetchOCR(missing, h) {
		textOf[u] = body
	}
	// One ocr chunk per image (page slot 0x40000+i) so the plugin can jump
	// to the exact embed whose picture the match's text came from. url:i
	// lives in the title column, which never appears in snippets.
	for i, u := range urls {
		if t := strings.TrimSpace(textOf[u]); t != "" {
			pages = append(pages, page{n: 0x40000 | i, source: "ocr", body: t, title: fmt.Sprintf("url:%d", i)})
		}
	}
	return pages, nil
}

// cachedRemote returns the OCR text already stored for each URL.
func (ix *Index) cachedRemote(urls []string) map[string]string {
	out := map[string]string{}
	if len(urls) == 0 {
		return out
	}
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	for _, u := range urls {
		var body string
		if ix.db.QueryRow(`SELECT body FROM remote_ocr WHERE url = ?`, u).Scan(&body) == nil {
			out[u] = body
		}
	}
	return out
}

// fetchOCR downloads and OCRs every URL, concurrently. Downloads run
// maxRemoteFetch-at-a-time so one slow link never stalls the rest; OCR fans
// out over a small pool of helpers (the worker's own plus spares) because a
// single helper answers one request at a time. Failures resolve to "" and
// are skipped by the caller.
func (ix *Index) fetchOCR(urls []string, h *helperProc) map[string]string {
	out := make(map[string]string, len(urls))
	if len(urls) == 0 {
		return out
	}
	type result struct {
		url, tmp string
	}
	sem := make(chan struct{}, maxRemoteFetch)
	res := make(chan result, len(urls))
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			tmp, err := fetchImage(u)
			if err != nil {
				res <- result{url: u}
				return
			}
			res <- result{url: u, tmp: tmp}
		}(u)
	}
	wg.Wait()
	close(res)
	files := map[string]string{} // url -> temp file
	for r := range res {
		if r.tmp != "" {
			files[r.url] = r.tmp
		}
	}
	// OCR pool: the worker's helper (model already loaded) plus spares.
	// Helpers stop themselves after 30 s idle, so this costs nothing
	// once the backlog drains.
	pool := []*helperProc{h}
	for range 3 {
		pool = append(pool, &helperProc{nice: true})
	}
	if h == nil {
		pool = pool[1:]
	}
	type ocrout struct {
		url, body string
	}
	oc := make(chan ocrout, len(files))
	var owg sync.WaitGroup
	i := 0
	for u, tmp := range files {
		owg.Add(1)
		go func(u, tmp string, hh *helperProc) {
			defer owg.Done()
			text, err := ocrFile(tmp, hh)
			os.Remove(tmp)
			if err != nil {
				oc <- ocrout{url: u}
				return
			}
			oc <- ocrout{url: u, body: strings.TrimSpace(text)}
		}(u, tmp, pool[i%len(pool)])
		i++
	}
	owg.Wait()
	close(oc)
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	for r := range oc {
		out[r.url] = r.body
		if r.body != "" {
			ix.db.Exec(`INSERT OR REPLACE INTO remote_ocr(url, body) VALUES(?, ?)`, r.url, r.body)
		}
	}
	return out
}

var pdfCache struct { // the last pdf's text, so back-to-back reads reuse it
	sync.Mutex
	key   string
	texts []string
}

func pdfPages(abs string) ([]string, error) {
	key := abs
	if info, err := os.Stat(abs); err == nil {
		key += fmt.Sprint("|", info.ModTime().UnixNano(), "|", info.Size())
	}
	pdfCache.Lock()
	defer pdfCache.Unlock()
	if pdfCache.key == key {
		return pdfCache.texts, nil
	}
	out, err := run(time.Minute, helperPath, "pdftext", abs)
	if err != nil {
		return nil, err
	}
	pdfCache.key, pdfCache.texts = key, strings.Split(strings.TrimSuffix(out, "\f"), "\f") // every page ends with \f
	return pdfCache.texts, nil
}

func run(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", fmt.Errorf("%s: %w %s", filepath.Base(name), err, msg)
	}
	return string(out), nil
}

func missingTools() []string {
	if _, err := os.Stat(helperPath); err != nil {
		return []string{"supersearch-helper"}
	}
	return []string{}
}
