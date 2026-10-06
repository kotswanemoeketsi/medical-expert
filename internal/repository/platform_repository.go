package repository

// UNSCOPED BY DESIGN. This file holds a raw *sql.DB because its job comes
// before a tenant scope can exist: finding the practice a login names,
// resolving a bearer token to the tenant it belongs to, and working out
// which practice a WhatsApp receipt or reply belongs to. It returns
// identifiers and practice details, never patient data.
//
// Statements here run on the raw handle with no binder, so the ":tenant"
// marker must never appear in this file (TestUnscopedFilesNeverUseTheScopeMarker).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

const tenantColumns = `id, slug, name, status, timezone, practice_number, address, phone, email, settings_json,
       created_at, updated_at`

func scanTenant(sc scanner) (*model.Tenant, error) {
	var t model.Tenant
	var practiceNo, address, phone, email sql.Null[string]
	var settings []byte
	if err := sc.Scan(&t.ID, &t.Slug, &t.Name, &t.Status, &t.Timezone,
		&practiceNo, &address, &phone, &email, &settings, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, translate(err)
	}
	t.PracticeNumber, t.Address, t.Phone, t.Email = practiceNo.V, address.V, phone.V, email.V
	if err := decodeSettings(settings, &t.Settings); err != nil {
		return nil, err
	}
	return &t, nil
}

// decodeSettings reads settings_json. NULL and {} both mean every default.
func decodeSettings(raw []byte, into *model.PracticeSettings) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("repository: practice settings are not valid JSON: %w", err)
	}
	return nil
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

const selectActiveTenants = `SELECT ` + tenantColumns + ` FROM tenants WHERE status = 'active' ORDER BY id`

// ActiveTenants lists practices, with their settings, for jobs that depend
// on them.
func (r *PlatformRepository) ActiveTenants(ctx context.Context) ([]model.Tenant, error) {
	rows, err := r.db.QueryContext(ctx, selectActiveTenants)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

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

// ---------------------------------------------------------------------------
// WhatsApp webhooks. A delivery receipt or a patient's reply arrives with no
// session, so which practice it belongs to has to be worked out before a
// scope can exist — from the message it refers to. These return identifiers
// only; everything else is read through the practice's own scope.
// ---------------------------------------------------------------------------

// NotificationRef identifies a message and the practice that sent it.
type NotificationRef struct {
	TenantID       model.ID
	NotificationID model.ID
}

const selectNotificationByReplyToken = `SELECT tenant_id, id FROM notifications WHERE reply_token = ?`

const selectNotificationByProviderID = `SELECT tenant_id, id FROM notifications WHERE provider_message_id = ?`

// The latest message that reached a number, for a reply that names nothing
// else: typed text rather than a button, and not a swipe-reply.
const selectLatestNotificationTo = `
SELECT tenant_id, id FROM notifications
 WHERE destination = ? AND created_at >= ? AND status IN ('sent', 'delivered', 'read')
 ORDER BY created_at DESC, id
 LIMIT 1`

func (r *PlatformRepository) scanRef(row *sql.Row) (*NotificationRef, error) {
	var ref NotificationRef
	if err := row.Scan(&ref.TenantID, &ref.NotificationID); err != nil {
		return nil, translate(err)
	}
	return &ref, nil
}

// NotificationByReplyToken finds the message whose button was pressed.
func (r *PlatformRepository) NotificationByReplyToken(ctx context.Context, token string) (*NotificationRef, error) {
	return r.scanRef(r.db.QueryRowContext(ctx, selectNotificationByReplyToken, token))
}

// NotificationByProviderID finds a message by the provider's ID, for a
// receipt or a swipe-reply.
func (r *PlatformRepository) NotificationByProviderID(ctx context.Context, providerID string) (*NotificationRef, error) {
	return r.scanRef(r.db.QueryRowContext(ctx, selectNotificationByProviderID, providerID))
}

// LatestNotificationTo finds the most recent message that reached a number
// since a moment.
func (r *PlatformRepository) LatestNotificationTo(ctx context.Context, destination string, since time.Time) (*NotificationRef, error) {
	return r.scanRef(r.db.QueryRowContext(ctx, selectLatestNotificationTo, destination, since.UTC()))
}

// ---------------------------------------------------------------------------
// Shared reference data: the medication catalogue (BR-0.10). Not tenant data
// — every practice reads the same list — so it lives with the other
// platform-level statements. Tenant-scoped statements may still join it.
// ---------------------------------------------------------------------------

const medicationColumns = `id, generic_name, brand_name, strength, dosage_form, route, schedule, nappi_code, pack_size, is_active`

func scanMedication(sc scanner) (*model.Medication, error) {
	var m model.Medication
	var schedule, nappi, pack sql.Null[string]
	if err := sc.Scan(&m.ID, &m.GenericName, &m.BrandName, &m.Strength, &m.DosageForm, &m.Route,
		&schedule, &nappi, &pack, &m.IsActive); err != nil {
		return nil, translate(err)
	}
	m.Schedule, m.NAPPICode, m.PackSize = schedule.V, nappi.V, pack.V
	return &m, nil
}

const selectMedicationByID = `SELECT ` + medicationColumns + ` FROM medications WHERE id = ?`

func (r *PlatformRepository) MedicationByID(ctx context.Context, id model.ID) (*model.Medication, error) {
	return scanMedication(r.db.QueryRowContext(ctx, selectMedicationByID, id.Bytes()))
}

// Strengths sort by their leading number, so 5 mg comes before 10 mg; the
// text breaks ties ("120 mg/5 ml" and "120 mg/ml").
const listMedications = `
SELECT ` + medicationColumns + ` FROM medications
 WHERE is_active = 1
 ORDER BY generic_name, brand_name, CAST(strength AS DECIMAL(12,4)), strength, dosage_form
 LIMIT ? OFFSET ?`

const searchMedications = `
SELECT ` + medicationColumns + ` FROM medications
 WHERE is_active = 1 AND (generic_name LIKE ? OR brand_name LIKE ? OR nappi_code = ?)
 ORDER BY generic_name, brand_name, CAST(strength AS DECIMAL(12,4)), strength, dosage_form
 LIMIT ? OFFSET ?`

// SearchMedications finds catalogue entries by generic or brand name prefix
// or exact NAPPI code. An empty query lists.
func (r *PlatformRepository) SearchMedications(ctx context.Context, q string, limit, offset int) ([]model.Medication, error) {
	var rows *sql.Rows
	var err error
	if q == "" {
		rows, err = r.db.QueryContext(ctx, listMedications, limit, offset)
	} else {
		like := likePrefix(q)
		rows, err = r.db.QueryContext(ctx, searchMedications, like, like, q, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Medication{}
	for rows.Next() {
		m, err := scanMedication(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

const selectMedicationNames = `
SELECT generic_name FROM medications
 UNION
SELECT brand_name FROM medications WHERE brand_name <> ''`

// MedicationNames lists every generic and brand name in the catalogue, so a
// message can be checked for them before it goes out.
func (r *PlatformRepository) MedicationNames(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, selectMedicationNames)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// A catalogue row is identified by its product key; importing the same
// product again updates its details rather than duplicating it.
const upsertMedication = `
INSERT INTO medications (id, generic_name, brand_name, strength, dosage_form, route, schedule, nappi_code,
                         pack_size, is_active, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE route = VALUES(route), schedule = VALUES(schedule), nappi_code = VALUES(nappi_code),
                        pack_size = VALUES(pack_size), is_active = VALUES(is_active), updated_at = VALUES(updated_at)`

const countMedicationByID = `SELECT COUNT(*) FROM medications WHERE id = ?`

// UpsertMedication adds or updates a catalogue entry. It reports whether a
// new row was inserted.
func (r *PlatformRepository) UpsertMedication(ctx context.Context, m *model.Medication, now time.Time) (bool, error) {
	if _, err := r.db.ExecContext(ctx, upsertMedication, m.ID.Bytes(), m.GenericName, m.BrandName, m.Strength,
		m.DosageForm, string(m.Route), nullString(m.Schedule), nullString(m.NAPPICode), nullString(m.PackSize),
		m.IsActive, now.UTC(), now.UTC()); err != nil {
		return false, translate(err)
	}
	// The affected-row count cannot tell an insert from an unchanged update
	// under clientFoundRows, but only an insert stores the ID we generated.
	var n int
	if err := r.db.QueryRowContext(ctx, countMedicationByID, m.ID.Bytes()).Scan(&n); err != nil {
		return false, err
	}
	return n == 1, nil
}
