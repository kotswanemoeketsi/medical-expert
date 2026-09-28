package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSplitStatements(t *testing.T) {
	script := `-- header comment; with a semicolon
CREATE TABLE a (x INT); -- trailing
/* block; comment */
INSERT INTO a VALUES (';'), ("it's; fine");
-- only a comment at the end;
`
	got := SplitStatements(script)
	if len(got) != 2 {
		t.Fatalf("expected 2 statements, got %d: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "CREATE TABLE a") {
		t.Errorf("first statement: %q", got[0])
	}
	if !strings.Contains(got[1], `("it's; fine")`) {
		t.Errorf("quoted semicolon split the statement: %q", got[1])
	}
}

func TestEmbeddedMigrationsParse(t *testing.T) {
	ms, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("no embedded migrations")
	}
	for i, m := range ms {
		if i > 0 && ms[i-1].Name >= m.Name {
			t.Errorf("migrations out of order: %s then %s", ms[i-1].Name, m.Name)
		}
		if len(m.Checksum) != 64 {
			t.Errorf("%s: bad checksum %q", m.Name, m.Checksum)
		}
		if len(SplitStatements(m.SQL)) == 0 {
			t.Errorf("%s contains no statements", m.Name)
		}
	}
}

// The repository keeps a readable copy of the migrations at db/migrations.
// If the two drift, someone applying the readable copy by hand gets a schema
// the binary does not expect.
func TestMigrationCopiesMatch(t *testing.T) {
	ms, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	copyDir := filepath.Join("..", "..", "db", "migrations")
	entries, err := os.ReadDir(copyDir)
	if err != nil {
		t.Fatalf("readable copy missing: %v", err)
	}
	if len(entries) != len(ms) {
		t.Fatalf("db/migrations has %d files, embedded set has %d", len(entries), len(ms))
	}
	for _, m := range ms {
		b, err := os.ReadFile(filepath.Join(copyDir, m.Name))
		if err != nil {
			t.Errorf("%s missing from db/migrations", m.Name)
			continue
		}
		if string(normaliseLineEndings(b)) != string(normaliseLineEndings([]byte(m.SQL))) {
			t.Errorf("%s differs between internal/db/migrations and db/migrations", m.Name)
		}
	}
}

func TestChecksumIgnoresLineEndings(t *testing.T) {
	a := normaliseLineEndings([]byte("CREATE TABLE x (\r\n  id INT\r\n);\r\n"))
	b := normaliseLineEndings([]byte("CREATE TABLE x (\n  id INT\n);\n"))
	if string(a) != string(b) {
		t.Fatal("CRLF and LF checkouts would produce different checksums")
	}
}

func TestPrepareDSNForcesUTCAndParseTime(t *testing.T) {
	cfg, err := PrepareDSN("mes:secret@tcp(127.0.0.1:3306)/mes")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ParseTime || cfg.Loc != time.UTC || cfg.Params["time_zone"] != "'+00:00'" {
		t.Fatalf("DSN not prepared: parseTime=%v loc=%v params=%v", cfg.ParseTime, cfg.Loc, cfg.Params)
	}
	if _, err := PrepareDSN(""); err == nil {
		t.Fatal("empty DSN accepted")
	}
}
