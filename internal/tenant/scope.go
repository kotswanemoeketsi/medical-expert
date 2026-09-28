// Package tenant provides the only database access path available to
// repositories, and makes tenant isolation a property of the type system
// rather than of developer discipline.
//
// BRS BR-0.3: "isolation shall not depend on application-layer filtering
// alone." Three layers enforce that:
//
//  1. Repositories are constructed with a *Scope, never with *sql.DB. The
//     underlying handle is unexported and has no accessor, so a repository
//     cannot issue an unscoped query even by accident.
//  2. Every statement must name the tenant explicitly with the Marker below.
//     A statement without one is refused before it reaches the driver.
//  3. Scope supplies the tenant value itself, so a caller cannot pass the
//     wrong one.
//
// Layer 2 is a guard, not a parser. It catches the realistic mistake — a
// forgotten predicate — not a deliberate bypass. Deliberate bypass is
// prevented by layer 1 plus a test that greps for raw handles.
package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/yourorg/medical-expert-system/internal/model"
)

// ErrUnscopedQuery is returned when a statement would reach the database
// without naming a tenant. This is a programming error, not a user error.
var ErrUnscopedQuery = errors.New("tenant: statement does not name a tenant")

// ID is a tenant identifier. Aliased to model.ID so a tenant reference can
// pass between packages without conversion.
type ID = model.ID

// Marker is the placeholder callers write wherever the tenant value belongs.
//
// An explicit marker rather than positional injection, because positional
// injection breaks the moment a statement has a parameter before its WHERE
// clause — "UPDATE patients SET mobile = ? WHERE tenant_id = ?" being the
// obvious case. The marker also makes an unscoped statement greppable.
const Marker = ":tenant"

// Scope is a database handle bound to exactly one tenant.
//
// The db field is unexported with no getter. That is deliberate: it is the
// only thing standing between a repository and a cross-tenant read.
type Scope struct {
	db       queryable
	tenantID ID

	// breakGlass records that this scope was created by a platform operator
	// acting outside the normal tenant relationship (BR-0.5).
	breakGlass *BreakGlass
}

// BreakGlass captures operator access to tenant data.
type BreakGlass struct {
	OperatorID model.ID
	Reason     string
	TicketRef  string
}

// queryable is satisfied by both *sql.DB and *sql.Tx, so a Scope works the
// same inside and outside a transaction.
type queryable interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func NewScope(db *sql.DB, tenantID ID) (*Scope, error) {
	if tenantID.IsZero() {
		return nil, errors.New("tenant: refusing to create a scope with a zero tenant ID")
	}
	return &Scope{db: db, tenantID: tenantID}, nil
}

// NewBreakGlassScope binds a handle to a tenant on behalf of a platform
// operator. Callers must already have written an audit entry recording the
// justification; this constructor only carries it forward.
func NewBreakGlassScope(db *sql.DB, tenantID ID, bg BreakGlass) (*Scope, error) {
	if strings.TrimSpace(bg.Reason) == "" {
		return nil, errors.New("tenant: break-glass access requires a recorded reason")
	}
	s, err := NewScope(db, tenantID)
	if err != nil {
		return nil, err
	}
	s.breakGlass = &bg
	return s, nil
}

func (s *Scope) TenantID() ID { return s.tenantID }

func (s *Scope) BreakGlass() *BreakGlass { return s.breakGlass }

// WithTx returns a Scope bound to the same tenant but running inside tx.
func (s *Scope) WithTx(tx *sql.Tx) *Scope {
	return &Scope{db: tx, tenantID: s.tenantID, breakGlass: s.breakGlass}
}

// InTx runs fn inside a transaction bound to the same tenant. If the scope is
// already inside a transaction, fn joins it rather than starting a nested one,
// so repository methods can use InTx without knowing whether their caller did.
//
// This exists so that repositories never need the raw handle to begin a
// transaction: the handle stays unexported and InTx is the only way in.
func (s *Scope) InTx(ctx context.Context, fn func(tx *Scope) error) (err error) {
	if _, inTx := s.db.(*sql.Tx); inTx {
		return fn(s)
	}
	root, ok := s.db.(*sql.DB)
	if !ok || root == nil {
		return errors.New("tenant: scope has no database to begin a transaction on")
	}
	tx, err := root.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("tenant: begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(s.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("tenant: commit: %w", err)
	}
	committed = true
	return nil
}

// Query runs a scoped query.
//
//	rows, err := scope.Query(ctx,
//	    `SELECT id FROM patients WHERE tenant_id = :tenant AND status = ?`,
//	    "active")
func (s *Scope) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q, a, err := s.bind(query, args...)
	if err != nil {
		return nil, err
	}
	return s.db.QueryContext(ctx, q, a...)
}

// QueryRow runs a scoped single-row query.
func (s *Scope) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	q, a, err := s.bind(query, args...)
	if err != nil {
		// *sql.Row carries no synthetic error, so force the failure through
		// the driver. Ugly, but it fails closed rather than returning rows.
		return s.db.QueryRowContext(ctx, "SELECT /* unscoped statement refused */ FAIL")
	}
	return s.db.QueryRowContext(ctx, q, a...)
}

// Exec runs a scoped statement.
func (s *Scope) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	q, a, err := s.bind(query, args...)
	if err != nil {
		return nil, err
	}
	return s.db.ExecContext(ctx, q, a...)
}

// bind validates the statement, replaces every Marker with a bind parameter,
// and interleaves the tenant value into the argument list at the right
// position. Multiple markers are supported, which matters for statements with
// a correlated subquery.
func (s *Scope) bind(query string, args ...any) (string, []any, error) {
	if err := Guard(query); err != nil {
		return "", nil, err
	}
	q := stripComments(query)

	var b strings.Builder
	b.Grow(len(q))
	out := make([]any, 0, len(args)+2)
	consumed := 0

	for i := 0; i < len(q); {
		switch {
		case q[i] == '\'' || q[i] == '"':
			// Copy a quoted literal verbatim so a '?' or ':tenant' inside it is
			// never mistaken for a placeholder. A doubled quote is an escape.
			quote := q[i]
			b.WriteByte(q[i])
			i++
			for i < len(q) {
				b.WriteByte(q[i])
				if q[i] == quote {
					if i+1 < len(q) && q[i+1] == quote {
						b.WriteByte(q[i+1])
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case q[i] == '?':
			if consumed >= len(args) {
				return "", nil, fmt.Errorf(
					"tenant: statement has more placeholders than arguments (%d given): %.80s",
					len(args), query)
			}
			out = append(out, args[consumed])
			consumed++
			b.WriteByte('?')
			i++
		case strings.HasPrefix(q[i:], Marker):
			out = append(out, s.tenantID.Bytes())
			b.WriteByte('?')
			i += len(Marker)
		default:
			b.WriteByte(q[i])
			i++
		}
	}

	if consumed != len(args) {
		return "", nil, fmt.Errorf(
			"tenant: %d arguments given but statement has %d placeholders: %.80s",
			len(args), consumed, query)
	}
	return b.String(), out, nil
}

var guardCache sync.Map // string -> error

// barePlaceholder matches "tenant_id = ?" however it is spaced or qualified,
// which is how a caller-supplied tenant value would sneak back in.
var barePlaceholder = regexp.MustCompile(`(?i)tenant_id\s*=\s*\?`)

// Guard reports whether a statement is safely tenant-scoped. Exported so the
// repository package's tests can assert every statement it ships passes.
func Guard(query string) error {
	if cached, ok := guardCache.Load(query); ok {
		if cached == nil {
			return nil
		}
		return cached.(error)
	}
	err := guard(query)
	if err == nil {
		guardCache.Store(query, nil)
	} else {
		guardCache.Store(query, err)
	}
	return err
}

func guard(query string) error {
	stripped := stripComments(query)
	if !strings.Contains(stripped, Marker) {
		return fmt.Errorf("%w: expected %q somewhere in: %.80s",
			ErrUnscopedQuery, Marker, strings.TrimSpace(query))
	}
	if barePlaceholder.MatchString(stripped) {
		return fmt.Errorf(
			"tenant: write \"tenant_id = %s\" rather than a bare placeholder so the "+
				"scope supplies the value: %.80s", Marker, strings.TrimSpace(query))
	}
	return nil
}

func stripComments(q string) string {
	var b strings.Builder
	for i := 0; i < len(q); i++ {
		if i+1 < len(q) && q[i] == '-' && q[i+1] == '-' {
			for i < len(q) && q[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
			continue
		}
		if i+1 < len(q) && q[i] == '/' && q[i+1] == '*' {
			i += 2
			for i+1 < len(q) && !(q[i] == '*' && q[i+1] == '/') {
				i++
			}
			i++
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}
