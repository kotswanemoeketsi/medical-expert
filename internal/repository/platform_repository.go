package repository

// UNSCOPED BY DESIGN. This file holds a raw *sql.DB because its job comes
// before a tenant scope can exist: finding the practice a login names, and
// resolving a bearer token to the tenant it belongs to. It returns
// identifiers and practice details, never patient data.
//
// Statements here run on the raw handle with no binder, so the ":tenant"
// marker must never appear in this file (TestUnscopedFilesNeverUseTheScopeMarker).

import (
	"context"
	"database/sql"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
)

// PlatformRepository administers tenants and resolves sessions.
type PlatformRepository struct {
	db *sql.DB
}

func NewPlatformRepository(db *sql.DB) *PlatformRepository { return &PlatformRepository{db: db} }

// Ping reports whether the database answers, for the health check.
func (r *PlatformRepository) Ping(ctx context.Context) error { return r.db.PingContext(ctx) }

// BeginTx starts a transaction for provisioning, which creates a tenant and
// its first administrator together or not at all.
func (r *PlatformRepository) BeginTx(ctx context.Context) (*sql.Tx, error) {
	return r.db.BeginTx(ctx, nil)
}

const insertTenant = `
INSERT INTO tenants (id, slug, name, status, timezone, practice_number, address, phone, email, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// CreateTenant inserts a practice inside tx.
func (r *PlatformRepository) CreateTenant(ctx context.Context, tx *sql.Tx, t *model.Tenant) error {
	_, err := tx.ExecContext(ctx, insertTenant,
		t.ID.Bytes(), t.Slug, t.Name, string(t.Status), t.Timezone,
		nullString(t.PracticeNumber), nullString(t.Address), nullString(t.Phone), nullString(t.Email),
		t.CreatedAt, t.UpdatedAt)
	return translate(err)
}

const tenantColumns = `id, slug, name, status, timezone, practice_number, address, phone, email, created_at, updated_at`

func scanTenant(sc scanner) (*model.Tenant, error) {
	var t model.Tenant
	var practiceNo, address, phone, email sql.Null[string]
	if err := sc.Scan(&t.ID, &t.Slug, &t.Name, &t.Status, &t.Timezone,
		&practiceNo, &address, &phone, &email, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, translate(err)
	}
	t.PracticeNumber, t.Address, t.Phone, t.Email = practiceNo.V, address.V, phone.V, email.V
	return &t, nil
}

const selectTenantBySlug = `SELECT ` + tenantColumns + ` FROM tenants WHERE slug = ?`

func (r *PlatformRepository) TenantBySlug(ctx context.Context, slug string) (*model.Tenant, error) {
	return scanTenant(r.db.QueryRowContext(ctx, selectTenantBySlug, slug))
}

const selectTenantByID = `SELECT ` + tenantColumns + ` FROM tenants WHERE id = ?`

func (r *PlatformRepository) TenantByID(ctx context.Context, id model.ID) (*model.Tenant, error) {
	return scanTenant(r.db.QueryRowContext(ctx, selectTenantByID, id.Bytes()))
}

const updateTenantStatus = `UPDATE tenants SET status = ?, updated_at = ? WHERE id = ?`

// SetTenantStatus suspends, reactivates or begins offboarding a practice
// (BR-0.4). A suspended practice's sessions stop resolving immediately.
func (r *PlatformRepository) SetTenantStatus(ctx context.Context, id model.ID, status model.TenantStatus, now time.Time) error {
	return expectFound(r.db.ExecContext(ctx, updateTenantStatus, string(status), now.UTC(), id.Bytes()))
}

const selectActiveTenantIDs = `SELECT id FROM tenants WHERE status = 'active' ORDER BY id`

// ActiveTenantIDs lists practices for per-tenant maintenance jobs.
func (r *PlatformRepository) ActiveTenantIDs(ctx context.Context) ([]model.ID, error) {
	rows, err := r.db.QueryContext(ctx, selectActiveTenantIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []model.ID
	for rows.Next() {
		var id model.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ResolvedSession is what a bearer token resolves to: enough to build a
// tenant scope and an actor, and nothing more.
type ResolvedSession struct {
	TenantID       model.ID
	SessionID      model.ID
	UserID         model.ID
	Role           model.Role
	PatientID      *model.ID
	MFAPending     bool
	MFAEnabled     bool
	LastSeenAt     time.Time
	AbsoluteExpiry time.Time
}

// The idle and absolute timeouts, revocation, and the status of the user and
// the practice are all checked in the query, so an expired or revoked
// session is never returned to the caller at all.
const selectSessionByToken = `
SELECT s.tenant_id, s.id, s.user_id, u.role, u.patient_id, s.mfa_pending, u.mfa_enabled,
       s.last_seen_at, s.absolute_expiry
  FROM sessions s
  JOIN users u   ON u.tenant_id = s.tenant_id AND u.id = s.user_id
  JOIN tenants t ON t.id = s.tenant_id
 WHERE s.token_hash = ?
   AND s.revoked_at IS NULL
   AND s.absolute_expiry > ?
   AND s.last_seen_at > ?
   AND u.status = 'active'
   AND t.status = 'active'`

// ResolveToken finds the live session for a token digest. idleCutoff is the
// earliest last_seen_at that still counts as active.
func (r *PlatformRepository) ResolveToken(ctx context.Context, digest [32]byte, now, idleCutoff time.Time) (*ResolvedSession, error) {
	var rs ResolvedSession
	var patientID sql.Null[model.ID]
	err := r.db.QueryRowContext(ctx, selectSessionByToken, digest[:], now.UTC(), idleCutoff.UTC()).Scan(
		&rs.TenantID, &rs.SessionID, &rs.UserID, &rs.Role, &patientID, &rs.MFAPending, &rs.MFAEnabled,
		&rs.LastSeenAt, &rs.AbsoluteExpiry)
	if err != nil {
		return nil, translate(err)
	}
	rs.PatientID = idPtr(patientID)
	return &rs, nil
}

const deleteDeadSessions = `
DELETE FROM sessions
 WHERE absolute_expiry < ? OR (revoked_at IS NOT NULL AND revoked_at < ?)
 LIMIT 5000`

// PurgeSessions deletes sessions that ended before cutoff, in batches so a
// large backlog never holds a long lock. Returns how many were removed.
func (r *PlatformRepository) PurgeSessions(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for {
		res, err := r.db.ExecContext(ctx, deleteDeadSessions, cutoff.UTC(), cutoff.UTC())
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < 5000 {
			return total, nil
		}
	}
}
