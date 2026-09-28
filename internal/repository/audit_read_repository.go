package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// AuditReadRepository reads a practice's own audit trail. Unlike writes,
// reads are tenant-scoped: a practice administrator can see what happened in
// their practice and nothing else.
type AuditReadRepository struct {
	scope *tenant.Scope
}

func NewAuditReadRepository(s *tenant.Scope) *AuditReadRepository {
	return &AuditReadRepository{scope: s}
}

// AuditFilter narrows a listing. Zero values mean "any".
type AuditFilter struct {
	SubjectID *model.ID
	ActorID   *model.ID
	Action    string
	Since     time.Time
	BeforeSeq uint64 // cursor: only entries older than this sequence number
	Limit     int
}

// NULL-tolerant predicates keep this one fixed statement rather than SQL
// assembled per filter, which the guard could not verify.
const selectAudit = `
SELECT id, actor_id, actor_role, action, subject_type, subject_id, outcome, reason, request_id, ip, metadata, occurred_at
  FROM audit_log
 WHERE tenant_id = :tenant
   AND (? IS NULL OR subject_id = ?)
   AND (? IS NULL OR actor_id = ?)
   AND (? = '' OR action = ?)
   AND occurred_at >= ?
   AND id < ?
 ORDER BY id DESC
 LIMIT ?`

func (r *AuditReadRepository) List(ctx context.Context, f AuditFilter) ([]model.AuditRecord, error) {
	before := f.BeforeSeq
	if before == 0 {
		before = ^uint64(0) >> 1
	}
	subject, actor := nullID(f.SubjectID), nullID(f.ActorID)
	rows, err := r.scope.Query(ctx, selectAudit,
		subject, subject, actor, actor, f.Action, f.Action, f.Since.UTC(), before, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AuditRecord{}
	for rows.Next() {
		var rec model.AuditRecord
		var actorID, subjectID sql.Null[model.ID]
		var role, subjectType, reason, requestID, ip, meta sql.Null[string]
		if err := rows.Scan(&rec.Seq, &actorID, &role, &rec.Action, &subjectType, &subjectID, &rec.Outcome,
			&reason, &requestID, &ip, &meta, &rec.OccurredAt); err != nil {
			return nil, err
		}
		rec.ActorID, rec.SubjectID = idPtr(actorID), idPtr(subjectID)
		rec.ActorRole, rec.SubjectType, rec.Reason, rec.RequestID, rec.IP = role.V, subjectType.V, reason.V, requestID.V, ip.V
		if meta.Valid && meta.V != "" {
			_ = json.Unmarshal([]byte(meta.V), &rec.Metadata)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
