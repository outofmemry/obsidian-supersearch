package main

import (
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	// The C library through cgo: measured 2.3× faster than the pure-Go port on
	// every query, and a smaller binary. Needs a C compiler (the Xcode tools the
	// Swift helper needs anyway) and the sqlite_fts5 build tag.
	_ "github.com/mattn/go-sqlite3"
)

const schema = `
CREATE TABLE IF NOT EXISTS files (
  id     INTEGER PRIMARY KEY,
  path   TEXT UNIQUE NOT NULL,  -- vault-relative, forward slashes
  kind   TEXT NOT NULL,         -- see kinds in extract.go
  mtime  INTEGER NOT NULL,
  size   INTEGER NOT NULL,
  status TEXT NOT NULL,         -- pending | ocr | done | error
  error  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS files_status ON files(status);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
-- One row per note section (page = start line + 1), pdf page (page = 1..n),
-- or whole file (page 0). rowid = file_id<<20 | page, so a file's chunks are
-- one cheap rowid range.
-- name: file name, first chunk only. title: section heading + tags.
CREATE VIRTUAL TABLE IF NOT EXISTS chunks USING ` + chunksDef + `;
CREATE VIRTUAL TABLE IF NOT EXISTS vocab USING fts5vocab(chunks, 'row');`

// Prefix indexes make as-you-type queries fast. 1 matters most: the first
// letter typed is the most common query, and without it "t" took twice as long.
const chunksDef = `fts5(name, title, body, source UNINDEXED,
  tokenize='unicode61 remove_diacritics 2', prefix='1 2 3')`

const (
	pageBits      = 20 // ponytail: caps a document at ~1M pages / a note at ~1M lines
	schemaVersion = 5
)

type Index struct {
	db     *sql.DB
	vault  string
	ignore []string    // vault-relative folders never indexed
	paused atomic.Bool // OCR workers idle while set
	wmu    sync.Mutex  // SQLite is single-writer; serialize here instead of juggling SQLITE_BUSY
	wakes  []chan struct{}

	ah *helperProc // `supersearch-helper serve`, for ask

	cmu     sync.Mutex
	claimed map[int64]bool // file ids a worker is processing right now
}

type page struct {
	n      int
	source string // text | ocr
	title  string // heading + tags
	body   string
}

type Result struct {
	Path    string  `json:"path"`
	Kind    string  `json:"kind"`
	Page    int     `json:"page"` // pdf page, 0 otherwise
	Line    int     `json:"line"` // first line of the matching note section
	Source  string  `json:"source"`
	Snippet string  `json:"snippet"` // match wrapped in \x02 … \x03
	Score   float64 `json:"score"`
	rowid   int64
}

func openIndex(dbPath, vault string) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + (&url.URL{Path: dbPath}).EscapedPath() +
		"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		if strings.Contains(err.Error(), "no such module: fts5") {
			return nil, errors.New("built without full-text search: build with -tags sqlite_fts5 (./install.sh does)")
		}
		return nil, err
	}
	var version int
	db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version < 2 { // chunk layout changed: rebuild chunks, re-extract everything
		_, err := db.Exec(`DROP TABLE vocab; DROP TABLE chunks;` + schema + `
			UPDATE files SET status = CASE WHEN kind IN ('image', 'audio') THEN 'ocr' ELSE 'pending' END`)
		if err != nil {
			return nil, err
		}
	}
	if version < 3 { // search by meaning was removed: drop its embeddings and give the space back
		if _, err := db.Exec(`DROP TABLE IF EXISTS vectors; DELETE FROM meta WHERE key = 'embed'`); err != nil {
			return nil, err
		}
		if version == 2 {
			db.Exec(`VACUUM`)
		}
	}
	if version == 2 || version == 3 { // add the 1-letter prefix index: copy the text over, nothing is re-read or re-OCR'd
		_, err := db.Exec(`BEGIN;
			CREATE VIRTUAL TABLE chunks_new USING ` + chunksDef + `;
			INSERT INTO chunks_new(rowid, name, title, body, source) SELECT rowid, name, title, body, source FROM chunks;
			DROP TABLE vocab; DROP TABLE chunks; ALTER TABLE chunks_new RENAME TO chunks;
			CREATE VIRTUAL TABLE vocab USING fts5vocab(chunks, 'row');
			COMMIT`)
		if err != nil {
			db.Exec(`ROLLBACK`)
			return nil, err
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = ` + strconv.Itoa(schemaVersion)); err != nil {
		return nil, err
	}
	// PDF OCR was removed in v5: PDFs are text layer only. A leftover 'pdf'
	// config key means old OCR chunks (misreads like "Dynamic Arrays" ->
	// "Oganic Armys", dropped short pages) that must be re-extracted once.
	// Any pdf stuck in ocr status is moved back to pending for the same
	// reason. Both are no-ops on databases that already migrated.
	if _, err := db.Exec(`UPDATE files SET status = 'pending' WHERE kind = 'pdf' AND status = 'ocr'`); err != nil {
		return nil, err
	}
	var pdfCfg string
	if db.QueryRow(`SELECT value FROM meta WHERE key = 'pdf'`).Scan(&pdfCfg) == nil {
		if _, err := db.Exec(`UPDATE files SET status = 'pending' WHERE kind = 'pdf';
			DELETE FROM meta WHERE key = 'pdf'`); err != nil {
			return nil, err
		}
	}
	// Upgrade: the legacy single 'ocr' key ("vision1|lang|figures") carried a
	// pdf-figures flag that no longer exists. Keep its language for images
	// and audio, drop the flag.
	var old string
	if db.QueryRow(`SELECT value FROM meta WHERE key = 'ocr'`).Scan(&old) == nil {
		if p := strings.Split(old, "|"); len(p) == 3 {
			db.Exec(`INSERT OR REPLACE INTO meta VALUES ('media', ?)`,
				p[0]+"|"+normLang(p[1]))
		}
		db.Exec(`DELETE FROM meta WHERE key = 'ocr'`)
	}
	// A different OCR engine or language redoes only the files it affects. Old
	// text stays searchable until each file's new text replaces it.
	for _, c := range []struct{ key, want, redo string }{
		{"media", mediaConfig(), `UPDATE files SET status = 'ocr' WHERE kind IN ('image', 'audio')`},
	} {
		var have string
		db.QueryRow(`SELECT value FROM meta WHERE key = ?`, c.key).Scan(&have)
		if have != c.want {
			if _, err := db.Exec(c.redo+`; INSERT OR REPLACE INTO meta VALUES (?, ?)`, c.key, c.want); err != nil {
				return nil, err
			}
		}
	}
	return &Index{db: db, vault: vault, ah: &helperProc{}}, nil
}

func (ix *Index) wake() {
	for _, c := range ix.wakes {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (ix *Index) ignored(rel string) bool {
	for _, p := range ix.ignore {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

type change struct {
	rel, kind   string
	mtime, size int64
}

// scan diffs the vault against the index: catches everything that changed
// while the server was down, or that Obsidian never reported.
func (ix *Index) scan(retryErrors bool) error {
	type row struct {
		kind        string
		mtime, size int64
		status      string
	}
	known := map[string]row{}
	rows, err := ix.db.Query(`SELECT path, kind, mtime, size, status FROM files`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p string
		var r row
		if err := rows.Scan(&p, &r.kind, &r.mtime, &r.size, &r.status); err != nil {
			rows.Close()
			return err
		}
		known[p] = r
	}
	rows.Close()

	var changed []change
	err = filepath.WalkDir(ix.vault, func(abs string, d fs.DirEntry, err error) error {
		if err != nil || abs == ix.vault {
			return nil // unreadable entry: skip, don't abort the scan
		}
		rel, _ := filepath.Rel(ix.vault, abs)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(d.Name(), ".") || ix.ignored(rel) {
			if d.IsDir() {
				return filepath.SkipDir // .obsidian, .trash, ignored folders
			}
			return nil
		}
		kind := kindOf(abs)
		if d.IsDir() || kind == "" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		c := change{rel, kind, info.ModTime().UnixMilli(), info.Size()}
		r, ok := known[rel]
		if !ok || r.mtime != c.mtime || r.size != c.size || r.kind != c.kind || (retryErrors && r.status == "error") {
			changed = append(changed, c)
		}
		delete(known, rel)
		return nil
	})
	if err != nil {
		return err
	}
	var removed []string
	for p := range known {
		removed = append(removed, p)
	}
	return ix.apply(changed, removed)
}

// touch re-checks one vault-relative path after Obsidian reports a change.
func (ix *Index) touch(rel string) error {
	info, err := os.Stat(filepath.Join(ix.vault, filepath.FromSlash(rel)))
	if os.IsNotExist(err) || ix.ignored(rel) { // gone: drop it, and anything under it if it was a folder
		return ix.apply(nil, []string{rel})
	}
	if err != nil || info.IsDir() {
		return nil
	}
	kind := kindOf(rel)
	if kind == "" {
		return nil
	}
	return ix.apply([]change{{rel, kind, info.ModTime().UnixMilli(), info.Size()}}, nil)
}

func (ix *Index) apply(changed []change, removed []string) error {
	if len(changed)+len(removed) == 0 {
		return nil
	}
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range removed {
		if err := removeTx(tx, p); err != nil {
			return err
		}
	}
	for _, c := range changed {
		status := "pending"
		if slow(c.kind) {
			status = "ocr"
		}
		// Old chunks stay searchable until the worker replaces them.
		_, err := tx.Exec(`INSERT INTO files(path, kind, mtime, size, status) VALUES(?,?,?,?,?)
			ON CONFLICT(path) DO UPDATE SET kind=excluded.kind, mtime=excluded.mtime,
			size=excluded.size, status=excluded.status, error=''
			WHERE mtime != excluded.mtime OR size != excluded.size OR kind != excluded.kind OR status = 'error'`,
			c.rel, c.kind, c.mtime, c.size, status)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	ix.wake()
	return nil
}

// removeTx deletes a file, or every file under it when p is a folder.
func removeTx(tx *sql.Tx, p string) error {
	rows, err := tx.Query(`SELECT id FROM files WHERE path = ?1 OR substr(path, 1, length(?1)+1) = ?1 || '/'`, p)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := deleteChunks(tx, id<<pageBits, (id+1)<<pageBits-1); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM files WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// deleteChunks removes a rowid range of chunks.
func deleteChunks(tx *sql.Tx, from, to int64) error {
	_, err := tx.Exec(`DELETE FROM chunks WHERE rowid BETWEEN ? AND ?`, from, to)
	return err
}

// rename moves a file's index entry so extracted text (OCR!) is not redone.
func (ix *Index) rename(oldRel, newRel string) error {
	if ix.ignored(newRel) {
		return ix.apply(nil, []string{oldRel})
	}
	if k := kindOf(newRel); k != "" && k == kindOf(oldRel) {
		moved, err := ix.renameRow(oldRel, newRel)
		if err != nil || moved {
			return err
		}
	}
	if err := ix.apply(nil, []string{oldRel}); err != nil {
		return err
	}
	return ix.touch(newRel)
}

func (ix *Index) renameRow(oldRel, newRel string) (bool, error) {
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	tx, err := ix.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id int64
	if tx.QueryRow(`SELECT id FROM files WHERE path = ?`, oldRel).Scan(&id) != nil {
		return false, nil
	}
	if err := removeTx(tx, newRel); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE files SET path = ? WHERE id = ?`, newRel, id); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE chunks SET name = ? WHERE rowid IN (?, ?)`, nameOf(newRel), id<<pageBits, id<<pageBits|1); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// nameOf is the searchable file name. Images and recordings get none:
// attachment names are noise ("Pasted image 2024…", "Recording 2024…") and
// made them match words that are not in them.
func nameOf(rel string) string {
	if slow(kindOf(rel)) {
		return ""
	}
	base := path.Base(rel)
	return strings.TrimSuffix(base, path.Ext(base))
}

// store writes extracted pages, wiping the file's chunks first. The status
// only advances if the file hasn't changed since the job was claimed.
func (ix *Index) store(j job, pages []page, status, errMsg string) error {
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The file was deleted, changed, or the index was reset while this job
	// ran: its text is stale, and writing it would leave orphan chunks.
	if tx.QueryRow(`SELECT 1 FROM files WHERE id = ? AND mtime = ? AND size = ?`, j.id, j.mtime, j.size).Scan(new(int)) != nil {
		return nil
	}
	base := j.id << pageBits
	if err := deleteChunks(tx, base, base+1<<pageBits-1); err != nil {
		return err
	}
	for _, p := range pages {
		if p.n >= 1<<pageBits {
			break
		}
		name := ""
		if p.n <= 1 { // only the first chunk carries the file name, so a name match is one result, not one per page
			name = nameOf(j.rel)
		}
		rowid := base | int64(p.n)
		if _, err := tx.Exec(`INSERT INTO chunks(rowid, name, title, body, source) VALUES(?,?,?,?,?)`,
			rowid, name, p.title, p.body, p.source); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE files SET status = ?, error = ? WHERE id = ? AND mtime = ? AND size = ?`,
		status, errMsg, j.id, j.mtime, j.size); err != nil {
		return err
	}
	return tx.Commit()
}

type job struct {
	id          int64
	rel, kind   string
	mtime, size int64
}

// next claims the oldest unclaimed job in the given status; release it when
// done. Claiming (not sharding by id) lets any subset of workers drain the
// whole queue, e.g. just one of them while on battery.
func (ix *Index) next(status string) (job, bool) {
	ix.cmu.Lock()
	defer ix.cmu.Unlock()
	if ix.claimed == nil {
		ix.claimed = map[int64]bool{}
	}
	// Claimed jobs are still in this status, so look a little past them.
	rows, err := ix.db.Query(`SELECT id, path, kind, mtime, size FROM files
		WHERE status = ? ORDER BY id LIMIT ?`, status, len(ix.claimed)+1)
	if err != nil {
		return job{}, false
	}
	defer rows.Close()
	for rows.Next() {
		var j job
		if rows.Scan(&j.id, &j.rel, &j.kind, &j.mtime, &j.size) == nil && !ix.claimed[j.id] {
			ix.claimed[j.id] = true
			return j, true
		}
	}
	return job{}, false
}

func (ix *Index) release(id int64) {
	ix.cmu.Lock()
	delete(ix.claimed, id)
	ix.cmu.Unlock()
}

// reset deletes everything indexed, gives the disk space back, and starts a
// fresh scan so the index rebuilds from the vault in the background.
func (ix *Index) reset() error {
	ix.wmu.Lock()
	_, err := ix.db.Exec(`DELETE FROM chunks; DELETE FROM files; DELETE FROM meta;
		INSERT INTO meta VALUES ('media', ?)`, mediaConfig())
	if err == nil {
		_, err = ix.db.Exec(`VACUUM`)
	}
	if err == nil {
		_, err = ix.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	ix.wmu.Unlock()
	if err != nil {
		return err
	}
	return ix.scan(true)
}

func (ix *Index) status() (map[string]any, error) {
	counts := map[string]int{"pending": 0, "ocr": 0, "done": 0, "error": 0}
	rows, err := ix.db.Query(`SELECT status, count(*) FROM files GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		rows.Scan(&s, &n)
		counts[s] = n
	}
	return map[string]any{"counts": counts, "paused": ix.paused.Load(), "missing": missingTools()}, rows.Err()
}
