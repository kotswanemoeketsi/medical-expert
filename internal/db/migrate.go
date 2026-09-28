// Package db opens the database and applies the embedded migrations.
//
// Migrations live inside the binary so the schema can never be out of step
// with the code that expects it. The same files are copied to db/migrations/
// at the repository root for people who want to read or apply them by hand;
// TestMigrationCopiesMatch keeps the two identical.
package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one embedded SQL file.
type Migration struct {
	Name     string
	SQL      string
	Checksum string
}

// Migrations returns every embedded migration in filename order.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(normaliseLineEndings(b))
		out = append(out, Migration{Name: e.Name(), SQL: string(b), Checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// normaliseLineEndings makes the checksum independent of how git checked the
// file out: a Windows clone with CRLF must not look like a modified migration.
func normaliseLineEndings(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), "\r\n", "\n"))
}

const createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version     VARCHAR(255) NOT NULL,
  checksum    CHAR(64)     NOT NULL,
  applied_at  DATETIME(3)  NOT NULL,
  PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

// ErrModifiedMigration means a migration that was already applied has since
// been edited. Applied migrations are history; changes go in a new file.
var ErrModifiedMigration = errors.New("db: an applied migration has been modified")

// Pending lists migrations not yet applied, and fails if an applied one has
// been modified since.
func Pending(ctx context.Context, db *sql.DB) ([]string, error) {
	all, err := Migrations()
	if err != nil {
		return nil, err
	}
	applied, err := appliedChecksums(ctx, db)
	if err != nil {
		return nil, err
	}
	var pending []string
	for _, m := range all {
		sum, ok := applied[m.Name]
		if !ok {
			pending = append(pending, m.Name)
			continue
		}
		if sum != m.Checksum {
			return nil, fmt.Errorf("%w: %s", ErrModifiedMigration, m.Name)
		}
	}
	return pending, nil
}

func appliedChecksums(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1146 { // table doesn't exist: nothing applied
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var v, c string
		if err := rows.Scan(&v, &c); err != nil {
			return nil, err
		}
		out[v] = c
	}
	return out, rows.Err()
}

// Migrate applies every pending migration in order and returns their names.
//
// A named lock stops two instances starting at once from racing each other.
// MySQL commits DDL implicitly, so a migration that fails part-way leaves
// its earlier statements applied; the error names the statement so it can be
// finished or reverted by hand.
func Migrate(ctx context.Context, db *sql.DB, logf func(format string, args ...any)) ([]string, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK('mes_schema_migrations', 60)`).Scan(&got); err != nil {
		return nil, fmt.Errorf("db: migration lock: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		return nil, errors.New("db: another process is applying migrations; timed out waiting for it")
	}
	defer conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK('mes_schema_migrations')`) //nolint:errcheck

	if _, err := conn.ExecContext(ctx, createMigrationsTable); err != nil {
		return nil, fmt.Errorf("db: create schema_migrations: %w", err)
	}
	all, err := Migrations()
	if err != nil {
		return nil, err
	}
	applied, err := appliedChecksums(ctx, conn)
	if err != nil {
		return nil, err
	}

	var done []string
	for _, m := range all {
		if sum, ok := applied[m.Name]; ok {
			if sum != m.Checksum {
				return done, fmt.Errorf("%w: %s", ErrModifiedMigration, m.Name)
			}
			continue
		}
		logf("applying migration %s", m.Name)
		for i, stmt := range SplitStatements(m.SQL) {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return done, fmt.Errorf("db: %s statement %d failed: %w\n%s", m.Name, i+1, err, firstLine(stmt))
			}
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, checksum, applied_at) VALUES (?, ?, ?)`,
			m.Name, m.Checksum, time.Now().UTC()); err != nil {
			return done, fmt.Errorf("db: record %s: %w", m.Name, err)
		}
		done = append(done, m.Name)
	}
	return done, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

// SplitStatements splits a SQL script on semicolons that end a statement,
// ignoring semicolons inside quotes and comments. Comment-only fragments are
// dropped. The scripts contain no stored procedures, so no DELIMITER support.
func SplitStatements(script string) []string {
	var (
		out     []string
		b       strings.Builder
		inQuote byte
	)
	flush := func() {
		stmt := strings.TrimSpace(b.String())
		b.Reset()
		if stripSQLComments(stmt) != "" {
			out = append(out, stmt)
		}
	}
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case inQuote != 0:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(script) {
				i++
				b.WriteByte(script[i])
			} else if c == inQuote {
				inQuote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			inQuote = c
			b.WriteByte(c)
		case c == '-' && i+1 < len(script) && script[i+1] == '-':
			for i < len(script) && script[i] != '\n' {
				b.WriteByte(script[i])
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(script) && script[i+1] == '*':
			end := strings.Index(script[i+2:], "*/")
			if end < 0 {
				b.WriteString(script[i:])
				i = len(script)
			} else {
				b.WriteString(script[i : i+2+end+2])
				i += 2 + end + 1
			}
		case c == ';':
			flush()
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return out
}

func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	out := b.String()
	for {
		start := strings.Index(out, "/*")
		if start < 0 {
			break
		}
		end := strings.Index(out[start+2:], "*/")
		if end < 0 {
			out = out[:start]
			break
		}
		out = out[:start] + out[start+2+end+2:]
	}
	return strings.TrimSpace(out)
}
