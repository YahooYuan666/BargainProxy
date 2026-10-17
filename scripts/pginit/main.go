// Command pginit creates the application database if it does not exist.
// Development helper for the portable PostgreSQL setup (the zonky binary
// distribution ships no client tools such as createdb).
//
// Usage:
//
//	go run ./scripts/pginit -dsn "postgres://bargainproxy@127.0.0.1:55432/postgres" -db bargainproxy
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	dsn := flag.String("dsn", "", "admin dsn (connect to the maintenance database)")
	db := flag.String("db", "bargainproxy", "database name to create")
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "usage: pginit -dsn <admin dsn> -db <name>")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, *db).Scan(&exists); err != nil {
		fmt.Fprintln(os.Stderr, "check:", err)
		os.Exit(1)
	}
	if exists {
		fmt.Printf("database %q already exists\n", *db)
		return
	}
	// Identifiers cannot be parameterized; the name comes from the operator's
	// own CLI flag, not from untrusted input, and is quoted here.
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, *db)); err != nil {
		fmt.Fprintln(os.Stderr, "create:", err)
		os.Exit(1)
	}
	fmt.Printf("database %q created\n", *db)
}
