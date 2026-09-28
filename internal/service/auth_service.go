package service

import (
	"context"
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/phone"
	"github.com/yourorg/medical-expert-system/internal/repository"
	"github.com/yourorg/medical-expert-system/internal/secure"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// LoginInput is what a sign-in form posts.
type LoginInput struct {
	TenantSlug string
	Email      string
	Password   string
}

// LoginResult is a new session. Token is the only copy of the bearer
// token; it is not stored and cannot be recovered.
type LoginResult struct {
	State     SessionState
	Token     string
	ExpiresAt time.Time
	User      *model.User   // nil while a second factor is outstanding
	Tenant    *model.Tenant // nil while a second factor is outstanding
}

// Login checks a password and opens a session.
//
// Every failure returns the same error with the same message, and an unknown
// account costs the same password-hashing time as a known one, so neither
// the response nor its timing reveals whether a practice or email exists.
// The audit log records the real reason.
func (r *Registry) Login(ctx context.Context, in LoginInput, meta RequestMeta) (*LoginResult, error) {
	slug := strings.ToLower(strings.TrimSpace(in.TenantSlug))
	email := normaliseEmail(in.Email)
	now := r.now()

	fail := func(t *model.Tenant, u *model.User, reason string) error {
		e := model.AuditEntry{
			Action: model.ActionLoginFailed, Outcome: model.OutcomeFailure, Reason: reason,
			RequestID: meta.RequestID, IP: meta.IP, OccurredAt: now,
			Metadata: map[string]any{"email": email, "tenant_slug": slug},
		}
		if t != nil {
			e.TenantID = idp(t.ID)
		}
		if u != nil {
			e.SubjectType, e.SubjectID = "user", idp(u.ID)
		}
		if err := r.audit.Record(ctx, e); err != nil {
			r.log.ErrorContext(ctx, "audit write failed", "action", e.Action, "err", err)
		}
		return ErrInvalidCredentials
	}
	burn := func() { _, _, _ = r.hasher.Verify(r.dummyHash, in.Password) }

	if slug == "" || email == "" || in.Password == "" {
		burn()
		return nil, fail(nil, nil, "missing_fields")
	}
	t, err := r.platform.TenantBySlug(ctx, slug)
	if errors.Is(err, repository.ErrNotFound) {
		burn()
		return nil, fail(nil, nil, "unknown_tenant")
	}
	if err != nil {
		return nil, err
	}
	if t.Status != model.TenantActive {
		burn()
		return nil, fail(t, nil, "tenant_"+string(t.Status))
	}
	scope, err := tenant.NewScope(r.db, t.ID)
	if err != nil {
		return nil, err
	}
	users := repository.NewUserRepository(scope)
	u, err := users.GetByEmail(ctx, email)
	if errors.Is(err, repository.ErrNotFound) {
		burn()
		return nil, fail(t, nil, "unknown_user")
	}
	if err != nil {
		return nil, err
	}
	if u.IsLocked(now) {
		// Checked before the password, and without counting another
		// failure, so an attacker cannot keep an account locked forever
		// just by continuing to guess.
		burn()
		return nil, fail(t, u, "locked")
	}
	ok, rehash, err := r.hasher.Verify(u.PasswordHash, in.Password)
	if err != nil {
		r.log.ErrorContext(ctx, "stored password hash unreadable", "user", u.ID.String(), "err", err)
		return nil, fail(t, u, "hash_unreadable")
	}
	if !ok {
		locked, err := users.RecordFailedAttempt(ctx, u.ID, now, r.cfg.MaxLoginAttempts, r.cfg.Lockout)
		if err != nil {
			return nil, err
		}
		if locked {
			r.auditAccountLocked(ctx, t.ID, u, meta, now)
		}
		return nil, fail(t, u, "bad_password")
	}
	if u.Status != model.UserActive {
		return nil, fail(t, u, "status_"+string(u.Status))
	}
	if rehash {
		if h, err := r.hasher.Hash(in.Password); err == nil {
			if err := users.UpdatePasswordHash(ctx, u.ID, h, now); err != nil {
				r.log.WarnContext(ctx, "password rehash failed", "err", err)
			}
		}
	}

	if u.MFAEnabled {
		// The failure count is deliberately not reset here: a stolen
		// password must not buy a fresh set of guesses at the code.
		token, expires, err := r.openSession(ctx, scope, u.ID, true, meta)
		if err != nil {
			return nil, err
		}
		r.recordFor(ctx, t.ID, u, meta, model.AuditEntry{Action: model.ActionLoginMFAChallenged, Outcome: model.OutcomeSuccess})
		return &LoginResult{State: StatePending, Token: token, ExpiresAt: expires}, nil
	}

	if err := users.RecordSuccessfulLogin(ctx, u.ID, now); err != nil {
		return nil, err
	}
	token, expires, err := r.openSession(ctx, scope, u.ID, false, meta)
	if err != nil {
		return nil, err
	}
	state := StateActive
	if r.cfg.RequireMFA && u.Role.RequiresMFA() {
		state = StateSetup
	}
	r.recordFor(ctx, t.ID, u, meta, model.AuditEntry{Action: model.ActionLoginSucceeded, Outcome: model.OutcomeSuccess,
		Metadata: map[string]any{"mfa": false, "state": string(state)}})
	return &LoginResult{State: state, Token: token, ExpiresAt: expires, User: u, Tenant: t}, nil
}

// openSession creates a session and returns its bearer token. A pending
// session lives only long enough to type a code.
func (r *Registry) openSession(ctx context.Context, scope *tenant.Scope, userID model.ID, pending bool, meta RequestMeta) (string, time.Time, error) {
	token, digest, err := secure.NewSessionToken()
	if err != nil {
		return "", time.Time{}, err
	}
	now := r.now()
	expires := now.Add(r.cfg.SessionMax)
	if pending {
		expires = now.Add(r.cfg.MFAChallengeTTL)
	}
	id, err := model.NewID()
	if err != nil {
		return "", time.Time{}, err
	}
	s := &model.Session{
		ID: id, UserID: userID, TokenHash: digest, MFAPending: pending,
		IssuedAt: now, LastSeenAt: now, AbsoluteExpiry: expires, IP: meta.IP, UserAgent: meta.UserAgent,
	}
	if err := repository.NewSessionRepository(scope).Create(ctx, s); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

func (r *Registry) recordFor(ctx context.Context, tenantID model.ID, u *model.User, meta RequestMeta, e model.AuditEntry) {
	e.TenantID = idp(tenantID)
	e.ActorID, e.ActorRole = idp(u.ID), string(u.Role)
	e.SubjectType, e.SubjectID = "user", idp(u.ID)
	e.RequestID, e.IP = meta.RequestID, meta.IP
	if e.OccurredAt.IsZero() {
		e.OccurredAt = r.now()
	}
	if err := r.audit.Record(ctx, e); err != nil {
		r.log.ErrorContext(ctx, "audit write failed", "action", e.Action, "err", err)
	}
}

func (r *Registry) auditAccountLocked(ctx context.Context, tenantID model.ID, u *model.User, meta RequestMeta, now time.Time) {
	r.log.WarnContext(ctx, "account locked after repeated failures", "tenant", tenantID.String(), "user", u.ID.String())
	e := model.AuditEntry{
		TenantID: idp(tenantID), Action: model.ActionAccountLocked, Outcome: model.OutcomeSuccess,
		SubjectType: "user", SubjectID: idp(u.ID), RequestID: meta.RequestID, IP: meta.IP, OccurredAt: now,
		Metadata: map[string]any{"lockout_minutes": int(r.cfg.Lockout / time.Minute)},
	}
	if err := r.audit.Record(ctx, e); err != nil {
		r.log.ErrorContext(ctx, "audit write failed", "action", e.Action, "err", err)
	}
}

// ---------------------------------------------------------------------------
// Provisioning a practice (BR-0.4)
// ---------------------------------------------------------------------------

// ProvisionInput creates a practice and its first administrator.
type ProvisionInput struct {
	Slug           string
	Name           string
	Timezone       string
	PracticeNumber string
	Address        string
	Phone          string
	Email          string
	AdminEmail     string
	AdminName      string
	AdminPassword  string // empty: one is generated and returned once
}

// ProvisionResult carries the administrator's password when it was
// generated, because this is the only moment it exists in plaintext.
type ProvisionResult struct {
	Tenant            *model.Tenant
	Admin             *model.User
	GeneratedPassword string
}

var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,61}[a-z0-9])$`)

// Provision creates a practice and its first administrator together, in one
// transaction, so a failure never leaves a practice nobody can sign in to.
func (r *Registry) Provision(ctx context.Context, in ProvisionInput) (*ProvisionResult, error) {
	var v model.ValidationErrors
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	if !slugPattern.MatchString(slug) {
		v.Add("slug", "must be 3-63 lower-case letters, digits or hyphens, starting and ending with a letter or digit")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 255 {
		v.Add("name", "is required (at most 255 characters)")
	}
	tz := strings.TrimSpace(in.Timezone)
	if tz == "" {
		tz = "Africa/Johannesburg"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		v.Add("timezone", "is not a known time zone")
	}
	practicePhone := ""
	if strings.TrimSpace(in.Phone) != "" {
		p, err := phone.NormalizeE164(in.Phone)
		if err != nil {
			v.Add("phone", "%s", err.Error())
		}
		practicePhone = p
	}
	adminEmail := normaliseEmail(in.AdminEmail)
	if !validEmail(adminEmail) {
		v.Add("admin_email", "is not a valid email address")
	}
	adminName := strings.TrimSpace(in.AdminName)
	if adminName == "" || len(adminName) > 200 {
		v.Add("admin_name", "is required (at most 200 characters)")
	}
	password, generated := in.AdminPassword, ""
	if password == "" {
		p, err := secure.RandomPassword()
		if err != nil {
			return nil, err
		}
		password, generated = p, p
	} else if err := secure.CheckPasswordPolicy(password, adminEmail, adminName); err != nil {
		v.Add("admin_password", "%s", err.Error())
	}
	if err := v.Err(); err != nil {
		return nil, invalid(v)
	}

	hash, err := r.hasher.Hash(password)
	if err != nil {
		return nil, err
	}
	now := r.now()
	t := &model.Tenant{
		ID: model.MustNewID(), Slug: slug, Name: name, Status: model.TenantActive, Timezone: tz,
		PracticeNumber: strings.TrimSpace(in.PracticeNumber), Address: strings.TrimSpace(in.Address),
		Phone: practicePhone, Email: normaliseEmail(in.Email), CreatedAt: now, UpdatedAt: now,
	}
	admin := &model.User{
		ID: model.MustNewID(), TenantID: t.ID, Email: adminEmail, FullName: adminName, PasswordHash: hash,
		Role: model.RoleClinicAdmin, Status: model.UserActive, CreatedAt: now, UpdatedAt: now,
	}

	tx, err := r.platform.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if err := r.platform.CreateTenant(ctx, tx, t); err != nil {
		if errors.Is(err, repository.ErrDuplicate) {
			return nil, ErrSlugTaken
		}
		return nil, err
	}
	scope, err := tenant.NewScope(r.db, t.ID)
	if err != nil {
		return nil, err
	}
	if err := repository.NewUserRepository(scope.WithTx(tx)).Create(ctx, admin, nil); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	r.recordFor(ctx, t.ID, admin, RequestMeta{}, model.AuditEntry{
		Action: model.ActionTenantCreated, Outcome: model.OutcomeSuccess,
		Metadata: map[string]any{"slug": slug, "name": name, "via": "provisioning"},
	})
	return &ProvisionResult{Tenant: t, Admin: admin, GeneratedPassword: generated}, nil
}

// ---------------------------------------------------------------------------
// The signed-in session
// ---------------------------------------------------------------------------

// SessionService covers what a signed-in person does with their own session.
type SessionService struct{ *base }

// MeResult describes the signed-in person.
type MeResult struct {
	User   *model.User
	Tenant *model.Tenant
	State  SessionState
	MFA    MFAStatus
}

func (s *SessionService) Me(ctx context.Context) (*MeResult, error) {
	u, err := s.users().GetByID(ctx, s.actor.UserID)
	if err != nil {
		return nil, fromRepo(err)
	}
	t, err := s.reg.platform.TenantByID(ctx, s.scope.TenantID())
	if err != nil {
		return nil, fromRepo(err)
	}
	status, err := (&MFAService{base: s.base}).statusFor(ctx, u)
	if err != nil {
		return nil, err
	}
	return &MeResult{User: u, Tenant: t, State: s.state, MFA: *status}, nil
}

// Logout revokes the current session.
func (s *SessionService) Logout(ctx context.Context) error {
	if err := s.sessions().Revoke(ctx, s.actor.SessionID, s.now(), "logout"); err != nil {
		return err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionLogout, Outcome: model.OutcomeSuccess,
		SubjectType: "user", SubjectID: idp(s.actor.UserID)})
	return nil
}

// ChangePassword replaces the signed-in user's password and signs out every
// other session they hold: if the old password leaked, so may have they.
func (s *SessionService) ChangePassword(ctx context.Context, current, next string) error {
	users := s.users()
	u, err := users.GetByID(ctx, s.actor.UserID)
	if err != nil {
		return fromRepo(err)
	}
	ok, _, err := s.reg.hasher.Verify(u.PasswordHash, current)
	if err != nil {
		return err
	}
	now := s.now()
	if !ok {
		locked, err := users.RecordFailedAttempt(ctx, u.ID, now, s.reg.cfg.MaxLoginAttempts, s.reg.cfg.Lockout)
		if err != nil {
			return err
		}
		if locked {
			s.reg.auditAccountLocked(ctx, s.scope.TenantID(), u, RequestMeta{RequestID: s.actor.RequestID, IP: s.actor.IP}, now)
			_, _ = s.sessions().RevokeAllForUser(ctx, u.ID, now, "locked")
		}
		return ErrWrongPassword
	}
	if err := secure.CheckPasswordPolicy(next, u.Email, u.FullName); err != nil {
		return invalidField("new_password", err.Error())
	}
	if current == next {
		return invalidField("new_password", "must be different from the current password")
	}
	hash, err := s.reg.hasher.Hash(next)
	if err != nil {
		return err
	}
	if err := users.UpdatePasswordHash(ctx, u.ID, hash, now); err != nil {
		return fromRepo(err)
	}
	n, err := s.sessions().RevokeOthersForUser(ctx, u.ID, s.actor.SessionID, now, "password_changed")
	if err != nil {
		return err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionPasswordChanged, Outcome: model.OutcomeSuccess,
		SubjectType: "user", SubjectID: idp(u.ID), Metadata: map[string]any{"other_sessions_revoked": n}})
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func normaliseEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func validEmail(s string) bool {
	if s == "" || len(s) > 255 || strings.ContainsAny(s, " \t\r\n<>") {
		return false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	return at > 0 && strings.Contains(s[at:], ".")
}
