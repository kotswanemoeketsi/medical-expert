//go:build integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/medical-expert-system/internal/db"
	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/secure"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// These tests need a real database: MES_TEST_DSN, e.g.
//   mes:secret@tcp(127.0.0.1:3306)/mes_test
// Every test creates its own practices with random slugs, so the database
// does not need resetting between runs.

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MES_TEST_DSN")
	if dsn == "" {
		t.Skip("MES_TEST_DSN not set")
	}
	ctx := context.Background()
	conn, err := db.Open(ctx, dsn, db.Options{MaxOpen: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := db.Migrate(ctx, conn, nil); err != nil {
		t.Fatal(err)
	}
	return conn
}

var now = time.Now().UTC().Truncate(time.Millisecond)

func newPractice(t *testing.T, conn *sql.DB) (*model.Tenant, *tenant.Scope) {
	t.Helper()
	ctx := context.Background()
	id := model.MustNewID()
	tn := &model.Tenant{
		ID: id, Slug: "t-" + strings.ToLower(id.String()[:12]), Name: "Practice " + id.String()[:4],
		Status: model.TenantActive, Timezone: "Africa/Johannesburg", CreatedAt: now, UpdatedAt: now,
	}
	platform := NewPlatformRepository(conn)
	tx, err := platform.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.CreateTenant(ctx, tx, tn); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	scope, err := tenant.NewScope(conn, id)
	if err != nil {
		t.Fatal(err)
	}
	return tn, scope
}

func newPatient(t *testing.T, scope *tenant.Scope, first, last string, dob model.Date) *model.Patient {
	t.Helper()
	p := &model.Patient{
		ID: model.MustNewID(), FirstName: first, LastName: last, DateOfBirth: dob, Sex: model.SexFemale,
		PreferredChannel: model.ChannelNone, SmokingStatus: model.SmokingUnknown, AlcoholUse: model.AlcoholUnknown,
		Status: model.PatientActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := NewPatientRepository(scope).Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func newUser(t *testing.T, scope *tenant.Scope, role model.Role) *model.User {
	t.Helper()
	id := model.MustNewID()
	u := &model.User{
		ID: id, Email: fmt.Sprintf("u-%s@example.test", id.String()[:8]), FullName: "Test " + string(role),
		PasswordHash: "pbkdf2_sha256$1$c2FsdA$aGFzaA", Role: role, Status: model.UserActive,
		CreatedAt: now, UpdatedAt: now,
	}
	if role.IsClinical() {
		u.ProfessionalRegNo = "MP0123456"
	}
	if err := NewUserRepository(scope).Create(context.Background(), u, nil); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestScopedReadsNeverCrossTenants(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scopeA := newPractice(t, conn)
	_, scopeB := newPractice(t, conn)
	pa := newPatient(t, scopeA, "Thabo", "Nkosi", model.NewDate(1970, 5, 4))
	doctorB := newUser(t, scopeB, model.RoleDoctor)

	repoB := NewPatientRepository(scopeB)
	if _, err := repoB.Get(ctx, pa.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get across tenants: got %v, want ErrNotFound", err)
	}
	if ok, err := repoB.Exists(ctx, pa.ID); err != nil || ok {
		t.Errorf("Exists across tenants: %v %v", ok, err)
	}
	list, err := repoB.Search(ctx, "", "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.ID == pa.ID {
			t.Error("practice B's patient list includes practice A's patient")
		}
	}
	found, _ := repoB.Search(ctx, "Nkosi", "", 100, 0)
	for _, p := range found {
		if p.ID == pa.ID {
			t.Error("practice B's search found practice A's patient")
		}
	}
	dups, _ := repoB.FindDuplicates(ctx, pa)
	if len(dups) != 0 {
		t.Errorf("duplicate check in B matched A's patient: %d candidates", len(dups))
	}
	months := 6
	if err := repoB.SetReviewInterval(ctx, pa.ID, &months, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetReviewInterval across tenants: got %v", err)
	}
	pa2 := *pa
	pa2.FirstName = "Hijacked"
	if err := repoB.Update(ctx, &pa2); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update across tenants: got %v", err)
	}
	// The foreign key is (tenant_id, patient_id), so the database itself
	// refuses a child row pointing into another practice.
	err = repoB.AddAllergy(ctx, &model.Allergy{
		ID: model.MustNewID(), PatientID: pa.ID, Substance: "Penicillin", Severity: model.SeveritySevere,
		Status: model.ClinicalActive, RecordedBy: doctorB.ID, RecordedAt: now,
	})
	if !errors.Is(err, ErrReference) {
		t.Errorf("allergy for another practice's patient: got %v, want ErrReference", err)
	}
	err = repoB.AddConsent(ctx, &model.Consent{
		ID: model.MustNewID(), PatientID: pa.ID, Purpose: model.ConsentAIAssistedReview, Granted: true,
		Method: model.ConsentWritten, RecordedBy: doctorB.ID, RecordedAt: now,
	})
	if !errors.Is(err, ErrReference) {
		t.Errorf("consent for another practice's patient: got %v, want ErrReference", err)
	}
	// And A's record is untouched.
	got, err := NewPatientRepository(scopeA).Get(ctx, pa.ID)
	if err != nil || got.FirstName != "Thabo" || got.ReviewIntervalMonths != nil {
		t.Errorf("A's patient was modified: %+v %v", got, err)
	}
	if _, err := NewUserRepository(scopeA).GetByID(ctx, doctorB.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("user lookup across tenants: got %v", err)
	}
}

func TestNationalIDIsUniquePerPracticeNotGlobally(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scopeA := newPractice(t, conn)
	_, scopeB := newPractice(t, conn)
	mk := func(scope *tenant.Scope) error {
		p := &model.Patient{
			ID: model.MustNewID(), FirstName: "Lerato", LastName: "Dlamini", DateOfBirth: model.NewDate(1980, 1, 1),
			Sex: model.SexFemale, IDType: model.IDTypeSAID, NationalID: "8001015009087",
			PreferredChannel: model.ChannelNone, SmokingStatus: model.SmokingUnknown, AlcoholUse: model.AlcoholUnknown,
			Status: model.PatientActive, CreatedAt: now, UpdatedAt: now,
		}
		return NewPatientRepository(scope).Create(ctx, p)
	}
	if err := mk(scopeA); err != nil {
		t.Fatal(err)
	}
	if err := mk(scopeB); err != nil {
		t.Fatalf("the same person at a second practice must be allowed: %v", err)
	}
	err := mk(scopeA)
	var dup *DuplicateKeyError
	if !errors.As(err, &dup) || dup.Key != "uq_patients_national_id" {
		t.Fatalf("second record with the same ID number at one practice: got %v", err)
	}
}

func TestDuplicateDetectionRanksMatches(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	repo := NewPatientRepository(scope)
	existing := newPatient(t, scope, "Zoë", "Nkosi", model.NewDate(1990, 3, 12))
	existing.Mobile = "+27821234567"
	existing.UpdatedAt = now
	if err := repo.Update(ctx, existing); err != nil {
		t.Fatal(err)
	}

	candidate := &model.Patient{FirstName: "ZOE", LastName: "nkosi", DateOfBirth: model.NewDate(1990, 3, 12)}
	dups, err := repo.FindDuplicates(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(dups) != 1 || dups[0].Match != MatchNameAndDOB {
		t.Fatalf("case- and accent-insensitive name+DOB match expected, got %+v", dups)
	}

	candidate = &model.Patient{FirstName: "Nomsa", LastName: "Nkosi", DateOfBirth: model.NewDate(2001, 1, 1), Mobile: "+27821234567"}
	dups, _ = repo.FindDuplicates(ctx, candidate)
	if len(dups) != 1 || dups[0].Match != MatchMobileSurname {
		t.Fatalf("mobile+surname match expected, got %+v", dups)
	}

	candidate = &model.Patient{FirstName: "Someone", LastName: "Else", DateOfBirth: model.NewDate(2001, 1, 1)}
	dups, _ = repo.FindDuplicates(ctx, candidate)
	if len(dups) != 0 {
		t.Fatalf("unrelated patient matched: %+v", dups)
	}
}

func TestReplayGuardLivesInTheWhereClause(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	users := NewUserRepository(scope)
	u := newUser(t, scope, model.RoleDoctor)

	if err := users.SetMFASecret(ctx, u.ID, []byte("JBSWY3DPEHPK3PXP"), now); err != nil {
		t.Fatal(err)
	}
	if err := users.EnableMFA(ctx, u.ID, 100, now); err != nil {
		t.Fatal(err)
	}
	if err := users.EnableMFA(ctx, u.ID, 100, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("enabling twice: got %v, want ErrConflict", err)
	}
	if err := users.SetMFASecret(ctx, u.ID, []byte("AAAA"), now); !errors.Is(err, ErrConflict) {
		t.Fatalf("replacing a live secret must be refused, got %v", err)
	}
	if err := users.AdvanceMFACounter(ctx, u.ID, 100); !errors.Is(err, ErrConflict) {
		t.Fatalf("the enrolment code's counter was accepted again: %v", err)
	}
	if err := users.AdvanceMFACounter(ctx, u.ID, 101); err != nil {
		t.Fatalf("next counter refused: %v", err)
	}
	if err := users.AdvanceMFACounter(ctx, u.ID, 101); !errors.Is(err, ErrConflict) {
		t.Fatalf("same counter accepted twice: %v", err)
	}
	if err := users.AdvanceMFACounter(ctx, u.ID, 99); !errors.Is(err, ErrConflict) {
		t.Fatalf("older counter accepted: %v", err)
	}
	if err := users.ResetMFA(ctx, u.ID, now); err != nil {
		t.Fatal(err)
	}
	after, _ := users.GetByID(ctx, u.ID)
	if after.MFAEnabled || after.MFASecret != nil || after.MFALastCounter != nil {
		t.Fatalf("reset left factor state behind: %+v", after)
	}
}

func TestRecoveryCodesAreSingleUse(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	users := NewUserRepository(scope)
	u := newUser(t, scope, model.RoleNurse)

	codes, _ := secure.NewRecoveryCodes()
	hashes := make([][32]byte, len(codes))
	for i, c := range codes {
		hashes[i] = secure.HashRecoveryCode(c)
	}
	if err := users.ReplaceRecoveryCodes(ctx, u.ID, hashes, now); err != nil {
		t.Fatal(err)
	}
	if n, _ := users.CountUnusedRecoveryCodes(ctx, u.ID); n != 10 {
		t.Fatalf("expected 10 unused codes, got %d", n)
	}
	h := secure.HashRecoveryCode(strings.ToLower(codes[3]))
	if err := users.UseRecoveryCode(ctx, u.ID, h, now); err != nil {
		t.Fatalf("first use refused: %v", err)
	}
	if err := users.UseRecoveryCode(ctx, u.ID, h, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("second use: got %v, want ErrConflict", err)
	}
	if n, _ := users.CountUnusedRecoveryCodes(ctx, u.ID); n != 9 {
		t.Fatalf("expected 9 unused codes, got %d", n)
	}
	// Issuing a new set invalidates the old one.
	fresh, _ := secure.NewRecoveryCodes()
	freshHashes := make([][32]byte, len(fresh))
	for i, c := range fresh {
		freshHashes[i] = secure.HashRecoveryCode(c)
	}
	if err := users.ReplaceRecoveryCodes(ctx, u.ID, freshHashes, now); err != nil {
		t.Fatal(err)
	}
	if err := users.UseRecoveryCode(ctx, u.ID, secure.HashRecoveryCode(codes[5]), now); !errors.Is(err, ErrConflict) {
		t.Fatalf("an old code still works after regeneration: %v", err)
	}
}

func TestLockoutCountsAndLapses(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	users := NewUserRepository(scope)
	u := newUser(t, scope, model.RoleDoctor)

	for i := 1; i <= 4; i++ {
		locked, err := users.RecordFailedAttempt(ctx, u.ID, now, 5, 15*time.Minute)
		if err != nil || locked {
			t.Fatalf("attempt %d: locked=%v err=%v", i, locked, err)
		}
	}
	locked, err := users.RecordFailedAttempt(ctx, u.ID, now, 5, 15*time.Minute)
	if err != nil || !locked {
		t.Fatalf("fifth failure should lock: locked=%v err=%v", locked, err)
	}
	got, _ := users.GetByID(ctx, u.ID)
	if !got.IsLocked(now) {
		t.Fatal("account not locked")
	}
	// After the lock lapses, one more mistake starts the count again rather
	// than re-locking immediately.
	later := now.Add(16 * time.Minute)
	locked, err = users.RecordFailedAttempt(ctx, u.ID, later, 5, 15*time.Minute)
	if err != nil || locked {
		t.Fatalf("first failure after the lock lapsed re-locked: locked=%v err=%v", locked, err)
	}
	got, _ = users.GetByID(ctx, u.ID)
	if got.FailedAttempts != 1 {
		t.Fatalf("count should restart at 1, got %d", got.FailedAttempts)
	}
	if err := users.RecordSuccessfulLogin(ctx, u.ID, later); err != nil {
		t.Fatal(err)
	}
	got, _ = users.GetByID(ctx, u.ID)
	if got.FailedAttempts != 0 || got.LockedUntil != nil || got.LastLoginAt == nil {
		t.Fatalf("successful login did not reset state: %+v", got)
	}
}

func TestResolveTokenHonoursEveryWayASessionEnds(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	tn, scope := newPractice(t, conn)
	platform := NewPlatformRepository(conn)
	sessions := NewSessionRepository(scope)
	users := NewUserRepository(scope)
	u := newUser(t, scope, model.RoleDoctor)

	mk := func() (model.Session, [32]byte) {
		_, digest, _ := secure.NewSessionToken()
		s := model.Session{
			ID: model.MustNewID(), UserID: u.ID, TokenHash: digest, IssuedAt: now, LastSeenAt: now,
			AbsoluteExpiry: now.Add(12 * time.Hour),
		}
		if err := sessions.Create(ctx, &s); err != nil {
			t.Fatal(err)
		}
		return s, digest
	}
	idle := 30 * time.Minute

	s, digest := mk()
	rs, err := platform.ResolveToken(ctx, digest, now, now.Add(-idle))
	if err != nil || rs.SessionID != s.ID || rs.TenantID != tn.ID || rs.Role != model.RoleDoctor {
		t.Fatalf("live session did not resolve: %+v %v", rs, err)
	}
	later := now.Add(31 * time.Minute)
	if _, err := platform.ResolveToken(ctx, digest, later, later.Add(-idle)); !errors.Is(err, ErrNotFound) {
		t.Errorf("idle session resolved: %v", err)
	}
	if _, err := platform.ResolveToken(ctx, digest, now.Add(13*time.Hour), now.Add(13*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("session past its absolute expiry resolved: %v", err)
	}
	if err := sessions.Revoke(ctx, s.ID, now, "logout"); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.ResolveToken(ctx, digest, now, now.Add(-idle)); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked session resolved: %v", err)
	}

	_, digest2 := mk()
	if err := users.UpdateStatus(ctx, u.ID, model.UserSuspended, now); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.ResolveToken(ctx, digest2, now, now.Add(-idle)); !errors.Is(err, ErrNotFound) {
		t.Errorf("suspended user's session resolved: %v", err)
	}
	if err := users.UpdateStatus(ctx, u.ID, model.UserActive, now); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.ResolveToken(ctx, digest2, now, now.Add(-idle)); err != nil {
		t.Fatalf("reactivated user's session should resolve again: %v", err)
	}
	if err := platform.SetTenantStatus(ctx, tn.ID, model.TenantSuspended, now); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.ResolveToken(ctx, digest2, now, now.Add(-idle)); !errors.Is(err, ErrNotFound) {
		t.Errorf("suspended practice's session resolved: %v", err)
	}
}

func TestPendingSessionCanOnlyBeConsumedOnce(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	sessions := NewSessionRepository(scope)
	u := newUser(t, scope, model.RoleNurse)
	_, digest, _ := secure.NewSessionToken()
	s := model.Session{ID: model.MustNewID(), UserID: u.ID, TokenHash: digest, MFAPending: true,
		IssuedAt: now, LastSeenAt: now, AbsoluteExpiry: now.Add(5 * time.Minute)}
	if err := sessions.Create(ctx, &s); err != nil {
		t.Fatal(err)
	}
	if err := sessions.ConsumePending(ctx, s.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := sessions.ConsumePending(ctx, s.ID, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending session consumed twice: %v", err)
	}
}

func TestConsentHistoryIsAppendOnlyAndLatestWins(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	repo := NewPatientRepository(scope)
	p := newPatient(t, scope, "Anele", "Zulu", model.NewDate(1985, 7, 7))
	doc := newUser(t, scope, model.RoleDoctor)

	state, err := repo.ConsentState(ctx, p.ID)
	if err != nil || state.Granted(model.ConsentAIAssistedReview) {
		t.Fatalf("no record must read as not granted: %v", err)
	}
	for _, granted := range []bool{true, false, true, false} {
		// Same millisecond on purpose: ordering must not depend on the clock.
		err := repo.AddConsent(ctx, &model.Consent{ID: model.MustNewID(), PatientID: p.ID,
			Purpose: model.ConsentAIAssistedReview, Granted: granted, Method: model.ConsentVerbal,
			RecordedBy: doc.ID, RecordedAt: now})
		if err != nil {
			t.Fatal(err)
		}
	}
	history, _ := repo.ConsentHistory(ctx, p.ID)
	if len(history) != 4 {
		t.Fatalf("history should keep every decision, got %d", len(history))
	}
	state, _ = repo.ConsentState(ctx, p.ID)
	if state.Granted(model.ConsentAIAssistedReview) {
		t.Fatal("the latest decision was a withdrawal, but consent reads as granted")
	}
}

func TestAuditReadsAreTenantScoped(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	tnA, scopeA := newPractice(t, conn)
	tnB, _ := newPractice(t, conn)
	audit := NewAuditRepository(conn)
	for _, tid := range []model.ID{tnA.ID, tnB.ID} {
		id := tid
		if err := audit.Record(ctx, model.AuditEntry{TenantID: &id, Action: model.ActionPatientRead,
			Outcome: model.OutcomeSuccess, Metadata: map[string]any{"tenant": id.String()}, OccurredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	// A platform event with no tenant at all must still be recordable.
	if err := audit.Record(ctx, model.AuditEntry{Action: model.ActionLoginFailed, Outcome: model.OutcomeFailure,
		Reason: "unknown_tenant", OccurredAt: now}); err != nil {
		t.Fatal(err)
	}
	recs, err := NewAuditReadRepository(scopeA).List(ctx, AuditFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Metadata["tenant"] != tnA.ID.String() {
		t.Fatalf("practice A should see exactly its own entry, got %+v", recs)
	}
}
