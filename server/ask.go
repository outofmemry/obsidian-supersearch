package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// "Ask your vault": the best keyword hits go to a local language model as
// numbered sources it cites: Apple's on-device model on macOS (through
// `supersearch-helper serve`), Ollama elsewhere (see askModel in backend_*.go).

const (
	askSources   = 6
	askChunkSize = 1000
)

// helperIdle is how long an unused helper stays loaded. A loaded helper holds
// 50-100 MB of Vision or language model (three idle OCR helpers were 280 MB
// next to a 28 MB server); starting one again costs about 0.13 s.
const helperIdle = 30 * time.Second

// helperProc is a `supersearch-helper serve` process: started on first use,
// kept while busy so models stay loaded, stopped once idle, restarted on failure.
type helperProc struct {
	nice      bool          // background work: run at low CPU priority
	idleAfter time.Duration // 0 = helperIdle
	mu        sync.Mutex
	cmd       *exec.Cmd
	in        io.WriteCloser
	out       *bufio.Reader
	last      time.Time   // end of the latest call
	idle      *time.Timer // fires stopIdle
}

func (h *helperProc) idleLimit() time.Duration {
	if h.idleAfter > 0 {
		return h.idleAfter
	}
	return helperIdle
}

// stopIdle ends the process unless a call finished within the idle limit.
func (h *helperProc) stopIdle() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmd != nil && time.Since(h.last) >= h.idleLimit() {
		h.cmd.Process.Kill()
		h.cmd.Wait()
		h.cmd = nil
	}
}

func (h *helperProc) call(req, resp any, timeout time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.idle != nil {
		h.idle.Stop()
	}
	defer func() {
		h.last = time.Now()
		h.idle = time.AfterFunc(h.idleLimit(), h.stopIdle)
	}()
	if h.cmd == nil {
		cmd := command(context.Background(), h.nice, helperPath, "serve")
		in, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		h.cmd, h.in, h.out = cmd, in, bufio.NewReaderSize(out, 1<<20)
	}
	cmd := h.cmd
	timer := time.AfterFunc(timeout, func() { cmd.Process.Kill() })
	defer timer.Stop()
	b, _ := json.Marshal(req)
	_, err := h.in.Write(append(b, '\n'))
	var line []byte
	if err == nil {
		line, err = h.out.ReadBytes('\n')
	}
	if err != nil { // dead or hung: next call starts a fresh one
		cmd.Process.Kill()
		cmd.Wait()
		h.cmd = nil
		return fmt.Errorf("helper: %w", err)
	}
	return json.Unmarshal(line, resp)
}

// stopWords are question words too common to help keyword retrieval.
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`what which when where does difference between about explain describe
		there their these those with from that this have into your should would could tell show give using used`) {
		stopWords[w] = true
	}
}

type askResponse struct {
	Answer  string   `json:"answer"`
	Sources []Result `json:"sources"`
}

// ask answers a question from the vault. Sources are the best keyword hits
// for any significant word, ranked by bm25 so rare terms like "FCFS" dominate.
func (ix *Index) ask(question string, scope []string) (askResponse, error) {
	qq := parseQuery(question)
	kw := qq
	kw.pos, kw.neg, kw.any = nil, nil, true
	for _, t := range qq.pos {
		if len([]rune(t.text)) > 3 && !stopWords[strings.ToLower(t.text)] {
			kw.pos = append(kw.pos, t)
		}
	}
	sources, err := ix.run(kw, scope, askSources)
	if err != nil {
		return askResponse{}, err
	}
	if len(sources) == 0 {
		return askResponse{Answer: "Nothing in your vault seems related to that question.", Sources: []Result{}}, nil
	}
	var sb strings.Builder
	for i, s := range sources {
		var body string
		ix.db.QueryRow(`SELECT substr(body, 1, ?) FROM chunks WHERE rowid = ?`, askChunkSize, s.rowid).Scan(&body)
		loc := s.Path
		if s.Page > 0 {
			loc += fmt.Sprintf(", page %d", s.Page)
		}
		fmt.Fprintf(&sb, "[%d] (%s)\n%s\n\n", i+1, loc, strings.TrimSpace(body))
	}
	sb.WriteString("Question: " + question)
	answer, err := askModel(ix, "You answer questions using only the numbered notes you are given. "+
		"Cite the notes you used like [1] or [2]. Be concise. If the notes do not contain the answer, say so in one sentence.",
		sb.String())
	return askResponse{Answer: answer, Sources: sources}, err
}
