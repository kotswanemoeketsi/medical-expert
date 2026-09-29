// Package service holds every decision the system makes.
//
// A service never imports net/http and never sees a request, a header or a
// status code. It takes input structs and returns *Error values. That keeps
// the rules testable without a server and reusable from the command line or
// a background job.
//
// Tenant scoping reaches the services through the Registry: per-request
// services come from Registry.For(scope, actor), and the only scope a
// request can obtain is the one its session resolved to.
package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yourorg/medical-expert-system/internal/ai"
	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/repository"
	"github.com/yourorg/medical-expert-system/internal/secure"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// Config holds the security rules services apply.
type Config struct {
	SessionIdle      time.Duration // BR-1.8
	SessionMax       time.Duration // absolute cap on a session
	MFAChallengeTTL  time.Duration // how long a password-only session waits for a code
	MaxLoginAttempts int           // BR-1.6
	Lockout          time.Duration
	RequireMFA       bool // BR-1.3
	MFAIssuer        string

	// AIDailyLimit caps note-drafting requests per practice per day
	// (BR-5.18, NFR-12).
	AIDailyLimit int
	// DefaultReviewMonths applies to chronic patients with no interval of
	// their own: a repeat prescription for a patient not seen in a signed
	// note within it needs an override reason.
	DefaultReviewMonths int
}

func (c Config) withDefaults() Config {
	if c.SessionIdle <= 0 {
		c.SessionIdle = 30 * time.Minute
	}
	if c.SessionMax <= 0 {
		c.SessionMax = 12 * time.Hour
	}
	if c.MFAChallengeTTL <= 0 {
		c.MFAChallengeTTL = 5 * time.Minute
	}
	if c.MaxLoginAttempts <= 0 {
		c.MaxLoginAttempts = 5
	}
	if c.Lockout <= 0 {
		c.Lockout = 15 * time.Minute
	}
	if c.MFAIssuer == "" {
		c.MFAIssuer = "Medical Expert System"
	}
	if c.AIDailyLimit <= 0 {
		c.AIDailyLimit = 200
	}
	if c.DefaultReviewMonths <= 0 {
		c.DefaultReviewMonths = 6
	}
	return c
}

// Deps are the process-wide dependencies.
type Deps struct {
	DB     *sql.DB
	Logger *slog.Logger
	Hasher secure.Hasher
	Config Config
	// Drafter drafts consultation notes. Nil switches drafting off.
	Drafter ai.Drafter
	// Now is the clock; tests replace it. Defaults to UTC wall time.
	Now func() time.Time
}

// Registry holds process-wide dependencies and builds per-request services.
type Registry struct {
	db        *sql.DB
	log       *slog.Logger
	hasher    secure.Hasher
	cfg       Config
	clock     func() time.Time
	platform  *repository.PlatformRepository
	audit     *repository.AuditRepository
	drafter   ai.Drafter
	dummyHash string
}

func NewRegistry(d Deps) (*Registry, error) {
	if d.DB == nil {
		return nil, errors.New("service: a database is required")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Hasher == nil {
		d.Hasher = secure.NewPBKDF2Hasher()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	// Verified against on unknown accounts, so a failed sign-in takes the
	// same time whether or not the account exists.
	dummy, err := d.Hasher.Hash("timing-equaliser-not-a-real-password")
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	return &Registry{
		db:        d.DB,
		log:       d.Logger,
		hasher:    d.Hasher,
		cfg:       d.Config.withDefaults(),
		clock:     d.Now,
		platform:  repository.NewPlatformRepository(d.DB),
		audit:     repository.NewAuditRepository(d.DB),
		drafter:   d.Drafter,
		dummyHash: dummy,
	}, nil
}

// now is UTC truncated to the database's millisecond precision, so a value
// written and read back compares equal.
func (r *Registry) now() time.Time { return r.clock().UTC().Truncate(time.Millisecond) }

// Config returns the effective configuration, defaults applied.
func (r *Registry) Config() Config { return r.cfg }

// Actor is whoever is making the request.
type Actor struct {
	UserID    model.ID
	Role      model.Role
	SessionID model.ID
	PatientID *model.ID // set for patient logins: the one record they may see
	RequestID string
	IP        string
	UserAgent string
}

// RequestMeta describes where a request came from, for audit.
type RequestMeta struct {
	RequestID string
	IP        string
	UserAgent string
}

// SessionState is how far a session has got through sign-in.
type SessionState string

const (
	// StatePending: password accepted, second factor outstanding.
	StatePending SessionState = "pending"
	// StateSetup: signed in, but the role requires a second factor and none
	// is enrolled yet, so enrolment is the only thing the session can do.
	StateSetup SessionState = "setup"
	// StateActive: fully signed in.
	StateActive SessionState = "active"
)

// Principal is an authenticated request: who, in which practice, how far
// through sign-in, and the services scoped to all of that.
type Principal struct {
	Actor    Actor
	TenantID model.ID
	State    SessionState
	Services *Services
}

// Services is the per-request, per-tenant set. There is no unscoped
// service to reach for.
type Services struct {
	Session       *SessionService
	MFA           *MFAService
	Users         *UserService
	Patients      *PatientService
	Audit         *AuditService
	Medication    *MedicationService
	Encounters    *EncounterService
	Prescriptions *PrescriptionService
	Labs          *LabService
	Documents     *DocumentService
}

// For builds services bound to one tenant and one actor.
func (r *Registry) For(scope *tenant.Scope, actor Actor, state SessionState) *Services {
	b := &base{reg: r, scope: scope, actor: actor, state: state}
	return &Services{
		Session:       &SessionService{base: b},
		MFA:           &MFAService{base: b},
		Users:         &UserService{base: b},
		Patients:      &PatientService{base: b},
		Audit:         &AuditService{base: b},
		Medication:    &MedicationService{base: b},
		Encounters:    &EncounterService{base: b},
		Prescriptions: &PrescriptionService{base: b},
		Labs:          &LabService{base: b},
		Documents:     &DocumentService{base: b},
	}
}

// touchInterval bounds how often a session's last_seen_at is written.
const touchInterval = time.Minute

// Authenticate resolves a bearer token to a principal. Every way a session
// can end — idle, expired, revoked, user suspended, practice suspended — is
// checked in the lookup itself.
func (r *Registry) Authenticate(ctx context.Context, token string, meta RequestMeta) (*Principal, error) {
	if token == "" {
		return nil, ErrUnauthenticated
	}
	now := r.now()
	rs, err := r.platform.ResolveToken(ctx, secure.HashToken(token), now, now.Add(-r.cfg.SessionIdle))
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	scope, err := tenant.NewScope(r.db, rs.TenantID)
	if err != nil {
		return nil, err
	}
	if now.Sub(rs.LastSeenAt) >= touchInterval {
		if err := repository.NewSessionRepository(scope).Touch(ctx, rs.SessionID, now); err != nil {
			r.log.WarnContext(ctx, "session touch failed", "err", err)
		}
	}
	state := StateActive
	switch {
	case rs.MFAPending:
		state = StatePending
	case r.cfg.RequireMFA && rs.Role.RequiresMFA() && !rs.MFAEnabled:
		state = StateSetup
	}
	actor := Actor{
		UserID: rs.UserID, Role: rs.Role, SessionID: rs.SessionID, PatientID: rs.PatientID,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}
	return &Principal{Actor: actor, TenantID: rs.TenantID, State: state, Services: r.For(scope, actor, state)}, nil
}

// Ping reports whether the database answers.
func (r *Registry) Ping(ctx context.Context) error { return r.platform.Ping(ctx) }

// ExpirePrescriptions marks lapsed scripts expired in every active
// practice, one practice at a time. A convenience for listings: repeats
// check the date themselves, so a missed run cannot cause harm.
func (r *Registry) ExpirePrescriptions(ctx context.Context) (int64, error) {
	ids, err := r.platform.ActiveTenantIDs(ctx)
	if err != nil {
		return 0, err
	}
	today := model.DateOf(r.now().In(johannesburg))
	var total int64
	for _, id := range ids {
		scope, err := tenant.NewScope(r.db, id)
		if err != nil {
			return total, err
		}
		n, err := repository.NewPrescriptionRepository(scope).ExpireBefore(ctx, today)
		if err != nil {
			return total, fmt.Errorf("tenant %s: %w", id, err)
		}
		total += n
	}
	return total, nil
}

// DraftingEnabled reports whether a note drafter is configured.
func (r *Registry) DraftingEnabled() bool { return r.drafter != nil }

// PurgeSessions deletes sessions that ended more than a week ago.
func (r *Registry) PurgeSessions(ctx context.Context) (int64, error) {
	return r.platform.PurgeSessions(ctx, r.now().Add(-7*24*time.Hour))
}

// base carries what every scoped service needs.
type base struct {
	reg   *Registry
	scope *tenant.Scope
	actor Actor
	state SessionState
}

func (b *base) now() time.Time { return b.reg.now() }

func (b *base) users() *repository.UserRepository { return repository.NewUserRepository(b.scope) }
func (b *base) sessions() *repository.SessionRepository {
	return repository.NewSessionRepository(b.scope)
}
func (b *base) patients() *repository.PatientRepository {
	return repository.NewPatientRepository(b.scope)
}
func (b *base) regimens() *repository.RegimenRepository {
	return repository.NewRegimenRepository(b.scope)
}
func (b *base) encounters() *repository.EncounterRepository {
	return repository.NewEncounterRepository(b.scope)
}
func (b *base) prescriptions() *repository.PrescriptionRepository {
	return repository.NewPrescriptionRepository(b.scope)
}
func (b *base) labs() *repository.LabRepository { return repository.NewLabRepository(b.scope) }
func (b *base) documents() *repository.DocumentRepository {
	return repository.NewDocumentRepository(b.scope)
}

// isOwnPatient reports whether the actor is the patient with this record.
func (b *base) isOwnPatient(id model.ID) bool {
	return b.actor.Role == model.RolePatient && b.actor.PatientID != nil && *b.actor.PatientID == id
}

// canSeePatientClinical allows clinicians, and a patient for their own
// record. Anyone else gets not-found, never forbidden.
func (b *base) canSeePatientClinical(ctx context.Context, id model.ID) error {
	if b.actor.Role.IsClinical() || b.isOwnPatient(id) {
		return nil
	}
	if b.actor.Role == model.RoleClinicAdmin {
		return ErrForbidden
	}
	b.recordWrite(ctx, model.AuditEntry{Action: model.ActionPatientAccessDenied, Outcome: model.OutcomeDenied,
		SubjectType: "patient", SubjectID: idp(id)})
	return ErrNotFound
}

// requirePatientExists returns not-found for a patient outside this
// practice, before any list query can answer with an empty list instead.
func (b *base) requirePatientExists(ctx context.Context, id model.ID) error {
	ok, err := b.patients().Exists(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// readAudited records a read of patient data; if the record cannot be
// written, the data is not returned.
func (b *base) readAudited(ctx context.Context, action, subjectType string, subjectID model.ID, meta map[string]any) error {
	return b.record(ctx, model.AuditEntry{Action: action, Outcome: model.OutcomeSuccess,
		SubjectType: subjectType, SubjectID: idp(subjectID), Metadata: meta})
}

// record writes an audit entry attributed to the actor. It returns the error
// so that reads of patient data can fail closed: if the access cannot be
// recorded, the data is not returned (NFR-11).
func (b *base) record(ctx context.Context, e model.AuditEntry) error {
	tid := b.scope.TenantID()
	e.TenantID = &tid
	if !b.actor.UserID.IsZero() {
		uid := b.actor.UserID
		e.ActorID = &uid
		e.ActorRole = string(b.actor.Role)
	}
	e.RequestID, e.IP = b.actor.RequestID, b.actor.IP
	if e.OccurredAt.IsZero() {
		e.OccurredAt = b.now()
	}
	if bg := b.scope.BreakGlass(); bg != nil {
		if e.Metadata == nil {
			e.Metadata = map[string]any{}
		}
		e.Metadata["break_glass_operator"] = bg.OperatorID.String()
		e.Metadata["break_glass_reason"] = bg.Reason
	}
	if err := b.reg.audit.Record(ctx, e); err != nil {
		b.reg.log.ErrorContext(ctx, "audit write failed", "action", e.Action, "err", err)
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

// recordWrite audits a change that has already been committed. A failure
// is logged loudly but not returned: failing the request now would invite a
// retry that repeats a change which already happened.
func (b *base) recordWrite(ctx context.Context, e model.AuditEntry) {
	_ = b.record(ctx, e)
}

func (b *base) requireStaff() error {
	if !b.actor.Role.IsStaff() {
		return ErrForbidden
	}
	return nil
}

func (b *base) requireClinical() error {
	if !b.actor.Role.IsClinical() {
		return ErrForbidden
	}
	return nil
}

func (b *base) requireDoctor() error {
	if b.actor.Role != model.RoleDoctor {
		return ErrDoctorOnly
	}
	return nil
}

func (b *base) requireAdmin() error {
	if !b.actor.Role.IsAdministrative() {
		return ErrForbidden
	}
	return nil
}

func idp(id model.ID) *model.ID { return &id }
