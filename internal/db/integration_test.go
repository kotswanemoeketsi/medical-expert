//go:build integration

package db

import (
	"context"
	"os"
	"testing"
)

func TestMigrateIsIdempotentAgainstARealDatabase(t *testing.T) {
	dsn := os.Getenv("MES_TEST_DSN")
	if dsn == "" {
		t.Skip("MES_TEST_DSN not set")
	}
	ctx := context.Background()
	conn, err := Open(ctx, dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := Migrate(ctx, conn, t.Logf); err != nil {
		t.Fatalf("first run: %v", err)
	}
	again, err := Migrate(ctx, conn, t.Logf)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second run applied %v; migrations must be recorded once applied", again)
	}
	pending, err := Pending(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after migrating: %v", pending)
	}
}
