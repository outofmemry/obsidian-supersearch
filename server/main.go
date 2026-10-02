// supersearch-server: the search sidecar for the supersearch Obsidian plugin.
// It owns extraction, OCR and the on-disk index so Obsidian holds none of it.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	vault := flag.String("vault", "", "absolute path of the Obsidian vault")
	dbPath := flag.String("db", "", "index file (default <vault>/.obsidian/plugins/supersearch/index.nosync/index.db)")
	query := flag.String("query", "", "index text files, run one search, print it and exit (no server, no OCR)")
	askQ := flag.String("ask", "", "answer one question from the vault and exit (uses the existing index)")
	listen := flag.String("listen", "127.0.0.1:0", "address to serve on; 127.0.0.1:0 = local only, random port")
	// Measured on an M2: 1 worker 9 images/s, 2 → 17, 3 → 24, 4 → 27. On
	// battery only one runs (see worker), which keeps a MacBook quiet.
	ocrWorkers := flag.Int("ocr-workers", 3, "parallel OCR jobs on mains power (1 on battery)")
	stdinExit := flag.Bool("exit-on-stdin-close", false, "exit when stdin closes, so the server never outlives Obsidian")
	flag.Parse()
	if *vault == "" {
		log.Fatal("-vault is required")
	}
	pluginDir := filepath.Join(*vault, ".obsidian", "plugins", "supersearch")
	if *dbPath == "" {
		*dbPath = filepath.Join(pluginDir, "index.nosync", "index.db")
	}
	// Settings come from the plugin's own settings file, never from flags, so
	// every server on this vault (the plugin's, ./server.sh, a remote one on
	// the synced vault) agrees. Two servers with different OCR settings used
	// to make each other re-OCR every image on every start.
	ignore, err := loadSettings(filepath.Join(pluginDir, "data.json"))
	if err != nil {
		log.Fatal("settings: ", err)
	}
	if exe, err := os.Executable(); err == nil {
		helperPath = filepath.Join(filepath.Dir(exe), "supersearch-helper")
	}

	ix, err := openIndex(*dbPath, *vault)
	if err != nil {
		log.Fatal(err)
	}
	ix.ignore = ignore

	if *query != "" {
		runQuery(ix, *query)
		return
	}
	if *askQ != "" {
		res, err := ix.ask(*askQ, nil)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(res.Answer)
		for i, s := range res.Sources {
			fmt.Printf("[%d] %s p%d L%d\n", i+1, s.Path, s.Page, s.Line)
		}
		return
	}

	token := os.Getenv("SUPERSEARCH_TOKEN")
	if token == "" {
		log.Fatal("SUPERSEARCH_TOKEN must be set")
	}
	if *stdinExit {
		go func() {
			io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}()
	}

	// One fast worker for text/pdf/office, N slow ones for OCR, so a big OCR
	// backlog never delays a note you just edited.
	for n := range *ocrWorkers + 1 {
		wake := make(chan struct{}, 1)
		ix.wakes = append(ix.wakes, wake)
		if n == 0 {
			go ix.worker("pending", 0, wake)
		} else {
			go ix.worker("ocr", n-1, wake)
		}
	}
	// Full scan at startup (and retry files that failed), then a cheap rescan
	// every minute: catches anything Obsidian never reported, and is the only
	// change feed for a remote server whose vault is synced from elsewhere.
	go func() {
		for retry := true; ; retry = false {
			if err := ix.scan(retry); err != nil {
				log.Println("scan:", err)
			}
			time.Sleep(time.Minute)
		}
	}()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("LISTENING %d\n", ln.Addr().(*net.TCPAddr).Port)
	log.Fatal(http.Serve(ln, handler(ix, token)))
}

func handler(ix *Index, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 200 {
			limit = 30
		}
		start := time.Now()
		res, err := ix.search(r.URL.Query().Get("q"), r.URL.Query()["scope"], limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"results": res.Results, "corrected": res.Corrected,
			"ms": float64(time.Since(start).Microseconds()) / 1000})
	})
	mux.HandleFunc("POST /ask", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Question string
			Scope    []string
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.Question) == "" {
			http.Error(w, "bad question", http.StatusBadRequest)
			return
		}
		res, err := ix.ask(body.Question, body.Scope)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("POST /changed", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Path, OldPath string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || !vaultPath(body.Path) {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		var err error
		if body.OldPath != "" && vaultPath(body.OldPath) {
			err = ix.rename(body.OldPath, body.Path)
		} else {
			err = ix.touch(body.Path)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /pause", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Paused bool }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		ix.paused.Store(body.Paused)
		ix.wake()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		if err := ix.reset(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		s, err := ix.status()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// vaultPath accepts only vault-relative paths the scanner would also index:
// no escaping the vault, no dot-folders.
func vaultPath(rel string) bool {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// loadSettings reads the plugin's data.json into the OCR globals and returns
// the ignored folders. A missing file means defaults.
func loadSettings(file string) ([]string, error) {
	var s struct {
		Ignore  string
		OcrLang string
	}
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	ocrLang = s.OcrLang
	var ignore []string
	for _, dir := range strings.Split(s.Ignore, "\n") {
		if dir = strings.Trim(filepath.ToSlash(strings.TrimSpace(dir)), "/"); dir != "" {
			ignore = append(ignore, dir)
		}
	}
	return ignore, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func runQuery(ix *Index, q string) {
	start := time.Now()
	if err := ix.scan(true); err != nil {
		log.Fatal(err)
	}
	for ix.processOne("pending", nil) {
	}
	indexed := time.Since(start)
	start = time.Now()
	res, err := ix.search(q, nil, 10)
	if err != nil {
		log.Fatal(err)
	}
	took := time.Since(start)
	if res.Corrected != "" {
		fmt.Printf("showing results for: %s\n", res.Corrected)
	}
	for _, r := range res.Results {
		snippet := strings.NewReplacer("\x02", "[", "\x03", "]", "\n", " ").Replace(r.Snippet)
		fmt.Printf("%s  p%d L%d %s\n    %s\n", r.Path, r.Page, r.Line, r.Source, snippet)
	}
	s, _ := ix.status()
	fmt.Printf("\n%d results in %v (scan+index %v) %v\n", len(res.Results), took, indexed, s["counts"])
}
