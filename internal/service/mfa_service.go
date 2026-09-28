package service

import (
	"context"
	"errors"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/repository"
	"github.com/yourorg/medical-expert-system/internal/secure"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// MFAService manages the signed-in user's second factor (BR-1.3).
type MFAService struct{ *base }

// MFAStatus describes a user's second factor.
type MFAStatus struct {
	Enabled                bool
	EnrolledAt             *time.Time
	RecoveryCodesRemaining int
	// Required is whether this user's role must have a second factor.
	Required bool
}

func (m *MFAService) Status(ctx context.Context) (*MFAStatus, error) {
	u, err := m.users().GetByID(ctx, m.actor.UserID)
	if err != nil {
		return nil, fromRepo(err)
	}
	return m.statusFor(ctx, u)
}

func (m *MFAService) statusFor(ctx context.Context, u *model.User) (*MFAStatus, error) {
	st := &MFAStatus{
		Enabled: u.MFAEnabled, EnrolledAt: u.MFAEnrolledAt,
		Required: m.reg.cfg.RequireMFA && u.Role.RequiresMFA(),
	}
	if u.MFAEnabled {
		n, err := m.users().CountUnusedRecoveryCodes(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		st.RecoveryCodesRemaining = n
	}
	return st, nil
}

// Enrolment is a secret waiting to be confirmed. The URI contains the
// secret, so it is rendered as a QR code on the client, never by a server
// or a third-party service.
type Enrolment struct {
	Secret  string
	URI     string
	Issuer  string
	Account string
}

// BeginEnrolment generates a secret. Nothing switches on until a code from
// the device proves the secret arrived (ConfirmEnrolment); otherwise a
// mistyped secret would lock the account out of itself.
func (m *MFAService) BeginEnrolment(ctx context.Context) (*Enrolment, error) {
	if m.state == StatePending {
		return nil, ErrMFARequired
	}
	u, err := m.users().GetByID(ctx, m.actor.UserID)
	if err != nil {
		return nil, fromRepo(err)
	}
	if u.MFAEnabled {
		return nil, ErrMFAAlreadyEnabled
	}
	secret, err := secure.NewTOTPSecret()
	if err != nil {
		return nil, err
	}
	if err := m.users().SetMFASecret(ctx, u.ID, []byte(secret), m.now()); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrMFAAlreadyEnabled
		}
		return nil, err
	}
	t, err := m.reg.platform.TenantByID(ctx, m.scope.TenantID())
	if err != nil {
		return nil, err
	}
	// The practice name in the issuer tells apart two accounts with the
	// same email at two practices in one authenticator app.
	issuer := m.reg.cfg.MFAIssuer + " - " + t.Name
	m.recordWrite(ctx, model.AuditEntry{Action: model.ActionMFAEnrolmentStarted, Outcome: model.OutcomeSuccess,
		SubjectType: "user", SubjectID: idp(u.ID)})
	return &Enrolment{
		Secret: secret, Issuer: issuer, Account: u.Email,
		URI: secure.TOTPProvisioningURI(issuer, u.Email, secret),
	}, nil
}

// RecoveryCodes are shown exactly once.
type RecoveryCodes struct {
	Codes []string
}

// ConfirmEnrolment switches the factor on once a code verifies, and issues
// recovery codes. Without them the first lost phone ends with someone
// disabling MFA for the whole practice.
func (m *MFAService) ConfirmEnrolment(ctx context.Context, code string) (*RecoveryCodes, error) {
	if m.state == StatePending {
		return nil, ErrMFARequired
	}
	users := m.users()
	u, err := users.GetByID(ctx, m.actor.UserID)
	if err != nil {
		return nil, fromRepo(err)
	}
	if u.MFAEnabled {
		return nil, ErrMFAAlreadyEnabled
	}
	if len(u.MFASecret) == 0 {
		return nil, ErrMFAEnrolmentNotStarted
	}
	now := m.now()
	counter, err := secure.VerifyTOTP(string(u.MFASecret), code, now, nil)
	if err != nil {
		if errors.Is(err, secure.ErrInvalidCode) || errors.Is(err, secure.ErrCodeReplayed) {
			return nil, ErrInvalidMFACode
		}
		return nil, err
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	err = m.scope.InTx(ctx, func(txScope *tenant.Scope) error {
		txUsers := repository.NewUserRepository(txScope)
		if err := txUsers.EnableMFA(ctx, u.ID, counter, now); err != nil {
			return err
		}
		return txUsers.ReplaceRecoveryCodes(ctx, u.ID, hashes, now)
	})
	if errors.Is(err, repository.ErrConflict) {
		return nil, ErrMFAAlreadyEnabled
	}
	if err != nil {
		return nil, err
	}
	m.recordWrite(ctx, model.AuditEntry{Action: model.ActionMFAEnabled, Outcome: model.OutcomeSuccess,
		SubjectType: "user", SubjectID: idp(u.ID)})
	return &RecoveryCodes{Codes: codes}, nil
}

// VerifyInput carries either an authenticator code or a recovery code.
type VerifyInput struct {
	Code         string
	RecoveryCode string
}

// Verify completes a sign-in that is waiting for a second factor. The
// pending session is consumed and a new, full session issued, so the token
// that only proved a password never becomes a full credential.
//
// A wrong code counts towards the same lockout as a wrong password.
func (m *MFAService) Verify(ctx context.Context, in VerifyInput) (*LoginResult, error) {
	if m.state != StatePending {
		return nil, ErrNotPending
	}
	users := m.users()
	u, err := users.GetByID(ctx, m.actor.UserID)
	if err != nil {
		return nil, fromRepo(err)
	}
	now := m.now()
	meta := RequestMeta{RequestID: m.actor.RequestID, IP: m.actor.IP, UserAgent: m.actor.UserAgent}
	if u.IsLocked(now) || u.Status != model.UserActive || !u.MFAEnabled {
		_ = m.sessions().Revoke(ctx, m.actor.SessionID, now, "mfa_refused")
		return nil, ErrInvalidMFACode
	}

	usedRecovery := false
	verified := false
	switch {
	case in.RecoveryCode != "":
		err := users.UseRecoveryCode(ctx, u.ID, secure.HashRecoveryCode(in.RecoveryCode), now)
		switch {
		case err == nil:
			verified, usedRecovery = true, true
		case !errors.Is(err, repository.ErrConflict):
			return nil, err
		}
	default:
		counter, err := secure.VerifyTOTP(string(u.MFASecret), in.Code, now, u.MFALastCounter)
		switch {
		case err == nil:
			// The database decides between two requests racing with the
			// same code: only one conditional update can match.
			err := users.AdvanceMFACounter(ctx, u.ID, counter)
			if err == nil {
				verified = true
			} else if !errors.Is(err, repository.ErrConflict) {
				return nil, err
			}
		case errors.Is(err, secure.ErrInvalidCode), errors.Is(err, secure.ErrCodeReplayed):
		default:
			return nil, err
		}
	}

	if !verified {
		locked, err := users.RecordFailedAttempt(ctx, u.ID, now, m.reg.cfg.MaxLoginAttempts, m.reg.cfg.Lockout)
		if err != nil {
			return nil, err
		}
		m.recordWrite(ctx, model.AuditEntry{Action: model.ActionMFAFailed, Outcome: model.OutcomeFailure,
			SubjectType: "user", SubjectID: idp(u.ID), Metadata: map[string]any{"recovery_code": in.RecoveryCode != ""}})
		if locked {
			m.reg.auditAccountLocked(ctx, m.scope.TenantID(), u, meta, now)
			_ = m.sessions().Revoke(ctx, m.actor.SessionID, now, "locked")
		}
		return nil, ErrInvalidMFACode
	}

	if err := m.sessions().ConsumePending(ctx, m.actor.SessionID, now); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrUnauthenticated
		}
		return nil, err
	}
	if err := users.RecordSuccessfulLogin(ctx, u.ID, now); err != nil {
		return nil, err
	}
	token, expires, err := m.reg.openSession(ctx, m.scope, u.ID, false, meta)
	if err != nil {
		return nil, err
	}
	t, err := m.reg.platform.TenantByID(ctx, m.scope.TenantID())
	if err != nil {
		return nil, err
	}
	m.recordWrite(ctx, model.AuditEntry{Action: model.ActionMFAVerified, Outcome: model.OutcomeSuccess,
		SubjectType: "user", SubjectID: idp(u.ID), Metadata: map[string]any{"recovery_code": usedRecovery}})
	if usedRecovery {
		// Usually a lost phone; occasionally someone else holding the
		// printout. Either way a person should look at it.
		remaining, _ := users.CountUnusedRecoveryCodes(ctx, u.ID)
		m.reg.log.WarnContext(ctx, "recovery code used to sign in",
			"tenant", m.scope.TenantID().String(), "user", u.ID.String(), "remaining", remaining)
		m.recordWrite(ctx, model.AuditEntry{Action: model.ActionMFARecoveryCodeUsed, Outcome: model.OutcomeSuccess,
			SubjectType: "user", SubjectID: idp(u.ID), Metadata: map[string]any{"remaining": remaining}})
	}
	return &LoginResult{State: StateActive, Token: token, ExpiresAt: expires, User: u, Tenant: t}, nil
}

// RegenerateRecoveryCodes replaces every recovery code. It needs a current
// code, so a session left open on a shared computer cannot mint new ones.
func (m *MFAService) RegenerateRecoveryCodes(ctx context.Context, code string) (*RecoveryCodes, error) {
	if m.state != StateActive {
		return nil, ErrMFARequired
	}
	users := m.users()
	u, err := users.GetByID(ctx, m.actor.UserID)
	if err != nil {
		return nil, fromRepo(err)
	}
	if !u.MFAEnabled {
		return nil, ErrMFANotEnabled
	}
	now := m.now()
	counter, err := secure.VerifyTOTP(string(u.MFASecret), code, now, u.MFALastCounter)
	if err != nil {
		if errors.Is(err, secure.ErrInvalidCode) || errors.Is(err, secure.ErrCodeReplayed) {
			return nil, ErrInvalidMFACode
		}
		return nil, err
	}
	if err := users.AdvanceMFACounter(ctx, u.ID, counter); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrInvalidMFACode
		}
		return nil, err
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	if err := users.ReplaceRecoveryCodes(ctx, u.ID, hashes, now); err != nil {
		return nil, err
	}
	m.recordWrite(ctx, model.AuditEntry{Action: model.ActionMFARecoveryCodesIssue, Outcome: model.OutcomeSuccess,
		SubjectType: "user", SubjectID: idp(u.ID)})
	return &RecoveryCodes{Codes: codes}, nil
}

func newRecoveryCodes() ([]string, [][32]byte, error) {
	codes, err := secure.NewRecoveryCodes()
	if err != nil {
		return nil, nil, err
	}
	hashes := make([][32]byte, len(codes))
	for i, c := range codes {
		hashes[i] = secure.HashRecoveryCode(c)
	}
	return codes, hashes, nil
}
