//go:build !darwin || portable

package main

// Linux and Windows (or a Mac built with -tags portable): the Apple
// frameworks are replaced by common open-source tools, found on PATH or in
// their usual install folders. All of them are optional; a missing one only
// costs its own file kinds.
//
//   images  tesseract                     (OCR)
//   PDFs    pdftotext                     (poppler)
//   audio   whisper-cli + ffmpeg + model  (whisper.cpp, model in ~/.config/supersearch/models)
//   .doc    antiword or catdoc            (.rtf and .odt are read in Go)
//   ask     Ollama on localhost

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"
)

const (
	appleBackend = false
	mediaEngine  = "tess1"
)

func init() {
	// Tesseract's own threads fight our parallel workers: one thread each is
	// several times faster overall (tesseract's documented advice).
	if os.Getenv("OMP_THREAD_LIMIT") == "" {
		os.Setenv("OMP_THREAD_LIMIT", "1")
	}
}

// ---- images ----

func ocrImage(abs string, _ *helperProc) (string, error) {
	tess := findTool("tesseract")
	if tess == "" {
		return "", errors.New("tesseract is not installed (it reads text in images)")
	}
	in, cleanup, err := ocrInput(abs)
	if err != nil {
		return "", err
	}
	defer cleanup()
	args := []string{in, "stdout"}
	if l := tessLangs(tess); l != "" {
		args = append(args, "-l", l)
	}
	out, err := runLow(3*time.Minute, tess, args...)
	return strings.TrimSpace(out), err
}

// ocrInput hands tesseract a file it reads well. Transparent pixels read as
// black (dark text on a transparent PNG came back empty with Vision too), so
// images with alpha are flattened onto white; WebP and GIF are converted
// because not every tesseract build reads them; HEIC needs heif-convert.
func ocrInput(abs string) (string, func(), error) {
	keep := func() {}
	ext := strings.ToLower(filepath.Ext(abs))
	if kinds[ext] != "image" {
		ext = sniffImage(abs) // a downloaded remote image has no extension
	}
	switch ext {
	case ".heic", ".heif":
		conv := findTool("heif-convert", "heif-dec")
		if conv == "" {
			return "", nil, errors.New("HEIC images need heif-convert (libheif)")
		}
		tmp, err := tempName(".png")
		if err != nil {
			return "", nil, err
		}
		if _, err := runLow(time.Minute, conv, abs, tmp); err != nil {
			os.Remove(tmp)
			return "", nil, err
		}
		return tmp, func() { os.Remove(tmp) }, nil
	case ".png":
		if !pngMayHaveAlpha(abs) {
			return abs, keep, nil // the common case: no decode at all
		}
	case ".webp", ".gif":
	default:
		return abs, keep, nil
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", nil, err
	}
	img, _, err := image.Decode(bufio.NewReader(f))
	f.Close()
	if err != nil {
		return "", nil, fmt.Errorf("cannot decode image: %w", err)
	}
	if o, ok := img.(interface{ Opaque() bool }); ok && o.Opaque() && ext == ".png" {
		return abs, keep, nil
	}
	flat := image.NewRGBA(img.Bounds())
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, img.Bounds().Min, draw.Over)
	tmp, err := tempName(".png")
	if err != nil {
		return "", nil, err
	}
	out, err := os.Create(tmp)
	if err != nil {
		return "", nil, err
	}
	enc := png.Encoder{CompressionLevel: png.BestSpeed} // a throwaway file: speed over size
	err = enc.Encode(out, flat)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", nil, err
	}
	return tmp, func() { os.Remove(tmp) }, nil
}

// sniffImage names an image format from its first bytes, as an extension.
func sniffImage(abs string) string {
	f, err := os.Open(abs)
	if err != nil {
		return ""
	}
	defer f.Close()
	var head [512]byte
	n, _ := io.ReadFull(f, head[:])
	switch {
	case n >= 12 && string(head[4:8]) == "ftyp" && (string(head[8:12]) == "heic" || string(head[8:12]) == "heix" || string(head[8:12]) == "mif1"):
		return ".heic"
	}
	switch http.DetectContentType(head[:n]) {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ""
}

// pngMayHaveAlpha reads the PNG header: gray+alpha, RGBA, or a palette
// (which can carry transparency). Plain RGB and gray need no decode.
func pngMayHaveAlpha(abs string) bool {
	f, err := os.Open(abs)
	if err != nil {
		return true
	}
	defer f.Close()
	var hdr [26]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return true
	}
	switch hdr[25] { // IHDR color type
	case 0, 2:
		return false
	}
	return true
}

func tempName(ext string) (string, error) {
	f, err := os.CreateTemp("", "ss-ocr-*"+ext)
	if err != nil {
		return "", err
	}
	f.Close()
	return f.Name(), nil
}

// BCP-47 (the setting's format, shared with macOS) → tesseract language data.
var tessCodes = map[string]string{
	"en": "eng", "hi": "hin", "fr": "fra", "de": "deu", "es": "spa", "it": "ita", "pt": "por",
	"nl": "nld", "ru": "rus", "uk": "ukr", "ja": "jpn", "ko": "kor", "zh-hans": "chi_sim",
	"zh-cn": "chi_sim", "zh-sg": "chi_sim", "zh": "chi_sim", "zh-hant": "chi_tra", "zh-tw": "chi_tra",
	"zh-hk": "chi_tra", "ar": "ara", "tr": "tur", "pl": "pol", "sv": "swe", "da": "dan", "fi": "fin",
	"no": "nor", "nb": "nor", "cs": "ces", "el": "ell", "he": "heb", "th": "tha", "vi": "vie",
	"id": "ind", "ms": "msa", "bn": "ben", "ta": "tam", "te": "tel", "mr": "mar", "gu": "guj",
	"kn": "kan", "ml": "mal", "pa": "pan", "ur": "urd", "fa": "fas", "ro": "ron", "hu": "hun",
	"bg": "bul", "hr": "hrv", "sk": "slk", "sl": "slv", "sr": "srp", "ca": "cat", "lt": "lit",
	"lv": "lav", "et": "est",
}

var tessLangCache struct {
	sync.Mutex
	key, langs string
}

// tessLangs maps the OCR language setting to installed tesseract languages.
// Missing language data is dropped (with one log line) instead of failing
// every image; "auto" has no tesseract equivalent and means English.
func tessLangs(tess string) string {
	tessLangCache.Lock()
	defer tessLangCache.Unlock()
	key := tess + "|" + ocrLang
	if tessLangCache.key == key {
		return tessLangCache.langs
	}
	installed := map[string]bool{}
	out, _ := run(30*time.Second, tess, "--list-langs")
	for _, l := range strings.Fields(out) {
		installed[l] = true
	}
	var want, have []string
	for _, l := range strings.Split(normLang(ocrLang), ",") {
		l = strings.ToLower(l)
		code := tessCodes[l]
		if code == "" {
			code = tessCodes[strings.SplitN(l, "-", 2)[0]]
		}
		if code == "" && l != "auto" {
			code = l // already a tesseract name, like "eng" or "chi_sim"
		}
		if code == "" {
			code = "eng"
		}
		want = append(want, code)
		if installed[code] && !contains(have, code) {
			have = append(have, code)
		}
	}
	if len(have) < len(want) && len(installed) > 0 {
		log.Printf("tesseract has no language data for some of %v (installed: %d languages); using %v", want, len(installed), have)
	}
	if len(have) == 0 && installed["eng"] {
		have = []string{"eng"}
	}
	tessLangCache.key, tessLangCache.langs = key, strings.Join(have, "+")
	return tessLangCache.langs
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- PDFs ----

// pdfRaw returns the PDF's text layer, every page ended by \f: pdftotext's
// own output format, the same as the macOS helper's.
func pdfRaw(abs string) (string, error) {
	tool := findTool("pdftotext")
	if tool == "" {
		return "", errors.New("pdftotext is not installed (poppler; it reads PDFs)")
	}
	return run(time.Minute, tool, "-q", "-enc", "UTF-8", abs, "-")
}

// ---- audio ----

// Not plain "whisper": that name is usually OpenAI's Python CLI, with other flags.
func whisperTool() string { return findTool("whisper-cli", "whisper-cpp") }

// whisperModel: $SUPERSEARCH_WHISPER_MODEL, else the first ggml model in
// ~/.config/supersearch/models (the installer downloads ggml-base.bin there).
func whisperModel() string {
	if m := os.Getenv("SUPERSEARCH_WHISPER_MODEL"); m != "" {
		return m
	}
	models, _ := filepath.Glob(filepath.Join(configDir(), "models", "ggml-*.bin"))
	sort.Strings(models)
	if len(models) == 0 {
		return ""
	}
	return models[0]
}

func transcribe(abs string) (string, error) {
	whisper, ffmpeg, model := whisperTool(), findTool("ffmpeg"), whisperModel()
	switch {
	case whisper == "":
		return "", errors.New("whisper-cli is not installed (whisper.cpp; it transcribes audio)")
	case ffmpeg == "":
		return "", errors.New("ffmpeg is not installed (it decodes audio for whisper)")
	case model == "":
		return "", errors.New("no speech model: put a ggml model in " + filepath.Join(configDir(), "models"))
	}
	dir, err := os.MkdirTemp("", "ss-audio-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	wav := filepath.Join(dir, "a.wav")
	// whisper.cpp wants 16 kHz mono PCM; ffmpeg reads every container we index.
	if _, err := runLow(10*time.Minute, ffmpeg, "-nostdin", "-loglevel", "error", "-i", abs,
		"-vn", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wav); err != nil {
		return "", err
	}
	lang := strings.ToLower(strings.SplitN(strings.SplitN(normLang(ocrLang), ",", 2)[0], "-", 2)[0])
	threads := fmt.Sprint(max(1, min(4, runtime.NumCPU()/2)))
	if _, err := runLow(30*time.Minute, whisper, "-m", model, "-f", wav, "-l", lang, "-t", threads,
		"-nt", "-np", "-otxt", "-of", filepath.Join(dir, "a")); err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	return strings.Join(strings.Fields(string(b)), " "), err
}

// ---- .doc .rtf .odt ----

func legacyText(abs string) (string, error) {
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".odt":
		return zipText(abs, func(n string) bool { return n == "content.xml" })
	case ".rtf":
		b, err := readCapped(abs)
		if err != nil {
			return "", err
		}
		return rtfText(b), nil
	}
	if t := findTool("antiword"); t != "" {
		return run(time.Minute, t, "-w", "0", abs)
	}
	if t := findTool("catdoc"); t != "" {
		return run(time.Minute, t, "-w", abs)
	}
	return "", errors.New("reading .doc files needs antiword or catdoc")
}

// ---- ask ----

var ollamaClient = &http.Client{Timeout: 5 * time.Minute} // a cold model can take a while to load

// ollamaBase follows Ollama's own OLLAMA_HOST convention.
func ollamaBase() string {
	h := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if h == "" {
		return "http://127.0.0.1:11434"
	}
	if !strings.Contains(h, "://") {
		h = "http://" + h
	}
	u, err := url.Parse(h)
	if err != nil {
		return "http://127.0.0.1:11434"
	}
	host, port := u.Hostname(), u.Port()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "11434"
	}
	u.Host = net.JoinHostPort(host, port)
	return strings.TrimSuffix(u.String(), "/")
}

const askSetup = "Ask is not available: it needs Ollama running on this computer (https://ollama.com), then: ollama pull llama3.2"

// ollamaModel: $SUPERSEARCH_OLLAMA_MODEL, else llama3.2 if pulled, else the
// first model Ollama has.
func ollamaModel(base string) (string, error) {
	if m := os.Getenv("SUPERSEARCH_OLLAMA_MODEL"); m != "" {
		return m, nil
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(base + "/api/tags")
	if err != nil {
		return "", errors.New(askSetup)
	}
	defer resp.Body.Close()
	var tags struct{ Models []struct{ Name string } }
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil || len(tags.Models) == 0 {
		return "", errors.New(askSetup)
	}
	for _, m := range tags.Models {
		if strings.HasPrefix(m.Name, "llama3.2") {
			return m.Name, nil
		}
	}
	return tags.Models[0].Name, nil
}

func askModel(_ *Index, instructions, prompt string) (string, error) {
	base := ollamaBase()
	model, err := ollamaModel(base)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{
		"model":  model,
		"stream": false,
		"messages": []map[string]string{
			{"role": "system", "content": instructions},
			{"role": "user", "content": prompt},
		},
	})
	resp, err := ollamaClient.Post(base+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", errors.New(askSetup)
	}
	defer resp.Body.Close()
	var out struct {
		Message struct{ Content string }
		Error   string
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("ollama: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("ollama (%s): %s", model, out.Error)
	}
	return strings.TrimSpace(out.Message.Content), nil
}

// ---- status ----

// missingTools names the tools the vault's own files need but can't find,
// so a vault without recordings never nags about whisper.
func (ix *Index) missingTools() []string {
	need := map[string]bool{}
	rows, err := ix.db.Query(`SELECT DISTINCT kind FROM files WHERE kind IN ('image', 'pdf', 'audio')`)
	if err == nil {
		for rows.Next() {
			var k string
			rows.Scan(&k)
			need[k] = true
		}
		rows.Close()
	}
	out := []string{}
	if need["image"] && findTool("tesseract") == "" {
		out = append(out, "tesseract (text in images)")
	}
	if need["pdf"] && findTool("pdftotext") == "" {
		out = append(out, "pdftotext from poppler (PDFs)")
	}
	if need["audio"] && audioMissing() != "" {
		out = append(out, audioMissing())
	}
	return out
}

// audioMissing names what audio transcription lacks, "" when ready.
func audioMissing() string {
	switch {
	case whisperTool() == "" || whisperModel() == "":
		return "whisper-cli and a speech model (audio)"
	case findTool("ffmpeg") == "":
		return "ffmpeg (audio)"
	}
	return ""
}
