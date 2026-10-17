// Command pgq runs a read-only SQL query and prints rows as JSON.
// Development helper for the portable PostgreSQL setup (no psql client in
// the embedded binary distribution).
//
// Usage:
//
//	go run ./scripts/pgq -dsn "postgres://bargainproxy@127.0.0.1:55432/bargainproxy" -sql "SELECT id, channel FROM accounts"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	dsn := flag.String("dsn", "", "database dsn")
	sql := flag.String("sql", "", "read-only sql")
	flag.Parse()
	if *dsn == "" || *sql == "" {
		fmt.Fprintln(os.Stderr, "usage: pgq -dsn <dsn> -sql <query>")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, *sql)
	if err != nil {
		fmt.Fprintln(os.Stderr, "query:", err)
		os.Exit(1)
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	var out []map[string]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			fmt.Fprintln(os.Stderr, "scan:", err)
			os.Exit(1)
		}
		row := map[string]any{}
		for i, f := range fields {
			v := vals[i]
			// Truncate long strings for readable evidence dumps.
			if s, ok := v.(string); ok && len(s) > 160 {
				v = s[:160] + "…(truncated)"
			}
			row[string(f.Name)] = v
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "rows:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
