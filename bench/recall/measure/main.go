package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"claude-mem-go/embed"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pgvector/pgvector-go"
)

// Queries phrased differently from how the corpus is worded, so this
// measures semantic retrieval rather than string overlap.
var queries = []string{
	"why did the connection pool stop handing out slots",
	"something about tokens being cached",
	"the index was not being used by the planner",
	"work was lost when the process shut down",
	"paths stored in the wrong form",
	"how retries are bounded",
	"which database the background process writes to",
	"summarising what happened in a session",
	"telling a health probe apart from real data",
	"reading files that were touched before",
}

var efSearch int

func topK(db *sql.DB, vec pgvector.Vector, k int, forceIndex bool) ([]int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// enable_seqscan off => the HNSW index is the only path (approximate).
	// on, with indexscan off => a full exact scan (ground truth).
	if forceIndex {
		_, err = tx.Exec("SET LOCAL enable_seqscan = off")
		if err == nil && efSearch > 0 {
			_, err = tx.Exec(fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch))
		}
	} else {
		_, err = tx.Exec("SET LOCAL enable_indexscan = off; SET LOCAL enable_bitmapscan = off")
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT id FROM observations ORDER BY embedding <=> $1 LIMIT $2`, vec, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func overlap(a, b []int64) int {
	m := map[int64]bool{}
	for _, x := range a {
		m[x] = true
	}
	n := 0
	for _, x := range b {
		if m[x] {
			n++
		}
	}
	return n
}

func main() {
	db, err := sql.Open("pgx", os.Args[1])
	if err != nil {
		panic(err)
	}
	defer db.Close()
	cl := embed.NewClient("nomic-embed-text")

	const k = 10
	fmt.Printf("  %-14s %-10s %s\n", "ef_search", "recall@10", "note")
	for _, ef := range []int{20, 40, 100, 200, 400} {
		efSearch = ef
		total, hits := 0, 0
		for _, q := range queries {
			v, err := cl.Embed(q)
			if err != nil {
				panic(err)
			}
			vec := pgvector.NewVector(v)
			exact, err := topK(db, vec, k, false)
			if err != nil {
				panic(err)
			}
			ann, err := topK(db, vec, k, true)
			if err != nil {
				panic(err)
			}
			o := overlap(exact, ann)
			total += len(exact)
			hits += o
		}
		note := ""
		if ef == 40 {
			note = "<- pgvector default"
		}
		fmt.Printf("  %-14d %-10.1f %s\n", ef, 100*float64(hits)/float64(total), note)
	}
	_ = strings.TrimSpace
}
