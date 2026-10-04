//go:build cgo && !purego

package main

// The C library through cgo: measured 2.3× faster than the pure-Go port on
// every query, and a smaller binary. Needs a C compiler and the sqlite_fts5
// build tag. Builds without cgo (Windows without gcc) use sqlite_purego.go.
import _ "github.com/mattn/go-sqlite3"

const sqlDriver = "sqlite3"

func sqlDSN(path string) string {
	return "file:" + uriPath(path) + "?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000"
}
