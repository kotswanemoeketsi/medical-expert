package service

import (
	"context"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/repository"
)

// AuditService lets a practice administrator read their own audit trail
// (BR-8.3). Reading the trail is itself audited.
type AuditService struct{ *base }

// AuditQuery narrows a listing.
type AuditQuery struct {
	SubjectID *model.ID
	ActorID   *model.ID
	Action    string
	Since     time.Time
	BeforeSeq uint64
	Limit     int
}

func (a *AuditService) List(ctx context.Context, q AuditQuery) ([]model.AuditRecord, error) {
	if err := a.requireAdmin(); err != nil {
		return nil, err
	}
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	if len(q.Action) > 64 {
		return nil, invalidField("action", "is too long")
	}
	meta := map[string]any{"limit": q.Limit}
	if q.SubjectID != nil {
		meta["subject_id"] = q.SubjectID.String()
	}
	if q.ActorID != nil {
		meta["actor_id"] = q.ActorID.String()
	}
	if err := a.record(ctx, model.AuditEntry{Action: model.ActionAuditRead, Outcome: model.OutcomeSuccess, Metadata: meta}); err != nil {
		return nil, err
	}
	return repository.NewAuditReadRepository(a.scope).List(ctx, repository.AuditFilter{
		SubjectID: q.SubjectID, ActorID: q.ActorID, Action: q.Action, Since: q.Since,
		BeforeSeq: q.BeforeSeq, Limit: q.Limit,
	})
}
