package repository

import (
	"context"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// SessionRepository creates and revokes sessions. Resolving a token to a
// session happens before a tenant is known and so lives in the platform
// repository; everything after that is tenant-scoped here.
type SessionRepository struct {
	scope *tenant.Scope
}

func NewSessionRepository(s *tenant.Scope) *SessionRepository { return &SessionRepository{scope: s} }

const insertSession = `
INSERT INTO sessions (tenant_id, id, user_id, token_hash, mfa_pending, issued_at, last_seen_at, absolute_expiry, ip, user_agent)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func (r *SessionRepository) Create(ctx context.Context, s *model.Session) error {
	_, err := r.scope.Exec(ctx, insertSession,
		s.ID.Bytes(), s.UserID.Bytes(), s.TokenHash[:], s.MFAPending, s.IssuedAt, s.LastSeenAt,
		s.AbsoluteExpiry, nullString(s.IP), nullString(truncate(s.UserAgent, 255)))
	return translate(err)
}

const touchSession = `
UPDATE sessions SET last_seen_at = ?
 WHERE tenant_id = :tenant AND id = ? AND revoked_at IS NULL`

// Touch records activity for the idle timeout. Callers write at most once a
// minute: the timeout is measured in tens of minutes, so a minute of slack
// costs nothing and saves a write on every request.
func (r *SessionRepository) Touch(ctx context.Context, id model.ID, now time.Time) error {
	_, err := r.scope.Exec(ctx, touchSession, now, id.Bytes())
	return err
}

const revokeSession = `
UPDATE sessions SET revoked_at = ?, revoked_reason = ?
 WHERE tenant_id = :tenant AND id = ? AND revoked_at IS NULL`

// Revoke ends one session. Revoking an already-revoked session is not an
// error: signing out twice should not fail.
func (r *SessionRepository) Revoke(ctx context.Context, id model.ID, now time.Time, reason string) error {
	_, err := r.scope.Exec(ctx, revokeSession, now, reason, id.Bytes())
	return err
}

const revokePendingSession = `
UPDATE sessions SET revoked_at = ?, revoked_reason = ?
 WHERE tenant_id = :tenant AND id = ? AND mfa_pending = 1 AND revoked_at IS NULL`

// ConsumePending revokes a half-authenticated session as it is exchanged for
// a full one. ErrConflict means it was already exchanged — the same pending
// session cannot yield two full sessions.
func (r *SessionRepository) ConsumePending(ctx context.Context, id model.ID, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, revokePendingSession, now, "mfa_completed", id.Bytes()))
}

const revokeUserSessions = `
UPDATE sessions SET revoked_at = ?, revoked_reason = ?
 WHERE tenant_id = :tenant AND user_id = ? AND revoked_at IS NULL`

// RevokeAllForUser ends every session a user holds (BR-1.7). Returns how many.
func (r *SessionRepository) RevokeAllForUser(ctx context.Context, userID model.ID, now time.Time, reason string) (int64, error) {
	res, err := r.scope.Exec(ctx, revokeUserSessions, now, reason, userID.Bytes())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const revokeOtherUserSessions = `
UPDATE sessions SET revoked_at = ?, revoked_reason = ?
 WHERE tenant_id = :tenant AND user_id = ? AND id <> ? AND revoked_at IS NULL`

// RevokeOthersForUser ends every session except the current one, after a
// password change.
func (r *SessionRepository) RevokeOthersForUser(ctx context.Context, userID, keep model.ID, now time.Time, reason string) (int64, error) {
	res, err := r.scope.Exec(ctx, revokeOtherUserSessions, now, reason, userID.Bytes(), keep.Bytes())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
