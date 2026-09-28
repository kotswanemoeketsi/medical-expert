package repository

// UNSCOPED BY DESIGN. The audit log must capture events that have no tenant
// (a login attempt against a practice that does not exist) and must never be
// filtered out of existence by a scoping bug, so writes take the tenant as an
// explicit field on a raw handle. Reading the log back is tenant-scoped and
// lives in audit_read_repository.go.
//
// Statements here run on the raw handle with no binder, so the ":tenant"
// marker must never appear in this file.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/yourorg/medical-expert-system/internal/model"
)

// AuditRepository appends to the audit log. It has no update or delete.
type AuditRepository struct {
	db *sql.DB
}

func NewAuditRepository(db *sql.DB) *AuditRepository { return &AuditRepository{db: db} }

const insertAudit = `
INSERT INTO audit_log
  (tenant_id, actor_id, actor_role, action, subject_type, subject_id, outcome, reason, request_id, ip, metadata, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// Record appends one entry.
func (r *AuditRepository) Record(ctx context.Context, e model.AuditEntry) error {
	var meta any
	if len(e.Metadata) > 0 {
		b, err := json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("audit metadata: %w", err)
		}
		meta = string(b)
	}
	_, err := r.db.ExecContext(ctx, insertAudit,
		nullID(e.TenantID), nullID(e.ActorID), nullString(e.ActorRole), e.Action,
		nullString(e.SubjectType), nullID(e.SubjectID), string(e.Outcome), nullString(truncate(e.Reason, 255)),
		nullString(e.RequestID), nullString(e.IP), meta, e.OccurredAt.UTC())
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
