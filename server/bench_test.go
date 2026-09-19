package main

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// Query latency on a real index. Skipped unless pointed at one:
//
//	BENCH_DB=/path/to/copy-of-index.db BENCH_VAULT=/path/to/vault go test -tags sqlite_fts5 -v -run TestBenchReal .
//
// Use a copy: opening an index can upgrade its schema.
func TestBenchReal(t *testing.T) {
	db := os.Getenv("BENCH_DB")
	if db == "" {
		t.Skip()
	}
	ix, err := openIndex(db, os.Getenv("BENCH_VAULT"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"t", "th", "the", "a", "process", "scheduling algorithm", "fcfs in:image", "the path:Operating", "schedulng algoritm", "zzzzqqqq", `"in the"`, "the -process"} {
		var ds []time.Duration
		n := 0
		for range 30 {
			st := time.Now()
			res, err := ix.search(q, nil, 50)
			if err != nil {
				t.Fatal(q, err)
			}
			ds = append(ds, time.Since(st))
			n = len(res.Results)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		fmt.Printf("%-24q %3d results  median %7.2f ms  worst %7.2f ms\n", q, n, float64(ds[15].Microseconds())/1000, float64(ds[29].Microseconds())/1000)
	}
}
