package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Options tune the connection pool.
type Options struct {
	MaxOpen     int
	MaxIdle     int
	ConnMaxLife time.Duration
	ConnMaxIdle time.Duration
}

// PrepareDSN parses a DSN and applies the settings the application depends
// on, so a DSN copied from a tutorial still works:
//
//   - parseTime=true, so DATETIME columns scan into time.Time
//   - loc=UTC, and the session time zone set to UTC, so every timestamp the
//     application writes and reads is UTC regardless of server configuration
//   - utf8mb4, so names with any character survive
func PrepareDSN(dsn string) (*mysql.Config, error) {
	if dsn == "" {
		return nil, errors.New("db: no DSN configured (set MES_DSN)")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: invalid DSN: %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Collation = "utf8mb4_unicode_ci"
	cfg.MultiStatements = false
	// Report rows matched rather than rows changed, so an UPDATE that sets a
	// value to what it already was still counts as having found its row.
	// Conditional updates ("... AND used_at IS NULL") depend on this to tell
	// "not in the expected state" apart from "nothing to change".
	cfg.ClientFoundRows = true
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+00:00'"
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	return cfg, nil
}

// Open connects and pings, failing fast with a readable error.
func Open(ctx context.Context, dsn string, opt Options) (*sql.DB, error) {
	cfg, err := PrepareDSN(dsn)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	db := sql.OpenDB(connector)
	if opt.MaxOpen > 0 {
		db.SetMaxOpenConns(opt.MaxOpen)
	}
	if opt.MaxIdle > 0 {
		db.SetMaxIdleConns(opt.MaxIdle)
	}
	if opt.ConnMaxLife > 0 {
		db.SetConnMaxLifetime(opt.ConnMaxLife)
	}
	if opt.ConnMaxIdle > 0 {
		// Below typical server and NAT idle timeouts, so a pooled connection
		// is recycled before something in the middle silently drops it.
		db.SetConnMaxIdleTime(opt.ConnMaxIdle)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("db: cannot reach the database at %s as %q: %w", cfg.Addr, cfg.User, err)
	}
	return db, nil
}
