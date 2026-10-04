package main

import (
	"path/filepath"
	"sync"
)

// remoteBacklog counts the remote images still to download and OCR, for the
// status bar: a note with 20 Drive images is one file in the queue but 20
// images of work. Only images not in the URL cache count (cached ones cost
// nothing), and each URL counts once however many notes embed it.
type remoteBacklog struct {
	mu   sync.Mutex
	urls map[int64]map[string]bool // queued note's file id → its uncached image URLs
}

// set records a queued note's uncached remote images.
func (b *remoteBacklog) set(id int64, urls []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.urls == nil {
		b.urls = map[int64]map[string]bool{}
	}
	if len(urls) == 0 {
		delete(b.urls, id)
		return
	}
	set := make(map[string]bool, len(urls))
	for _, u := range urls {
		set[u] = true
	}
	b.urls[id] = set
}

// done marks one of a note's images as read (or given up on).
func (b *remoteBacklog) done(id int64, url string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.urls[id], url)
}

func (b *remoteBacklog) clear(id int64) { b.set(id, nil) }

// left counts the distinct URLs of the given notes. Notes no longer queued
// (deleted, or finished elsewhere) are dropped.
func (b *remoteBacklog) left(queued map[int64]bool) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]bool{}
	for id, set := range b.urls {
		if !queued[id] {
			delete(b.urls, id)
			continue
		}
		for u := range set {
			seen[u] = true
		}
	}
	return len(seen)
}

// uncachedRemote lists a note's remote image URLs (up to the per-note cap)
// that aren't in the URL cache yet.
func (ix *Index) uncachedRemote(text string) []string {
	urls := remoteImageURLs(text)
	if len(urls) > maxRemoteImages {
		urls = urls[:maxRemoteImages]
	}
	cached := ix.cachedRemote(urls)
	var missing []string
	for _, u := range urls {
		if _, ok := cached[u]; !ok {
			missing = append(missing, u)
		}
	}
	return missing
}

// countQueuedRemote fills the backlog for notes that were already in the
// slow lane when the server started (queued by an earlier run, or by an
// index upgrade); notes queued from now on are counted by the fast lane.
func (ix *Index) countQueuedRemote() {
	rows, err := ix.db.Query(`SELECT id, path FROM files WHERE status = 'ocr' AND kind = 'text'`)
	if err != nil {
		return
	}
	type note struct {
		id  int64
		rel string
	}
	var notes []note
	for rows.Next() {
		var n note
		if rows.Scan(&n.id, &n.rel) == nil {
			notes = append(notes, n)
		}
	}
	rows.Close()
	for _, n := range notes {
		abs := filepath.Join(ix.vault, filepath.FromSlash(n.rel))
		if !isMarkdown(abs) {
			continue
		}
		b, err := readCapped(abs)
		if err != nil {
			continue
		}
		ix.backlog.mu.Lock()
		_, known := ix.backlog.urls[n.id] // a worker may have got there first
		ix.backlog.mu.Unlock()
		if !known {
			ix.backlog.set(n.id, ix.uncachedRemote(string(b)))
		}
	}
}

// remoteLeft is the number of remote images waiting, for /status.
func (ix *Index) remoteLeft() int {
	queued := map[int64]bool{}
	rows, err := ix.db.Query(`SELECT id FROM files WHERE status = 'ocr' AND kind = 'text'`)
	if err != nil {
		return 0
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			queued[id] = true
		}
	}
	rows.Close()
	return ix.backlog.left(queued)
}
