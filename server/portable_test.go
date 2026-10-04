//go:build !darwin || portable

package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	write(t, path, b.Bytes())
}

func TestOCRInputFlattensAlpha(t *testing.T) {
	dir := t.TempDir()
	rgba := image.NewNRGBA(image.Rect(0, 0, 4, 4)) // fully transparent…
	rgba.Set(1, 1, color.NRGBA{0, 0, 0, 255})      // …with one dark pixel of "text"
	writePNG(t, filepath.Join(dir, "clear.png"), rgba)
	in, done, err := ocrInput(filepath.Join(dir, "clear.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if in == filepath.Join(dir, "clear.png") {
		t.Fatal("a transparent png must be flattened, not passed through")
	}
	f, _ := os.Open(in)
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if r, g, b, _ := img.At(0, 0).RGBA(); r>>8 != 255 || g>>8 != 255 || b>>8 != 255 {
		t.Errorf("transparent pixel became %v, want white", img.At(0, 0))
	}
	if r, _, _, _ := img.At(1, 1).RGBA(); r>>8 != 0 {
		t.Errorf("text pixel became %v, want black", img.At(1, 1))
	}

	// a remote image is downloaded without an extension: sniffed by content
	writePNG(t, filepath.Join(dir, "ss-remote-123"), rgba)
	if in, done, _ := ocrInput(filepath.Join(dir, "ss-remote-123")); in == filepath.Join(dir, "ss-remote-123") {
		t.Error("a transparent png without extension must be flattened too")
	} else {
		done()
	}

	gray := image.NewGray(image.Rect(0, 0, 4, 4))
	writePNG(t, filepath.Join(dir, "gray.png"), gray)
	if in, done, _ := ocrInput(filepath.Join(dir, "gray.png")); in != filepath.Join(dir, "gray.png") {
		done()
		t.Error("an opaque png must be passed through without decoding")
	}
}

func TestTessLangs(t *testing.T) {
	tess := findTool("tesseract")
	if tess == "" {
		t.Skip("tesseract not installed")
	}
	defer func(old string) { ocrLang = old }(ocrLang)
	for lang, want := range map[string]string{"": "eng", "en-US": "eng", "auto": "eng", "en-US,xx-YY": "eng", "eng": "eng"} {
		ocrLang = lang
		if got := tessLangs(tess); got != want {
			t.Errorf("tessLangs(%q) = %q, want %q", lang, got, want)
		}
	}
}

func TestLegacyDocs(t *testing.T) {
	dir := t.TempDir()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("content.xml")
	w.Write([]byte(`<office:document-content><office:body><office:text><text:p>odt wombat</text:p></office:text></office:body></office:document-content>`))
	zw.Close()
	write(t, filepath.Join(dir, "a.odt"), b.Bytes())
	write(t, filepath.Join(dir, "a.rtf"), []byte(`{\rtf1\ansi rtf numbat\par}`))
	for file, want := range map[string]string{"a.odt": "odt wombat", "a.rtf": "rtf numbat"} {
		got, err := legacyText(filepath.Join(dir, file))
		if err != nil || !strings.Contains(got, want) {
			t.Errorf("%s: %q %v", file, got, err)
		}
	}
}

func TestOllamaBase(t *testing.T) {
	for env, want := range map[string]string{
		"":                       "http://127.0.0.1:11434",
		"0.0.0.0":                "http://127.0.0.1:11434",
		"192.168.1.9:9999":       "http://192.168.1.9:9999",
		"https://ollama.example": "https://ollama.example:11434",
	} {
		t.Setenv("OLLAMA_HOST", env)
		if got := ollamaBase(); got != want {
			t.Errorf("OLLAMA_HOST=%q: %q, want %q", env, got, want)
		}
	}
}

// A fake Ollama: the same /api/tags and /api/chat shapes the real one serves.
func TestAskOllama(t *testing.T) {
	var got struct {
		Model    string
		Stream   bool
		Messages []struct{ Role, Content string }
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"qwen2.5:3b"},{"name":"llama3.2:latest"}]}`))
		case "/api/chat":
			json.NewDecoder(r.Body).Decode(&got)
			w.Write([]byte(`{"message":{"role":"assistant","content":" FCFS runs them in arrival order [1]. "},"done":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	t.Setenv("SUPERSEARCH_OLLAMA_MODEL", "")
	ix := newIndex(t, t.TempDir(), map[string]string{"os/fcfs.md": fcfsNote})
	ix.scan(true)
	drain(ix)
	res, err := ix.ask("How does FCFS decide which process runs?", nil)
	if err != nil || res.Answer != "FCFS runs them in arrival order [1]." || len(res.Sources) != 1 {
		t.Fatalf("ask: %+v %v", res, err)
	}
	if got.Model != "llama3.2:latest" || got.Stream || len(got.Messages) != 2 || got.Messages[0].Role != "system" ||
		!strings.Contains(got.Messages[1].Content, "[1] (os/fcfs.md)") {
		t.Errorf("request to ollama: %+v", got)
	}

	t.Setenv("OLLAMA_HOST", "127.0.0.1:1") // nothing listens: a setup hint, not a stack trace
	if _, err := ix.ask("How does FCFS decide which process runs?", nil); err == nil || !strings.Contains(err.Error(), "ollama pull") {
		t.Errorf("no ollama: %v", err)
	}
}
