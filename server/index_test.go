package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// makePDF builds a minimal pdf, one page per text with a real text layer
// ("" = a blank page with no text layer).
func makePDF(texts ...string) []byte {
	objs := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	var kids []string
	for _, text := range texts {
		stream := "BT /F1 40 Tf 60 650 Td (" + text + ") Tj ET"
		objs = append(objs, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents "+
			strconv.Itoa(len(objs)+2)+" 0 R /Resources << /Font << /F1 3 0 R >> >> >>",
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
		kids = append(kids, strconv.Itoa(len(objs)-1)+" 0 R")
	}
	objs[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(texts))
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func makeDocx(t *testing.T, path, text string) {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("word/document.xml")
	// the word is split across two runs on purpose: Word does this all the time
	fmt.Fprintf(w, `<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>%s</w:t></w:r><w:r><w:t>%s</w:t></w:r></w:p></w:body></w:document>`, text[:3], text[3:])
	zw.Close()
	write(t, path, b.Bytes())
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func drain(ix *Index) {
	h := &helperProc{nice: true}
	for ix.processOne("pending", nil) {
	}
	for ix.processOne("ocr", h) {
	}
}

func find(t *testing.T, ix *Index, q string) []Result {
	t.Helper()
	res, err := ix.search(q, nil, 10)
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	return res.Results
}

func expect(t *testing.T, ix *Index, q, path string, page int, source string) {
	t.Helper()
	res := find(t, ix, q)
	if len(res) != 1 || res[0].Path != path || res[0].Page != page || res[0].Source != source {
		t.Errorf("search %q = %+v, want exactly %s p%d %s", q, res, path, page, source)
	}
}

func TestIndex(t *testing.T) {
	vault := t.TempDir()
	write(t, filepath.Join(vault, "notes/alpha.md"), []byte("# Alpha\nthe zebracorn lives here"))
	write(t, filepath.Join(vault, ".obsidian/secret.md"), []byte("hiddenword"))
	makeDocx(t, filepath.Join(vault, "report.docx"), "quokkafile")

	ix, err := openIndex(filepath.Join(t.TempDir(), "index.db"), vault)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.scan(true); err != nil {
		t.Fatal(err)
	}
	drain(ix)

	expect(t, ix, "zebracorn", "notes/alpha.md", 0, "text")
	expect(t, ix, "zebrac", "notes/alpha.md", 0, "text") // prefix as you type
	expect(t, ix, `"zebracorn lives"`, "notes/alpha.md", 0, "text")
	expect(t, ix, "alpha", "notes/alpha.md", 0, "text") // by file name
	expect(t, ix, "quokkafile", "report.docx", 0, "text")
	if res := find(t, ix, "hiddenword"); len(res) != 0 {
		t.Errorf("dot-folder was indexed: %+v", res)
	}
	for _, q := range []string{`"`, `-`, `a" OR b`, `NEAR(`, `*`, `title:x`, ``} { // none may be an FTS5 syntax error
		find(t, ix, q)
	}

	// rename keeps the extracted text without re-running extraction
	os.Rename(filepath.Join(vault, "notes/alpha.md"), filepath.Join(vault, "notes/beta.md"))
	if err := ix.rename("notes/alpha.md", "notes/beta.md"); err != nil {
		t.Fatal(err)
	}
	if _, pending := ix.next("pending"); pending {
		t.Error("rename queued a re-extraction")
	}
	expect(t, ix, "zebracorn", "notes/beta.md", 0, "text")
	expect(t, ix, "beta", "notes/beta.md", 0, "text")

	// modify
	write(t, filepath.Join(vault, "notes/beta.md"), []byte("now about a wombatross instead"))
	ix.touch("notes/beta.md")
	drain(ix)
	expect(t, ix, "wombatross", "notes/beta.md", 0, "text")
	if res := find(t, ix, "zebracorn"); len(res) != 0 {
		t.Errorf("stale text survived a modify: %+v", res)
	}

	// deleted while the server was down: the startup scan must notice
	os.Remove(filepath.Join(vault, "report.docx"))
	if err := ix.scan(true); err != nil {
		t.Fatal(err)
	}
	if res := find(t, ix, "quokkafile"); len(res) != 0 {
		t.Errorf("scan kept a deleted file: %+v", res)
	}

	// reset: everything gone (nothing left on disk), then rebuilt from the vault
	stale := job{id: 1, rel: "notes/beta.md", kind: "text"} // an extraction still running during the reset
	if err := ix.reset(); err != nil {
		t.Fatal(err)
	}
	var left int
	ix.db.QueryRow(`SELECT (SELECT count(*) FROM chunks) + (SELECT count(*) FROM files WHERE status = 'done')`).Scan(&left)
	if left != 0 {
		t.Errorf("reset left %d rows", left)
	}
	ix.store(stale, []page{{source: "text", body: "ghosttext"}}, "done", "") // finishes after the reset
	if got := paths(find(t, ix, "ghosttext")); got != "" {
		t.Errorf("job that outlived the reset wrote orphan chunks: %s", got)
	}
	drain(ix)
	expect(t, ix, "wombatross", "notes/beta.md", 0, "text")

	// delete a whole folder
	os.RemoveAll(filepath.Join(vault, "notes"))
	ix.touch("notes")
	if res := find(t, ix, "wombatross"); len(res) != 0 {
		t.Errorf("deleted file still found: %+v", res)
	}
	var files, chunks int
	ix.db.QueryRow(`SELECT count(*) FROM files`).Scan(&files)
	ix.db.QueryRow(`SELECT count(*) FROM chunks`).Scan(&chunks)
	if files != 0 || chunks != 0 {
		t.Errorf("everything was deleted but %d files / %d chunks remain", files, chunks)
	}
}

// useHelper points extraction at the Swift helper built next to the tests
// (install.sh / make test build it).
func useHelper(t *testing.T) {
	t.Helper()
	abs, _ := filepath.Abs("supersearch-helper")
	if _, err := os.Stat(abs); err != nil {
		t.Skip("supersearch-helper not built")
	}
	helperPath = abs
}

func TestPDFAndOCR(t *testing.T) {
	useHelper(t)
	vault := t.TempDir()
	pdf := filepath.Join(vault, "paper.pdf")
	write(t, pdf, makePDF("Platypus migration report"))
	// render the pdf to a png (an image containing text): images are OCR'd,
	// but PDFs are text layer only, so a scanned pdf with no text layer has
	// no content to index.
	if out, err := exec.Command("sips", "-s", "format", "png", pdf, "--out", filepath.Join(vault, "shot.png")).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if out, err := exec.Command("sips", "-s", "format", "pdf", filepath.Join(vault, "shot.png"), "--out", filepath.Join(vault, "scanned.pdf")).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}

	ix, err := openIndex(filepath.Join(t.TempDir(), "index.db"), vault)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.scan(true); err != nil {
		t.Fatal(err)
	}
	drain(ix)

	if res := find(t, ix, "shot"); len(res) != 0 {
		t.Errorf("image matched by its file name: %+v", res)
	}
	write(t, filepath.Join(vault, "zz.md"), []byte("a long note that mentions the platypus migration once, in passing, among many other words"))
	ix.touch("zz.md")
	drain(ix)
	res := find(t, ix, "platypus migration")
	if len(res) == 0 || res[0].Path != "zz.md" {
		t.Errorf("notes must rank first, got %+v", res)
	}
	res = res[1:]
	got := map[string]string{}
	for _, r := range res {
		got[r.Path] = fmt.Sprintf("p%d %s", r.Page, r.Source)
	}
	want := map[string]string{"paper.pdf": "p1 text", "shot.png": "p0 ocr"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// a short title-only page keeps its exact text layer (no OCR replacement)
	write(t, filepath.Join(vault, "title.pdf"), makePDF("Data Structures and Algorithms", "Dynamic Arrays"))
	ix.touch("title.pdf")
	drain(ix)
	expect(t, ix, "dynamic arrays", "title.pdf", 2, "text")
	if s, _ := ix.status(); s["counts"].(map[string]int)["done"] != 5 {
		t.Errorf("status = %v, want 5 done", s)
	}
}

func newIndex(t *testing.T, vault string, files map[string]string) *Index {
	t.Helper()
	for p, body := range files {
		write(t, filepath.Join(vault, p), []byte(body))
	}
	ix, err := openIndex(filepath.Join(t.TempDir(), "index.db"), vault)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func paths(res []Result) string {
	var ps []string
	for _, r := range res {
		ps = append(ps, fmt.Sprintf("%s:%d", r.Path, r.Line))
	}
	return strings.Join(ps, " ")
}

func TestQueryFeatures(t *testing.T) {
	vault := t.TempDir()
	ix := newIndex(t, vault, map[string]string{
		"os/sched.md":    "---\ntags: [kernel, cpu]\n---\nintro text\n# Round Robin\nquantum based scheduling\n```\n# not a heading\n```\n## Priority\nstarvation happens #hard-topic\n",
		"os/other.md":    "mentions round robin in passing, and priority, and scheduling",
		"web/page.html":  "<html><head><style>.x{}</style><script>var hidden=1</script></head><body><p>Kangaroo&amp;co</p></body></html>",
		"draw.svg":       `<svg xmlns="http://www.w3.org/2000/svg"><text x="1" y="2">Wallaby label</text></svg>`,
		"archive/old.md": "scheduling in the archive",
	})
	ix.ignore = []string{"archive"}
	if err := ix.scan(true); err != nil {
		t.Fatal(err)
	}
	drain(ix)

	// sections: a heading match ranks first and points at the heading's line
	if got := paths(find(t, ix, "priority")); got != "os/sched.md:9 os/other.md:0" {
		t.Errorf("heading ranking/lines: %s", got)
	}
	if got := paths(find(t, ix, "quantum")); got != "os/sched.md:4" {
		t.Errorf("section line: %s", got)
	}
	if got := paths(find(t, ix, "hard")); got != "os/sched.md:9" {
		t.Errorf("inline tag: %s", got)
	}
	if got := paths(find(t, ix, "kernel")); got != "os/sched.md:0" {
		t.Errorf("frontmatter tag: %s", got)
	}
	// filters
	if got := paths(find(t, ix, "scheduling path:other")); got != "os/other.md:0" {
		t.Errorf("path filter: %s", got)
	}
	if got := paths(find(t, ix, "scheduling -path:other")); got != "os/sched.md:4" {
		t.Errorf("negated path filter: %s", got)
	}
	if got := paths(find(t, ix, "scheduling -passing")); got != "os/sched.md:4" {
		t.Errorf("excluded word: %s", got)
	}
	if got := paths(find(t, ix, "kangaroo ext:html")); got != "web/page.html:0" {
		t.Errorf("ext filter / html: %s", got)
	}
	if got := paths(find(t, ix, "kangaroo in:image")); got != "" {
		t.Errorf("in: filter: %s", got)
	}
	if got := paths(find(t, ix, "hidden")); got != "" {
		t.Errorf("html script text was indexed: %s", got)
	}
	if got := paths(find(t, ix, "wallaby")); got != "draw.svg:0" {
		t.Errorf("svg text: %s", got)
	}
	// scope
	res, _ := ix.search("scheduling", []string{"os/other.md"}, 10)
	if got := paths(res.Results); got != "os/other.md:0" {
		t.Errorf("scope: %s", got)
	}
	// ignored folder never indexed, and dropped when a file moves into it
	if got := paths(find(t, ix, "archive")); got != "" {
		t.Errorf("ignored folder indexed: %s", got)
	}
	os.MkdirAll(filepath.Join(vault, "archive"), 0o755)
	os.Rename(filepath.Join(vault, "os/other.md"), filepath.Join(vault, "archive/other.md"))
	ix.rename("os/other.md", "archive/other.md")
	if got := paths(find(t, ix, "passing")); got != "" {
		t.Errorf("file moved into ignored folder still found: %s", got)
	}
	// typo tolerance, including a half-typed word with a typo
	for _, q := range []string{"schedulng", "shceduling", "quantm based", "rond robin"} {
		res, err := ix.search(q, nil, 10)
		if err != nil || len(res.Results) == 0 || res.Corrected == "" {
			t.Errorf("typo %q: %+v %v", q, res, err)
		}
	}
	if res, _ := ix.search("zzzzqqqq", nil, 10); len(res.Results) != 0 || res.Corrected != "" {
		t.Errorf("nonsense got corrected: %+v", res)
	}
	if res, _ := ix.search("scheduling", nil, 10); res.Corrected != "" {
		t.Errorf("correct word got corrected: %+v", res)
	}
}

func TestCompoundQuery(t *testing.T) {
	vault := t.TempDir()
	ix := newIndex(t, vault, map[string]string{
		"notes/dsa.md": "# DSA\nDynamic Arrays are contiguous memory.",
	})
	if err := ix.scan(true); err != nil {
		t.Fatal(err)
	}
	drain(ix)

	if got := paths(find(t, ix, "DynamicArrays")); got != "notes/dsa.md:0" {
		t.Errorf("spaceless compound: %s", got)
	}
	for _, q := range []string{"DynamixArrays", "DynamicArrais"} { // 1-letter typo per half
		res, err := ix.search(q, nil, 10)
		if err != nil || len(res.Results) == 0 || res.Corrected == "" {
			t.Errorf("compound typo %q: %+v %v", q, res, err)
		}
	}
	if res, _ := ix.search("zzzqqqxxx", nil, 10); len(res.Results) != 0 || res.Corrected != "" {
		t.Errorf("nonsense compound got corrected: %+v", res)
	}
}

func TestRemoteImageOCR(t *testing.T) {
	useHelper(t)
	vault := t.TempDir()
	// an image containing text, served over http like a Drive direct link
	pdf := filepath.Join(vault, "src.pdf")
	write(t, pdf, makePDF("Quokkawallaby remote"))
	if out, err := exec.Command("sips", "-s", "format", "png", pdf, "--out", filepath.Join(vault, "src.png")).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	img, err := os.ReadFile(filepath.Join(vault, "src.png"))
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(vault, "src.pdf"))
	os.Remove(filepath.Join(vault, "src.png"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pic.png" {
			w.Header().Set("Content-Type", "image/png")
			w.Write(img)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	write(t, filepath.Join(vault, "drive.md"),
		[]byte("# Drive\nnotes about the migration\n![pic]("+srv.URL+"/pic.png)\n![dead]("+srv.URL+"/gone.png)"))
	write(t, filepath.Join(vault, "plain.md"), []byte("just some words"))

	ix, err := openIndex(filepath.Join(t.TempDir(), "index.db"), vault)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.scan(true); err != nil {
		t.Fatal(err)
	}
	drain(ix)

	expect(t, ix, "quokkawallaby", "drive.md", 0, "ocr") // remote OCR text lives on the host note
	expect(t, ix, "quokkawallaby in:ocr", "drive.md", 0, "ocr")
	if got := paths(find(t, ix, "migration")); got != "drive.md:0" {
		t.Errorf("note text alongside remote images: %s", got)
	}
	if s, _ := ix.status(); s["counts"].(map[string]int)["done"] != 2 {
		t.Errorf("a dead image link must not fail its note: %v", s)
	}
	// no image bytes land in the vault: only the two notes exist
	var names string
	ix.db.QueryRow(`SELECT group_concat(path, ',') FROM files`).Scan(&names)
	if names != "drive.md,plain.md" && names != "plain.md,drive.md" {
		t.Errorf("unexpected files indexed: %s", names)
	}
}

func TestOSA(t *testing.T) {
	for _, c := range []struct {
		a, b string
		d    int
	}{{"", "abc", 3}, {"abc", "abc", 0}, {"abc", "acb", 1}, {"kitten", "sitting", 3}, {"schedulng", "scheduling", 1}} {
		if d := osa([]rune(c.a), []rune(c.b)); d != c.d {
			t.Errorf("osa(%q,%q) = %d, want %d", c.a, c.b, d, c.d)
		}
	}
}

const fcfsNote = "# FCFS\nFirst come first served: the CPU runs processes in the order they arrive, using a queue."

func TestAskAndAudio(t *testing.T) {
	useHelper(t)
	vault := t.TempDir()
	ix := newIndex(t, vault, map[string]string{"os/fcfs.md": fcfsNote, "misc/css.md": "# Flexbox\njustify-content centers items."})
	if out, err := exec.Command("say", "-o", filepath.Join(vault, "memo.m4a"), "--data-format=aac", "the round robin scheduler uses a time quantum").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	ix.scan(true)
	drain(ix)
	if got := paths(find(t, ix, "quantum in:audio")); got != "memo.m4a:0" {
		t.Errorf("audio transcript: %s", got)
	}

	ans, err := ix.ask("How does FCFS decide which process runs?", nil)
	if err != nil && strings.Contains(err.Error(), "not available") {
		t.Skip(err)
	}
	if err != nil || ans.Answer == "" || len(ans.Sources) == 0 || ans.Sources[0].Path != "os/fcfs.md" {
		t.Errorf("ask: %+v %v", ans, err)
	}
	t.Logf("answer: %s", ans.Answer)
}

func TestClaim(t *testing.T) {
	vault := t.TempDir()
	ix := newIndex(t, vault, map[string]string{"a.md": "a", "b.md": "b", "c.md": "c"})
	ix.scan(true)
	seen := map[int64]bool{}
	for range 3 { // three workers asking at once never get the same file
		j, ok := ix.next("pending")
		if !ok || seen[j.id] {
			t.Fatalf("claim %v %v %v", j, ok, seen)
		}
		seen[j.id] = true
	}
	if _, ok := ix.next("pending"); ok {
		t.Error("a fourth claim got a job that is already taken")
	}
	for id := range seen {
		ix.release(id)
	}
	if _, ok := ix.next("pending"); !ok {
		t.Error("released jobs are not claimable again")
	}
}

func TestSettingsDontFlipFlop(t *testing.T) {
	vault := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	write(t, filepath.Join(vault, "a.png"), []byte("x"))
	write(t, filepath.Join(vault, "b.pdf"), []byte("x"))
	settings := filepath.Join(t.TempDir(), "data.json")
	requeued := func(lang string) (images, pdfs int) {
		t.Helper()
		write(t, settings, []byte(fmt.Sprintf(`{"ignore":"Templates\n","ocrLang":%q}`, lang)))
		ignore, err := loadSettings(settings)
		if err != nil || len(ignore) != 1 || ignore[0] != "Templates" {
			t.Fatalf("settings: %v %v", ignore, err)
		}
		ix, err := openIndex(dbPath, vault)
		if err != nil {
			t.Fatal(err)
		}
		defer ix.db.Close()
		ix.scan(true)
		ix.db.QueryRow(`SELECT count(*) FROM files WHERE kind = 'image' AND status = 'ocr'`).Scan(&images)
		ix.db.QueryRow(`SELECT count(*) FROM files WHERE kind = 'pdf' AND status = 'pending'`).Scan(&pdfs)
		ix.db.Exec(`UPDATE files SET status = 'done'`) // pretend the workers finished
		return
	}
	requeued("") // first start: everything is new
	if i, p := requeued(""); i != 0 || p != 0 {
		t.Errorf("restart with the same settings requeued %d images, %d pdfs", i, p)
	}
	if i, p := requeued("hi-IN"); i != 1 || p != 0 {
		t.Errorf("ocr-lang change requeued %d images (want 1), %d pdfs (want 0)", i, p)
	}
	// the old single 'ocr' key upgrades without redoing images
	ix, _ := openIndex(dbPath, vault)
	ix.db.Exec(`DELETE FROM meta; INSERT INTO meta VALUES ('ocr', 'vision1||false')`)
	ix.db.Close()
	if i, p := requeued(""); i != 0 || p != 0 {
		t.Errorf("upgrade requeued %d images, %d pdfs", i, p)
	}
	// a pre-fix database with pdf OCR chunks re-extracts its pdfs once
	ix, _ = openIndex(dbPath, vault)
	ix.db.Exec(`UPDATE files SET status = 'ocr' WHERE kind = 'pdf'; INSERT OR REPLACE INTO meta VALUES ('pdf', 'vision1|en-US|false')`)
	ix.db.Close()
	if i, p := requeued(""); i != 0 || p != 1 {
		t.Errorf("pdf-ocr removal requeued %d images (want 0), %d pdfs (want 1)", i, p)
	}
}

func TestHelperStopsWhenIdle(t *testing.T) {
	useHelper(t)
	h := &helperProc{idleAfter: 200 * time.Millisecond}
	ask := func() {
		t.Helper()
		var resp struct{ Answer, Error string }
		if err := h.call(map[string]any{"op": "nope"}, &resp, time.Minute); err != nil || resp.Error == "" {
			t.Fatalf("call: %v %+v", err, resp)
		}
	}
	running := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.cmd != nil
	}
	ask()
	time.Sleep(100 * time.Millisecond)
	ask() // still in use: the idle clock restarts
	time.Sleep(150 * time.Millisecond)
	if !running() {
		t.Fatal("helper was stopped 150 ms after its last call (idle limit is 200 ms)")
	}
	time.Sleep(300 * time.Millisecond)
	if running() {
		t.Fatal("idle helper was not stopped")
	}
	ask() // and it comes back on demand
}
