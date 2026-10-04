//go:build !cgo || purego

package main

// Pure Go SQLite (with FTS5): no C compiler needed, so plain `go build`
// works on Windows and minimal Linux installs. About 2× slower per query than
// the cgo driver, which is still a few ms on a big vault.
import _ "modernc.org/sqlite"

const sqlDriver = "sqlite"

func sqlDSN(path string) string {
	return "file:" + uriPath(path) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
}
