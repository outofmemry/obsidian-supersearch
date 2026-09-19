package main

import (
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Query syntax:
//   word        prefix match ("sched" finds "scheduling")
//   "a b"       exact phrase
//   -word       exclude
//   path:x      path contains x (-path:x excludes)   ext:pdf   in:note|image|pdf|doc|ocr
// Everything the user types is quoted before it reaches FTS5, so no input
// can be an FTS5 syntax error.

type term struct {
	text   string
	phrase bool
	alts   []string // typo corrections; when set they replace text
}

type query struct {
	any             bool // OR the positive terms instead of AND
	pos, neg        []term
	paths, notPaths []string
	exts, kinds     []string
}

func hasToken(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

func parseQuery(q string) query {
	var qq query
	for q = strings.TrimSpace(q); q != ""; q = strings.TrimSpace(q) {
		neg := strings.HasPrefix(q, "-") && len(q) > 1
		if neg {
			q = q[1:]
		}
		key := ""
		if i := strings.IndexAny(q, `: "`); i > 0 && q[i] == ':' {
			switch k := strings.ToLower(q[:i]); k {
			case "path", "ext", "in":
				key, q = k, q[i+1:]
			}
		}
		var word string
		phrase := false
		if strings.HasPrefix(q, `"`) {
			if end := strings.Index(q[1:], `"`); end >= 0 {
				word, q, phrase = q[1:1+end], q[end+2:], true
			} else {
				word, q, _ = strings.Cut(q[1:], " ") // unclosed quote: bare word
			}
		} else {
			word, q, _ = strings.Cut(q, " ")
		}
		word = strings.ReplaceAll(word, `"`, "")
		if word == "" {
			continue
		}
		switch {
		case key == "path" && neg:
			qq.notPaths = append(qq.notPaths, word)
		case key == "path":
			qq.paths = append(qq.paths, word)
		case key == "ext":
			qq.exts = append(qq.exts, strings.TrimPrefix(strings.ToLower(word), "."))
		case key == "in":
			qq.kinds = append(qq.kinds, strings.ToLower(word))
		case !hasToken(word): // FTS5 errors on a term with no tokens
		case neg:
			qq.neg = append(qq.neg, term{text: word, phrase: phrase})
		default:
			qq.pos = append(qq.pos, term{text: word, phrase: phrase})
		}
	}
	return qq
}

func (t term) fts() string {
	if len(t.alts) > 0 {
		alts := make([]string, len(t.alts))
		for i, a := range t.alts {
			alts[i] = `"` + a + `"`
		}
		return "(" + strings.Join(alts, " OR ") + ")"
	}
	s := `"` + strings.ReplaceAll(t.text, `"`, `""`) + `"`
	if !t.phrase {
		s += "*"
	}
	return s
}

func (qq query) match() string {
	if len(qq.pos) == 0 {
		return "" // FTS5 can't answer a purely negative query
	}
	join := func(ts []term, sep string) string {
		parts := make([]string, len(ts))
		for i, t := range ts {
			parts[i] = t.fts()
		}
		return strings.Join(parts, sep)
	}
	// Explicit AND: FTS5 has no implicit AND after a parenthesized group.
	op := " AND "
	if qq.any {
		op = " OR "
	}
	if len(qq.neg) == 0 {
		return join(qq.pos, op)
	}
	return "(" + join(qq.pos, op) + ") NOT (" + join(qq.neg, " OR ") + ")"
}

var inKinds = map[string]string{
	"note": "f.kind = 'text'", "notes": "f.kind = 'text'", "md": "f.kind = 'text'",
	"image": "f.kind = 'image'", "images": "f.kind = 'image'", "img": "f.kind = 'image'",
	"pdf": "f.kind = 'pdf'", "doc": "f.kind IN ('ooxml', 'textutil', 'epub', 'html')",
	"ocr": "chunks.source = 'ocr'", "audio": "f.kind = 'audio'",
}

func likeArg(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

type searchResponse struct {
	Results   []Result `json:"results"`
	Corrected string   `json:"corrected,omitempty"` // set when typo correction was applied
}

const perFile = 5 // max rows one file may take in a result list

// search ranks notes (and other plain-text files) first, then pdf, office
// and OCR hits; within each group by bm25, nudged towards recently edited
// files. scope, when set, restricts results to those exact paths.
func (ix *Index) search(q string, scope []string, limit int) (searchResponse, error) {
	qq := parseQuery(q)
	res, err := ix.run(qq, scope, limit)
	if err != nil || len(res) > 0 || !ix.correct(&qq) {
		return searchResponse{Results: res}, err
	}
	res, err = ix.run(qq, scope, limit)
	words := make([]string, len(qq.pos))
	for i, t := range qq.pos {
		words[i] = t.text
		if len(t.alts) > 0 {
			words[i] = t.alts[0]
		}
	}
	return searchResponse{Results: res, Corrected: strings.Join(words, " ")}, err
}

// filters turns path:/ext:/in: and scope into SQL over files f / chunks.
func filters(qq query, scope []string) (where []string, args []any) {
	for _, p := range qq.paths {
		where = append(where, `f.path LIKE ? ESCAPE '\'`)
		args = append(args, "%"+likeArg(p)+"%")
	}
	for _, p := range qq.notPaths {
		where = append(where, `f.path NOT LIKE ? ESCAPE '\'`)
		args = append(args, "%"+likeArg(p)+"%")
	}
	var or []string
	for _, e := range qq.exts {
		or = append(or, `f.path LIKE ? ESCAPE '\'`)
		args = append(args, "%."+likeArg(e))
	}
	if len(or) > 0 {
		where = append(where, "("+strings.Join(or, " OR ")+")")
	}
	or = nil
	for _, k := range qq.kinds {
		if c, ok := inKinds[k]; ok {
			or = append(or, c)
		}
	}
	if len(or) > 0 {
		where = append(where, "("+strings.Join(or, " OR ")+")")
	}
	if len(scope) > 0 {
		where = append(where, "f.path IN (?"+strings.Repeat(",?", len(scope)-1)+")")
		for _, p := range scope {
			args = append(args, p)
		}
	}
	return where, args
}

func (ix *Index) run(qq query, scope []string, limit int) ([]Result, error) {
	results := []Result{}
	match := qq.match()
	if match == "" {
		return results, nil
	}
	fw, fa := filters(qq, scope)
	where := append([]string{"chunks MATCH ?"}, fw...)
	args := append([]any{1<<pageBits - 1, time.Now().UnixMilli(), pageBits, match}, fa...)
	// Fetch extra rows so capping each file at perFile still fills the list.
	args = append(args, limit*perFile)
	// bm25 is negative (lower = better); the recency factor scales it by up to
	// 1.2 for a file edited today, fading over a few months.
	rows, err := ix.db.Query(`SELECT chunks.rowid, f.path, f.kind, chunks.rowid & ?, chunks.source,
			bm25(chunks, 8.0, 4.0, 1.0) * (1 + 0.2 * 30.0 / (30.0 + max(0, ? - f.mtime) / 86400000.0)) AS score
		FROM chunks JOIN files f ON f.id = chunks.rowid >> ?
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY f.kind != 'text', score LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Keep each file's rows together, in the order of its best row.
	var order []string
	byFile := map[string][]Result{}
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.rowid, &r.Path, &r.Kind, &r.Page, &r.Source, &r.Score); err != nil {
			return nil, err
		}
		if r.Kind == "text" && r.Page > 0 { // note section: page slot holds start line + 1
			r.Line, r.Page = r.Page-1, 0
		}
		if byFile[r.Path] == nil {
			order = append(order, r.Path)
		}
		if len(byFile[r.Path]) < perFile {
			byFile[r.Path] = append(byFile[r.Path], r)
		}
	}
	for _, p := range order {
		results = append(results, byFile[p]...)
	}
	if len(results) > limit {
		results = results[:limit]
	}
	if err := rows.Err(); err != nil || len(results) == 0 {
		return results, err
	}
	rows.Close()
	return results, ix.snippets(match, results)
}

// snippets fills in the highlighted excerpts, only for the rows that are
// shown: snippet() costs more than ranking, and running it on every ranked
// candidate was 75% of a broad query like "the". The unary + keeps this one
// scan of the matches; without it SQLite restarts the full-text query per
// rowid, which is far slower (234 ms vs 7 ms for "t").
func (ix *Index) snippets(match string, results []Result) error {
	args := []any{match}
	at := make(map[int64]int, len(results))
	for i, r := range results {
		args = append(args, r.rowid)
		at[r.rowid] = i
	}
	rows, err := ix.db.Query(`SELECT rowid, snippet(chunks, 2, char(2), char(3), '…', 24) FROM chunks
		WHERE chunks MATCH ? AND +rowid IN (?`+strings.Repeat(",?", len(results)-1)+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var snippet string
		if err := rows.Scan(&id, &snippet); err != nil {
			return err
		}
		results[at[id]].Snippet = snippet
	}
	return rows.Err()
}

// correct replaces each word that matches nothing with the closest indexed
// words. Only runs after a search came back empty, so it costs nothing when
// the query is fine.
func (ix *Index) correct(qq *query) bool {
	changed := false
	for i := range qq.pos {
		t := &qq.pos[i]
		w := strings.ToLower(t.text)
		if t.phrase || utf8.RuneCountInString(w) < 4 || strings.IndexFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) >= 0 {
			continue // phrases, short words and multi-token words are left alone
		}
		if ix.db.QueryRow(`SELECT 1 FROM vocab WHERE term >= ? AND term < ? LIMIT 1`, w, w+"\U0010FFFF").Scan(new(int)) == nil {
			continue // the word itself matches: another word caused the miss
		}
		if t.alts = ix.similar(w); len(t.alts) > 0 {
			changed = true
		}
	}
	return changed
}

// similar returns up to 3 indexed words within edit distance 1 (2 for words
// of 8+ letters) of w or of a prefix of them, so a half-typed word with a
// typo still finds its completion. Only words starting with w's first letter
// are considered: first-letter typos are rare and it keeps the scan small.
// ponytail: scans all vocab words with that first letter; fine for a vault,
// add a trigram index over vocab if this ever shows up in latency.
func (ix *Index) similar(w string) []string {
	first, _ := utf8.DecodeRuneInString(w)
	rows, err := ix.db.Query(`SELECT term, doc FROM vocab WHERE term >= ? AND term < ?`, string(first), string(first+1))
	if err != nil {
		return nil
	}
	defer rows.Close()
	wr := []rune(w)
	maxDist := 1
	if len(wr) >= 8 {
		maxDist = 2
	}
	type cand struct {
		term      string
		dist, doc int
	}
	var cands []cand
	for rows.Next() {
		var term string
		var doc int
		rows.Scan(&term, &doc)
		tr := []rune(term)
		if len(tr) < len(wr)-maxDist {
			continue
		}
		best := maxDist + 1
		for k := len(wr) - 1; k <= len(wr)+1 && k <= len(tr); k++ { // compare against prefixes around w's length
			if k > 0 {
				best = min(best, osa(wr, tr[:k]))
			}
		}
		if best <= maxDist {
			cands = append(cands, cand{term, best, doc})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].dist != cands[j].dist {
			return cands[i].dist < cands[j].dist
		}
		return cands[i].doc > cands[j].doc
	})
	var out []string
	for _, c := range cands[:min(3, len(cands))] {
		out = append(out, c.term)
	}
	return out
}

// osa is the optimal-string-alignment edit distance: insert, delete,
// substitute, or swap two adjacent letters, each costing 1.
func osa(a, b []rune) int {
	prev2 := make([]int, len(b)+1)
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev, cur = prev, cur, slices.Clone(prev2)
	}
	return prev[len(b)]
}
