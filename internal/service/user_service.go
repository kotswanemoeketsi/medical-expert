package service

import (
	"context"
	"errors"
	"strings"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/phone"
	"github.com/yourorg/medical-expert-system/internal/repository"
	"github.com/yourorg/medical-expert-system/internal/secure"
)

// UserService is practice administration: staff and patient logins
// (BR-1.4, BR-1.5, BR-1.7).
type UserService struct{ *base }

// CreateUserInput describes a new login. Accounts are created by an
// administrator and active immediately; the administrator hands over the
// initial password out of band and the user changes it.
type CreateUserInput struct {
	Email             string
	FullName          string
	Role              model.Role
	ProfessionalRegNo string
	Mobile            string
	PatientID         *model.ID
	Password          string
}

// Validate normalises and checks the input.
func (in *CreateUserInput) Validate() model.ValidationErrors {
	var v model.ValidationErrors
	in.Email = normaliseEmail(in.Email)
	in.FullName = strings.TrimSpace(in.FullName)
	in.ProfessionalRegNo = strings.ToUpper(strings.TrimSpace(in.ProfessionalRegNo))

	if !validEmail(in.Email) {
		v.Add("email", "is not a valid email address")
	}
	if in.FullName == "" || len(in.FullName) > 200 {
		v.Add("full_name", "is required (at most 200 characters)")
	}
	switch {
	case !in.Role.Valid():
		v.Add("role", "must be one of clinic_admin, doctor, nurse, patient")
	case in.Role == model.RoleSuperAdmin:
		v.Add("role", "platform operator accounts cannot be created by a practice")
	case in.Role == model.RoleCaregiver:
		v.Add("role", "caregiver accounts are not available yet")
	}
	if in.Role.RequiresProfessionalRegistration() {
		// BR-1.5: clinical access is not granted without registration details.
		if in.ProfessionalRegNo == "" {
			v.Add("professional_reg_no", "is required for clinical staff (e.g. the HPCSA or SANC number)")
		} else if len(in.ProfessionalRegNo) > 64 {
			v.Add("professional_reg_no", "is too long")
		}
	}
	if in.Role == model.RolePatient {
		if in.PatientID == nil || in.PatientID.IsZero() {
			v.Add("patient_id", "is required for a patient login")
		}
	} else if in.PatientID != nil {
		v.Add("patient_id", "only applies to patient logins")
	}
	if strings.TrimSpace(in.Mobile) != "" {
		m, err := phone.NormalizeE164(in.Mobile)
		if err != nil {
			v.Add("mobile", "%s", err.Error())
		}
		in.Mobile = m
	}
	if err := secure.CheckPasswordPolicy(in.Password, in.Email, in.FullName); err != nil {
		v.Add("password", "%s", err.Error())
	}
	return v
}

func (s *UserService) Create(ctx context.Context, in CreateUserInput) (*model.User, error) {
	if err := s.requireAdmin(); err != nil {
		return nil, err
	}
	if v := in.Validate(); len(v) > 0 {
		return nil, invalid(v)
	}
	if in.Role == model.RolePatient {
		ok, err := s.patients().Exists(ctx, *in.PatientID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, invalidField("patient_id", "is not a patient at this practice")
		}
	}
	hash, err := s.reg.hasher.Hash(in.Password)
	if err != nil {
		return nil, err
	}
	now := s.now()
	u := &model.User{
		ID: model.MustNewID(), TenantID: s.scope.TenantID(), Email: in.Email, FullName: in.FullName,
		Mobile: in.Mobile, PasswordHash: hash, Role: in.Role, ProfessionalRegNo: in.ProfessionalRegNo,
		PatientID: in.PatientID, Status: model.UserActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.users().Create(ctx, u, idp(s.actor.UserID)); err != nil {
		var dup *repository.DuplicateKeyError
		if errors.As(err, &dup) {
			if dup.Key == "uq_users_patient" {
				return nil, ErrPatientHasLogin
			}
			return nil, ErrEmailTaken
		}
		return nil, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionUserCreated, Outcome: model.OutcomeSuccess,
		SubjectType: "user", SubjectID: idp(u.ID), Metadata: map[string]any{"role": string(u.Role), "email": u.Email}})
	if u.PatientID != nil {
		s.refreshSchedule(ctx, *u.PatientID) // a patient with a login can confirm doses themselves
	}
	return u, nil
}

func (s *UserService) List(ctx context.Context) ([]model.User, error) {
	if err := s.requireAdmin(); err != nil {
		return nil, err
	}
	return s.users().List(ctx)
}

// SetStatus suspends, deactivates or reactivates an account. Anything other
// than active revokes every session the user holds, immediately (BR-1.7);
// their history and audit trail are kept. Setting an account active also
// clears a lockout, which is how an administrator unlocks someone who
// mistyped their password five times.
func (s *UserService) SetStatus(ctx context.Context, id model.ID, status model.UserStatus, reason string) (*model.User, error) {
	if err := s.requireAdmin(); err != nil {
		return nil, err
	}
	if id == s.actor.UserID {
		return nil, ErrSelfAction
	}
	reason = strings.TrimSpace(reason)
	var v model.ValidationErrors
	if !status.Valid() || status == model.UserPending {
		v.Add("status", "must be active, suspended or deactivated")
	}
	if status != model.UserActive && reason == "" {
		v.Add("reason", "is required when suspending or deactivating an account")
	}
	if len(reason) > 255 {
		v.Add("reason", "must be at most 255 characters")
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	users := s.users()
	target, err := users.GetByID(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if target.Role == model.RoleSuperAdmin {
		return nil, ErrForbidden
	}
	now := s.now()
	if err := users.UpdateStatus(ctx, id, status, now); err != nil {
		return nil, fromRepo(err)
	}
	var revoked int64
	unlocked := false
	if status != model.UserActive {
		if revoked, err = s.sessions().RevokeAllForUser(ctx, id, now, "status_"+string(status)); err != nil {
			return nil, err
		}
	} else if target.FailedAttempts > 0 || target.LockedUntil != nil {
		if err := users.ClearLockout(ctx, id); err != nil {
			return nil, fromRepo(err)
		}
		unlocked = target.IsLocked(now)
		target.FailedAttempts, target.LockedUntil = 0, nil
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionUserStatusChanged, Outcome: model.OutcomeSuccess,
		SubjectType: "user", SubjectID: idp(id), Reason: reason,
		Metadata: map[string]any{"from": string(target.Status), "to": string(status), "sessions_revoked": revoked, "unlocked": unlocked}})
	target.Status, target.UpdatedAt = status, now
	if target.PatientID != nil {
		s.refreshSchedule(ctx, *target.PatientID)
	}
	return target, nil
}

// ResetMFA removes a user's second factor so they can enrol a new device,
// and signs them out everywhere: if the old device is in someone else's
// hands, so are the sessions on it. Not available on one's own account.
func (s *UserService) ResetMFA(ctx context.Context, id model.ID, reason string) error {
	if err := s.requireAdmin(); err != nil {
		return err
	}
	if id == s.actor.UserID {
		return ErrSelfAction
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 255 {
		return invalidField("reason", "is required (at most 255 characters): say why the factor is being reset")
	}
	target, err := s.users().GetByID(ctx, id)
	if err != nil {
		return fromRepo(err)
	}
	if target.Role == model.RoleSuperAdmin {
		return ErrForbidden
	}
	now := s.now()
	if err := s.users().ResetMFA(ctx, id, now); err != nil {
		return fromRepo(err)
	}
	revoked, err := s.sessions().RevokeAllForUser(ctx, id, now, "mfa_reset")
	if err != nil {
		return err
	}
	s.reg.log.WarnContext(ctx, "second factor reset by administrator",
		"tenant", s.scope.TenantID().String(), "user", id.String(), "by", s.actor.UserID.String())
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionMFAReset, Outcome: model.OutcomeSuccess,
		SubjectType: "user", SubjectID: idp(id), Reason: reason,
		Metadata: map[string]any{"sessions_revoked": revoked, "was_enabled": target.MFAEnabled}})
	return nil
}
