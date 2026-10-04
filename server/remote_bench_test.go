package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Local vs remote image OCR through the real worker pool. Skipped unless asked:
//
//	BENCH_REMOTE=1 go test -tags sqlite_fts5 -v -run TestBenchRemoteOCR .
func TestBenchRemoteOCR(t *testing.T) {
	if os.Getenv("BENCH_REMOTE") == "" {
		t.Skip()
	}
	useHelper(t)
	const notes, perNote = 12, 5
	latency := envDuration("BENCH_LATENCY", 300*time.Millisecond) // a typical Drive/CDN first byte
	src := t.TempDir()
	pdf := filepath.Join(src, "p.pdf")
	write(t, pdf, makePDF("Remote benchmark wombat"))
	pdfToPNG(t, pdf, filepath.Join(src, "p.png"))
	img, _ := os.ReadFile(filepath.Join(src, "p.png"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(latency)
		w.Header().Set("Content-Type", "image/png")
		w.Write(img)
	}))
	defer srv.Close()

	run := func(name string, setup func(vault string)) {
		vault := t.TempDir()
		setup(vault)
		ix, err := openIndex(filepath.Join(t.TempDir(), "index.db"), vault)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		ix.startWorkers(3) // the server's default
		ix.scan(true)
		for {
			s, _ := ix.status()
			c := s["counts"].(map[string]int)
			if c["pending"]+c["ocr"] == 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		took := time.Since(start)
		hits := len(find(t, ix, "wombat"))
		t.Logf("%-28s %d images in %v (%.1f images/s), %d hits", name, notes*perNote, took.Round(time.Millisecond),
			float64(notes*perNote)/took.Seconds(), hits)
	}
	run("local images", func(vault string) {
		for i := range notes * perNote {
			write(t, filepath.Join(vault, fmt.Sprintf("img%d.png", i)), img)
		}
	})
	run("remote images", func(vault string) {
		for n := range notes {
			var sb strings.Builder
			fmt.Fprintf(&sb, "# Note %d\n", n)
			for i := range perNote {
				fmt.Fprintf(&sb, "![](%s/n%d-i%d.png)\n", srv.URL, n, i)
			}
			write(t, filepath.Join(vault, fmt.Sprintf("note%d.md", n)), []byte(sb.String()))
		}
	})
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

// One real remote image through the whole pipeline: download, OCR, index,
// search. Then the same image embedded many times under distinct URLs (so
// the per-URL cache can't help) to time a realistic remote backlog:
//
//	REMOTE_URL=https://lh3.googleusercontent.com/d/… REMOTE_EXPECT="jenkins,linting" \
//	  go test -tags sqlite_fts5 -v -run TestRealRemoteURL .
func TestRealRemoteURL(t *testing.T) {
	url := os.Getenv("REMOTE_URL")
	if url == "" {
		t.Skip()
	}
	useHelper(t)
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	index := func(notes map[string]string) (*Index, time.Duration) {
		vault := t.TempDir()
		for p, body := range notes {
			write(t, filepath.Join(vault, p), []byte(body))
		}
		ix, err := openIndex(filepath.Join(t.TempDir(), "index.db"), vault)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		ix.startWorkers(3)
		ix.scan(true)
		for {
			s, _ := ix.status()
			if c := s["counts"].(map[string]int); c["pending"]+c["ocr"] == 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		return ix, time.Since(start)
	}

	ix, took := index(map[string]string{"slides.md": "# Slides\n![](" + url + ")\n"})
	var body string
	ix.db.QueryRow(`SELECT body FROM remote_ocr`).Scan(&body)
	t.Logf("one image: %v, OCR text: %q", took.Round(time.Millisecond), strings.Join(strings.Fields(body), " "))
	for _, w := range strings.Split(os.Getenv("REMOTE_EXPECT"), ",") {
		if w = strings.TrimSpace(w); w == "" {
			continue
		}
		if res := find(t, ix, w); len(res) != 1 || res[0].Path != "slides.md" || res[0].Source != "ocr" {
			t.Errorf("search %q = %+v, want the note via its remote image", w, res)
		}
	}

	const notes, perNote = 4, 5
	many := map[string]string{}
	for n := range notes {
		var sb strings.Builder
		for i := range perNote {
			fmt.Fprintf(&sb, "![](%s%sss=%d-%d)\n", url, sep, n, i)
		}
		many[fmt.Sprintf("note%d.md", n)] = sb.String()
	}
	ix, took = index(many)
	var cached int
	ix.db.QueryRow(`SELECT count(*) FROM remote_ocr`).Scan(&cached)
	t.Logf("%d remote images in %d notes: %v (%.1f images/s), %d read", notes*perNote, notes, took.Round(time.Millisecond),
		float64(notes*perNote)/took.Seconds(), cached)
}
