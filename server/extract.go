package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
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
	maxText      = 16 << 20 // per file / per zip entry; also bounds zip bombs
	minPageChars = 20       // a pdf page with less text than this is treated as scanned
)

// Set by main from the plugin's settings. Part of the configs below:
// changing one redoes only the files it affects.
var (
	helperPath = "supersearch-helper" // Apple Vision + PDFKit, see helper/main.swift
	ocrLang    = ""                   // comma-separated BCP-47, "" = en-US
	ocrFigures = false                // also OCR pdf pages that have a text layer
)

// mediaConfig covers images and audio; pdfConfig covers pdfs.
func mediaConfig() string { return "vision1|" + normLang(ocrLang) }
func pdfConfig() string   { return fmt.Sprintf("vision1|%s|%t", normLang(ocrLang), ocrFigures) }

func normLang(l string) string {
	if l = strings.ReplaceAll(l, " ", ""); l == "" {
		return "en-US"
	}
	return l
}

// processOne runs one job in the given status. false = nothing to do.
// h is this worker's helper process for image OCR (nil in the fast lane).
func (ix *Index) processOne(status string, h *helperProc) bool {
	j, ok := ix.next(status)
	if !ok {
		return false
	}
	defer ix.release(j.id)
	abs := filepath.Join(ix.vault, filepath.FromSlash(j.rel))
	var pages []page
	var err error
	next, replaceAll := "done", true
	if status == "ocr" && j.kind == "pdf" {
		var more bool
		pages, more, err = ix.pdfOCR(j, abs)
		replaceAll = false // keep the text pages and the batches already done
		if more {
			next = "ocr"
		}
	} else if status == "ocr" {
		pages, err = ocr(abs, j.kind, h)
	} else {
		var needOCR bool
		pages, needOCR, err = extract(abs, j.kind)
		if needOCR {
			next = "ocr"
		}
	}
	msg := ""
	if err != nil {
		next, msg, pages = "error", err.Error(), nil
		if replaceAll { // keep the file findable by name even though its content failed
			pages = []page{{source: "text"}}
		}
	}
	if err := ix.store(j, pages, replaceAll, next, msg); err != nil {
		fmt.Fprintln(os.Stderr, "store:", j.rel, err)
		time.Sleep(time.Second) // don't spin on a broken db
	}
	return true
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

// extract is the cheap first pass: everything except OCR.
func extract(abs, kind string) (pages []page, needOCR bool, err error) {
	one := func(body string, err error) ([]page, bool, error) {
		return []page{{source: "text", body: body}}, false, err
	}
	switch kind {
	case "text":
		b, err := readCapped(abs)
		if err != nil {
			return nil, false, err
		}
		if ext := strings.ToLower(filepath.Ext(abs)); ext == ".md" || ext == ".markdown" {
			return markdownSections(string(b)), false, nil
		}
		return one(string(b), nil)
	case "html":
		b, err := readCapped(abs)
		if err != nil {
			return nil, false, err
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
		texts, err := pdfPages(abs)
		if err != nil {
			return nil, false, err
		}
		for i, t := range texts {
			if sparse(t) {
				needOCR = true
				t = ""
			}
			if t != "" || i == 0 { // page 1 always exists: it carries the file name
				pages = append(pages, page{n: i + 1, source: "text", body: t})
			}
		}
		return pages, needOCR || (ocrFigures && len(texts) > 0), nil
	}
	return nil, false, fmt.Errorf("no extractor for kind %q", kind)
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
		// Through the worker's long-running helper: Vision's model stays
		// loaded, which halves the time and CPU per image vs a process each.
		var resp struct{ Answer, Error string }
		err := h.call(map[string]any{"op": "ocr", "path": abs, "langs": ocrLang}, &resp, 3*time.Minute)
		if err == nil && resp.Error != "" {
			err = errors.New(resp.Error)
		}
		return []page{{source: "ocr", body: resp.Answer}}, err
	}
	return nil, fmt.Errorf("no OCR for kind %q", kind)
}

var pdfBatch = 8 // pages per OCR job; a var so tests can shrink it

// pdfOCR OCRs the next pdfBatch pages that need it (no text layer, or every
// page with ocrFigures). A page stored with source "ocr" is done, so progress
// survives restarts: a 265-page book used to be one job that was killed and
// started over from page 1 every time Obsidian closed, and never finished.
func (ix *Index) pdfOCR(j job, abs string) (pages []page, more bool, err error) {
	texts, err := pdfPages(abs)
	if err != nil {
		return nil, false, err
	}
	done := map[int]bool{}
	rows, err := ix.db.Query(`SELECT rowid & ?, source FROM chunks WHERE rowid BETWEEN ? AND ?`,
		1<<pageBits-1, j.id<<pageBits, (j.id+1)<<pageBits-1)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var n int
		var src string
		rows.Scan(&n, &src)
		done[n] = src == "ocr"
	}
	rows.Close()
	var todo []int
	for i, t := range texts {
		if (sparse(t) || ocrFigures) && !done[i+1] {
			todo = append(todo, i+1)
		}
	}
	more = len(todo) > pdfBatch
	todo = todo[:min(len(todo), pdfBatch)]
	if len(todo) == 0 {
		return nil, false, nil
	}
	nums := make([]string, len(todo))
	for k, n := range todo {
		nums[k] = strconv.Itoa(n)
	}
	out, err := run(time.Minute+time.Duration(len(todo))*10*time.Second,
		"nice", "-n", "15", helperPath, "pdfocr", abs, strings.Join(nums, ","), ocrLang)
	if err != nil {
		return nil, false, err
	}
	bodies := strings.Split(strings.TrimSuffix(out, "\f"), "\f")
	for k, n := range todo {
		body := ""
		if k < len(bodies) {
			body = bodies[k]
		}
		if t := texts[n-1]; !sparse(t) { // figure OCR: keep the text layer, add what the images say
			body = t + "\n" + body
		}
		pages = append(pages, page{n: n, source: "ocr", body: body})
	}
	return pages, more, nil
}

func pdfPages(abs string) ([]string, error) {
	out, err := run(time.Minute, helperPath, "pdftext", abs)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(out, "\f"), "\f"), nil // every page ends with \f
}

func sparse(s string) bool {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			if n++; n >= minPageChars {
				return false
			}
		}
	}
	return true
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
