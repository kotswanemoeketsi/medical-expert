//go:build integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

func medicationID(t *testing.T, conn *sql.DB, name string) model.ID {
	t.Helper()
	meds, err := NewPlatformRepository(conn).SearchMedications(context.Background(), name, 1, 0)
	if err != nil || len(meds) == 0 {
		t.Fatalf("catalogue has no %s: %v", name, err)
	}
	return meds[0].ID
}

func newRegimen(t *testing.T, scope *tenant.Scope, conn *sql.DB, patientID, prescriber model.ID) *model.Regimen {
	t.Helper()
	g := &model.Regimen{
		ID: model.MustNewID(), PatientID: patientID, MedicationID: medicationID(t, conn, "Metformin"),
		Dose: "1 tablet", Route: "oral", Frequency: model.FreqTwiceDaily, StartDate: model.DateOf(now),
		Status: model.RegimenActive, PrescribedBy: prescriber, CreatedAt: now, UpdatedAt: now,
	}
	if err := NewRegimenRepository(scope).Create(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	return g
}

func newScript(t *testing.T, scope *tenant.Scope, patientID, prescriber model.ID, repeats int, validUntil model.Date) *model.Prescription {
	t.Helper()
	p := &model.Prescription{
		ID: model.MustNewID(), PatientID: patientID, PrescriberID: prescriber, IssuedAt: now, ValidUntil: validUntil,
		RepeatsAuthorised: repeats, Status: model.PrescriptionActive, CreatedAt: now,
		Items: []model.PrescriptionItem{{ID: model.MustNewID(), LineNo: 1, MedicationName: "Metformin 500 mg tablet",
			Dose: "1 tablet", Route: "oral", Frequency: "twice daily", Quantity: "60 tablets"}},
	}
	if err := NewPrescriptionRepository(scope).Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func repeatOf(p *model.Prescription) *model.Prescription {
	r := *p
	r.ID, r.RepeatOfID, r.RepeatsAuthorised = model.MustNewID(), &p.ID, 0
	r.Items = []model.PrescriptionItem{p.Items[0]}
	r.Items[0].ID = model.MustNewID()
	return &r
}

func TestCatalogueListsStrengthsInNumericOrder(t *testing.T) {
	conn := openTestDB(t)
	meds, err := NewPlatformRepository(conn).SearchMedications(context.Background(), "Amlodipine", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]int{}
	for i, m := range meds {
		if m.BrandName == "" && m.DosageForm == "tablet" {
			at[m.Strength] = i + 1
		}
	}
	if at["5 mg"] == 0 || at["10 mg"] == 0 || at["5 mg"] > at["10 mg"] {
		t.Fatalf("want 5 mg listed before 10 mg, got positions %v", at)
	}
}

func TestStage2ChildRowsCannotPointIntoAnotherPractice(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scopeA := newPractice(t, conn)
	_, scopeB := newPractice(t, conn)
	pa := newPatient(t, scopeA, "Ayanda", "Cele", model.NewDate(1970, 1, 1))
	docB := newUser(t, scopeB, model.RoleDoctor)

	err := NewRegimenRepository(scopeB).Create(ctx, &model.Regimen{ID: model.MustNewID(), PatientID: pa.ID,
		MedicationID: medicationID(t, conn, "Amlodipine"), Dose: "1", Route: "oral", Frequency: model.FreqOnceDaily,
		StartDate: model.DateOf(now), Status: model.RegimenActive, PrescribedBy: docB.ID, CreatedAt: now, UpdatedAt: now})
	if !errors.Is(err, ErrReference) {
		t.Errorf("regimen for another practice's patient: %v", err)
	}
	err = NewEncounterRepository(scopeB).Create(ctx, &model.Encounter{ID: model.MustNewID(), PatientID: pa.ID,
		ClinicianID: docB.ID, Type: model.EncounterConsultation, EncounterAt: now, CreatedAt: now, UpdatedAt: now})
	if !errors.Is(err, ErrReference) {
		t.Errorf("encounter for another practice's patient: %v", err)
	}
	err = NewLabRepository(scopeB).Create(ctx, &model.LabResult{ID: model.MustNewID(), PatientID: pa.ID, LabName: "Lab",
		Panel: "HbA1c", ReportedAt: now, Status: model.LabFinal, RecordedBy: docB.ID, RecordedAt: now})
	if !errors.Is(err, ErrReference) {
		t.Errorf("lab result for another practice's patient: %v", err)
	}
	err = NewDocumentRepository(scopeB).Create(ctx, &model.ClinicalDocument{ID: model.MustNewID(), PatientID: pa.ID,
		Type: model.DocReferral, AuthorID: docB.ID, Body: []byte(`{}`), CreatedAt: now, UpdatedAt: now})
	if !errors.Is(err, ErrReference) {
		t.Errorf("document for another practice's patient: %v", err)
	}
	err = NewPrescriptionRepository(scopeB).Create(ctx, &model.Prescription{ID: model.MustNewID(), PatientID: pa.ID,
		PrescriberID: docB.ID, IssuedAt: now, ValidUntil: model.DateOf(now.AddDate(0, 1, 0)), CreatedAt: now})
	if !errors.Is(err, ErrReference) {
		t.Errorf("prescription for another practice's patient: %v", err)
	}
}

func TestDoseMustBelongToItsRegimensPatient(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	doc := newUser(t, scope, model.RoleDoctor)
	p1 := newPatient(t, scope, "One", "Patient", model.NewDate(1980, 1, 1))
	p2 := newPatient(t, scope, "Two", "Patient", model.NewDate(1981, 1, 1))
	g := newRegimen(t, scope, conn, p1.ID, doc.ID)

	// The composite foreign key (tenant, regimen, patient) refuses a dose
	// filed against the wrong patient even within one practice.
	wrong := *g
	wrong.PatientID = p2.ID
	if _, err := NewRegimenRepository(scope).ScheduleDoses(ctx, &wrong, []time.Time{now.Add(time.Hour)}, now); !errors.Is(err, ErrReference) {
		t.Fatalf("dose filed against another patient: %v", err)
	}
	repo := NewRegimenRepository(scope)
	n, err := repo.ScheduleDoses(ctx, g, []time.Time{now.Add(time.Hour), now.Add(time.Hour), now.Add(13 * time.Hour)}, now)
	if err != nil || n != 2 {
		t.Fatalf("expected 2 doses (one duplicate skipped), got %d %v", n, err)
	}
	doses, _ := repo.Doses(ctx, p1.ID, now, now.Add(24*time.Hour))
	if len(doses) != 2 || doses[0].MedicationName != "Metformin 500 mg tablet" {
		t.Fatalf("unexpected doses: %+v", doses)
	}
	if err := repo.Respond(ctx, doses[0].ID, model.DoseTaken, model.ResponsePortal, nil, "", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Respond(ctx, doses[0].ID, model.DoseSkipped, model.ResponsePortal, nil, "", now); !errors.Is(err, ErrConflict) {
		t.Fatalf("a dose was answered twice: %v", err)
	}
}

func TestTheLastRepeatCanOnlyBeSpentOnce(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	doc := newUser(t, scope, model.RoleDoctor)
	p := newPatient(t, scope, "Sizwe", "Dube", model.NewDate(1960, 6, 6))
	orig := newScript(t, scope, p.ID, doc.ID, 1, model.DateOf(now.AddDate(0, 6, 0)))
	repo := NewPrescriptionRepository(scope)

	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = repo.IssueRepeat(ctx, orig.ID, model.DateOf(now), repeatOf(orig))
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrConflict):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("one repeat authorised, %d issued", ok)
	}
	got, _ := repo.Get(ctx, orig.ID)
	if got.RepeatsUsed != 1 || got.RepeatsRemaining() != 0 {
		t.Fatalf("repeats used %d", got.RepeatsUsed)
	}
	list, _ := repo.ForPatient(ctx, p.ID, 10)
	if len(list) != 2 || len(list[0].Items) != 1 || len(list[1].Items) != 1 {
		t.Fatalf("expected the original and one repeat, each with its item: %+v", list)
	}
}

func TestRepeatsRespectExpiryAndCancellation(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	doc := newUser(t, scope, model.RoleDoctor)
	p := newPatient(t, scope, "Zanele", "Mthembu", model.NewDate(1955, 5, 5))
	repo := NewPrescriptionRepository(scope)

	expired := newScript(t, scope, p.ID, doc.ID, 5, model.DateOf(now.AddDate(0, 0, -1)))
	if err := repo.IssueRepeat(ctx, expired.ID, model.DateOf(now), repeatOf(expired)); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeat of an expired script: %v", err)
	}
	cancelled := newScript(t, scope, p.ID, doc.ID, 5, model.DateOf(now.AddDate(0, 6, 0)))
	if err := repo.Cancel(ctx, cancelled.ID, "changed treatment", doc.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.IssueRepeat(ctx, cancelled.ID, model.DateOf(now), repeatOf(cancelled)); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeat of a cancelled script: %v", err)
	}
	if n, err := repo.ExpireBefore(ctx, model.DateOf(now)); err != nil || n != 1 {
		t.Fatalf("expected one script expired, got %d %v", n, err)
	}
}

func TestEncounterSigningAndAmendment(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	doc := newUser(t, scope, model.RoleDoctor)
	other := newUser(t, scope, model.RoleDoctor)
	p := newPatient(t, scope, "Musa", "Zwane", model.NewDate(1990, 9, 9))
	repo := NewEncounterRepository(scope)

	e := &model.Encounter{ID: model.MustNewID(), PatientID: p.ID, ClinicianID: doc.ID, Type: model.EncounterConsultation,
		EncounterAt: now, PresentingComplaint: "Cough", CreatedAt: now, UpdatedAt: now}
	if err := repo.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := repo.Sign(ctx, e.ID, doc.ID, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("signed without an assessment or plan: %v", err)
	}
	amend := &model.Amendment{ID: model.MustNewID(), EncounterID: e.ID, Field: model.FieldAssessment, NewValue: "x",
		Reason: "r", AmendedBy: doc.ID, AmendedAt: now}
	if err := repo.Amend(ctx, amend); !errors.Is(err, ErrConflict) {
		t.Fatalf("amended a draft: %v", err)
	}
	e.Assessment, e.UpdatedAt = "Viral upper respiratory tract infection", now
	if err := repo.UpdateDraft(ctx, e, other.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("someone else edited the draft: %v", err)
	}
	if err := repo.UpdateDraft(ctx, e, doc.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Sign(ctx, e.ID, other.ID, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("someone else signed the note: %v", err)
	}
	if err := repo.Sign(ctx, e.ID, doc.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateDraft(ctx, e, doc.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("a signed note was edited in place: %v", err)
	}
	amend.ID, amend.NewValue, amend.Reason, amend.AmendedBy = model.MustNewID(), "Acute bronchitis", "Chest findings reviewed", other.ID
	if err := repo.Amend(ctx, amend); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.Get(ctx, e.ID)
	if got.Assessment != "Acute bronchitis" || got.AmendedAt == nil || got.SignedAt == nil {
		t.Fatalf("amendment not applied: %+v", got)
	}
	list, _ := repo.Amendments(ctx, e.ID)
	if len(list) != 1 || list[0].PreviousValue != "Viral upper respiratory tract infection" || list[0].AmendedByName == "" {
		t.Fatalf("previous value not preserved: %+v", list)
	}
	last, _ := repo.LastReview(ctx, p.ID)
	if last == nil || !last.Equal(now) {
		t.Fatalf("last review should be the signed note's time, got %v", last)
	}
}

func TestLabInboxCarriesItemsCriticalFirst(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	doc := newUser(t, scope, model.RoleNurse)
	p := newPatient(t, scope, "Lwazi", "Ngcobo", model.NewDate(1977, 7, 7))
	repo := NewLabRepository(scope)

	mk := func(panel string, flag model.AbnormalFlag, reported time.Time) *model.LabResult {
		l := &model.LabResult{ID: model.MustNewID(), PatientID: p.ID, LabName: "Lancet", Panel: panel, ReportedAt: reported,
			Status: model.LabFinal, HasAbnormal: flag.IsAbnormal(), HasCritical: flag.IsCritical(), RecordedBy: doc.ID, RecordedAt: now,
			Items: []model.LabResultItem{
				{ID: model.MustNewID(), LineNo: 1, Analyte: panel + " A", Value: "1", AbnormalFlag: model.FlagNone},
				{ID: model.MustNewID(), LineNo: 2, Analyte: panel + " B", Value: "2", AbnormalFlag: flag},
			}}
		if err := repo.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	normal := mk("FBC", model.FlagNone, now.Add(-3*time.Hour))
	high := mk("HbA1c", model.FlagHigh, now.Add(-2*time.Hour))
	critical := mk("Potassium", model.FlagCriticalHigh, now.Add(-1*time.Hour))

	inbox, err := repo.Inbox(ctx, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 3 || inbox[0].ID != critical.ID || inbox[1].ID != high.ID || inbox[2].ID != normal.ID {
		t.Fatalf("inbox order wrong: %+v", inbox)
	}
	for _, l := range inbox {
		if len(l.Items) != 2 || l.PatientName != "Lwazi Ngcobo" {
			t.Fatalf("result %s came back without its analytes or patient: %+v", l.Panel, l)
		}
	}
	abnormal, _ := repo.Inbox(ctx, true, 10)
	if len(abnormal) != 2 {
		t.Fatalf("abnormal-only inbox: %d", len(abnormal))
	}
	if err := repo.Acknowledge(ctx, critical.ID, doc.ID, "Phoned patient", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Acknowledge(ctx, critical.ID, doc.ID, "again", now); !errors.Is(err, ErrConflict) {
		t.Fatalf("acknowledged twice: %v", err)
	}
	if n, _ := repo.UnacknowledgedAbnormal(ctx, p.ID); n != 1 {
		t.Fatalf("expected one unacknowledged abnormal result, got %d", n)
	}
	one, err := repo.Get(ctx, high.ID)
	if err != nil || len(one.Items) != 2 || one.Items[1].AbnormalFlag != model.FlagHigh {
		t.Fatalf("get: %+v %v", one, err)
	}
}

func TestDocumentNumbersArePerPracticeTypeAndYear(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scopeA := newPractice(t, conn)
	_, scopeB := newPractice(t, conn)
	issue := func(scope *tenant.Scope, typ model.DocumentType) string {
		t.Helper()
		doc := newUser(t, scope, model.RoleDoctor)
		p := newPatient(t, scope, "Doc", "Patient", model.NewDate(1985, 5, 5))
		d := &model.ClinicalDocument{ID: model.MustNewID(), PatientID: p.ID, Type: typ, AuthorID: doc.ID,
			Body: []byte(`{}`), CreatedAt: now, UpdatedAt: now}
		repo := NewDocumentRepository(scope)
		if err := repo.Create(ctx, d); err != nil {
			t.Fatal(err)
		}
		number, text, err := repo.Issue(ctx, d, doc.ID, now, 2026, func(n string) (string, error) { return "text " + n, nil })
		if err != nil || text != "text "+number {
			t.Fatalf("issue: %v", err)
		}
		if _, _, err := repo.Issue(ctx, d, doc.ID, now, 2026, func(n string) (string, error) { return n, nil }); !errors.Is(err, ErrConflict) {
			t.Fatalf("issued twice: %v", err)
		}
		return number
	}
	got := []string{issue(scopeA, model.DocReferral), issue(scopeA, model.DocReferral), issue(scopeA, model.DocSickNote), issue(scopeB, model.DocReferral)}
	want := []string{"REF-2026-0001", "REF-2026-0002", "MC-2026-0001", "REF-2026-0001"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("numbers %v, want %v", got, want)
	}

	// Concurrent issues never share a number.
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := issue(scopeB, model.DocSickNote)
			mu.Lock()
			seen[n] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 6 {
		t.Fatalf("concurrent issues produced duplicate numbers: %v", seen)
	}
	for n := range seen {
		if !strings.HasPrefix(n, "MC-2026-000") {
			t.Fatalf("unexpected number %s", n)
		}
	}
}

func TestDraftUsageCountsForTheDailyLimit(t *testing.T) {
	conn := openTestDB(t)
	ctx := context.Background()
	_, scope := newPractice(t, conn)
	doc := newUser(t, scope, model.RoleDoctor)
	p := newPatient(t, scope, "Ai", "Usage", model.NewDate(1999, 9, 9))
	repo := NewEncounterRepository(scope)
	e := &model.Encounter{ID: model.MustNewID(), PatientID: p.ID, ClinicianID: doc.ID, Type: model.EncounterConsultation,
		EncounterAt: now, CreatedAt: now, UpdatedAt: now}
	if err := repo.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	for _, status := range []model.NoteDraftStatus{model.DraftGenerated, model.DraftFailed} {
		d := &model.NoteDraft{ID: model.MustNewID(), EncounterID: e.ID, Dictation: "dictated", Model: "m", Status: status,
			CreatedBy: doc.ID, CreatedAt: now}
		if status == model.DraftGenerated {
			d.Draft = &model.DraftContent{Assessment: "a", Flags: []string{}}
		}
		if err := repo.SaveDraft(ctx, d, []byte(`{"age_years":27}`)); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := repo.DraftsSince(ctx, now.Add(-time.Minute)); n != 2 {
		t.Fatalf("expected both attempts counted, got %d", n)
	}
	drafts, _ := repo.Drafts(ctx, e.ID)
	if len(drafts) != 2 {
		t.Fatalf("drafts: %d", len(drafts))
	}
	if err := repo.FlagDraft(ctx, e.ID, drafts[0].ID, doc.ID, "added a finding I did not say", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.FlagDraft(ctx, e.ID, drafts[0].ID, doc.ID, "again", now); !errors.Is(err, ErrConflict) {
		t.Fatalf("flagged twice: %v", err)
	}
}
