// Package repository is the data access layer: one type per aggregate.
//
// Every type here is constructed from a *tenant.Scope and reaches the
// database only through it, so every statement names the tenant and the
// scope supplies the value. Two files are the documented exceptions — see
// unscopedByDesign in scoped_test.go — and hold a raw *sql.DB because their
// job is to work out which tenant a request belongs to, or to record events
// that have no tenant.
//
// Dynamic SQL is banned. Every statement is a string constant, so the tests
// can find each one and run it through tenant.Guard. A safe query that
// cannot be verified eventually becomes an unsafe one nobody notices.
package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/yourorg/medical-expert-system/internal/model"
)

var (
	// ErrNotFound means no row matched in this tenant. A row belonging to
	// another tenant is, by construction, indistinguishable from none.
	ErrNotFound = errors.New("repository: not found")
	// ErrDuplicate means a unique key rejected the row. errors.As with
	// *DuplicateKeyError to learn which key.
	ErrDuplicate = errors.New("repository: duplicate")
	// ErrConflict means a conditional update matched nothing: the row exists
	// but is not in the state the statement required (already used, already
	// enabled, already signed).
	ErrConflict = errors.New("repository: row is not in the expected state")
	// ErrReference means a foreign key rejected the row: the parent does not
	// exist in this tenant.
	ErrReference = errors.New("repository: referenced row does not exist")
)

// DuplicateKeyError names the unique index that rejected a row.
type DuplicateKeyError struct{ Key string }

func (e *DuplicateKeyError) Error() string        { return "repository: duplicate value for " + e.Key }
func (e *DuplicateKeyError) Is(target error) bool { return target == ErrDuplicate }

// translate turns driver errors into the repository vocabulary.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1062: // ER_DUP_ENTRY
			return &DuplicateKeyError{Key: duplicateKeyName(me.Message)}
		case 1452: // ER_NO_REFERENCED_ROW_2
			return fmt.Errorf("%w: %s", ErrReference, me.Message)
		}
	}
	return err
}

// duplicateKeyName extracts the index name from "Duplicate entry 'x' for key
// 'uq_users_email'" (MariaDB) or "... for key 'users.uq_users_email'" (MySQL 8).
func duplicateKeyName(msg string) string {
	i := strings.LastIndex(msg, "for key '")
	if i < 0 {
		return ""
	}
	key := strings.TrimSuffix(msg[i+len("for key '"):], "'")
	if dot := strings.LastIndexByte(key, '.'); dot >= 0 {
		key = key[dot+1:]
	}
	return key
}

// expectOne turns an Exec result into ErrConflict when nothing matched. The
// DSN sets clientFoundRows, so "affected" means matched, not changed.
func expectOne(res sql.Result, err error) error {
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

// expectFound is expectOne for statements whose only condition is identity,
// where matching nothing means the row does not exist.
func expectFound(res sql.Result, err error) error {
	if err := expectOne(res, err); err != nil {
		if errors.Is(err, ErrConflict) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

// nullString stores an empty string as NULL, so "no mobile number" is one
// value in the database rather than two.
func nullString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullID(id *model.ID) any {
	if id == nil || id.IsZero() {
		return nil
	}
	return id.Bytes()
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func nullInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

func nullDate(d *model.Date) any {
	if d == nil || d.IsZero() {
		return nil
	}
	return d.String()
}

func nullTimeOfDay(t *model.TimeOfDay) any {
	if t == nil {
		return nil
	}
	return t.String() + ":00"
}

func idPtr(n sql.Null[model.ID]) *model.ID {
	if !n.Valid {
		return nil
	}
	id := n.V
	return &id
}

func timePtr(n sql.Null[time.Time]) *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.V.UTC()
	return &t
}

// likePrefix escapes LIKE metacharacters and appends a wildcard, so a search
// for "O'Neil_" matches that literal prefix and nothing else.
func likePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s) + "%"
}
