// Command migrate applies the embedded database migrations.
//
//	go run ./cmd/migrate           apply everything pending
//	go run ./cmd/migrate -check    exit 2 if anything is pending (for deploy gates)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/yourorg/medical-expert-system/internal/config"
	"github.com/yourorg/medical-expert-system/internal/db"
)

func main() {
	check := flag.Bool("check", false, "report pending migrations and exit 2 if there are any, without applying them")
	dsn := flag.String("dsn", "", "database DSN (default: MES_DSN)")
	flag.Parse()

	if err := config.LoadDotEnv(config.EnvFile()); err != nil {
		fail(err)
	}
	if *dsn == "" {
		*dsn = os.Getenv("MES_DSN")
	}
	ctx := context.Background()
	conn, err := db.Open(ctx, *dsn, db.Options{MaxOpen: 2})
	if err != nil {
		fail(err)
	}
	defer conn.Close()

	if *check {
		pending, err := db.Pending(ctx, conn)
		if err != nil {
			fail(err)
		}
		if len(pending) > 0 {
			fmt.Printf("%d pending: %s\n", len(pending), strings.Join(pending, ", "))
			os.Exit(2)
		}
		fmt.Println("schema is up to date")
		return
	}

	applied, err := db.Migrate(ctx, conn, func(f string, a ...any) { fmt.Printf(f+"\n", a...) })
	if err != nil {
		fail(err)
	}
	if len(applied) == 0 {
		fmt.Println("schema is up to date; nothing to apply")
		return
	}
	fmt.Printf("applied %d migration(s)\n", len(applied))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "migrate:", err)
	os.Exit(1)
}
