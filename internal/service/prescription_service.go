package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/repository"
)

// PrescriptionService issues and repeats prescriptions for chronic patients.
//
// What it enforces is administrative: who may prescribe, whether the patient
// is on the chronic register and due for review, and whether an
// authorisation still has repeats and time left. What it deliberately does
// NOT do is check interactions, allergies or appropriateness. A system that
// half-performs those checks is worse than one that does not: a doctor who
// has seen it catch one thing reasonably assumes it would catch another.
type PrescriptionService struct{ *base }

// PrescriptionNotice is returned with every script issued, so nobody reads
// the listed allergies as a check that was run.
const PrescriptionNotice = "No interaction, allergy or appropriateness checks were performed. " +
	"The patient's recorded allergies and conditions are shown for your information only."

// maxValidity is the longest an authorisation may run.
const maxValidityMonths = 6

// PrescriptionItemInput is one line: which regimen to copy, and how much.
type PrescriptionItemInput struct {
	RegimenID    model.ID
	Quantity     string
	Instructions string // empty: the regimen's own instructions
}

// PrescriptionInput authorises a script.
type PrescriptionInput struct {
	Items             []PrescriptionItemInput
	RepeatsAuthorised int
	ValidUntil        *model.Date // empty: six months from today
	OverrideReason    string
	Notes             string
}

// IssuedPrescription is a script plus what the prescriber should see.
type IssuedPrescription struct {
	Prescription       *model.Prescription
	RecordedAllergies  []model.Allergy
	RecordedConditions []model.Condition
	Notice             string
}

// ReviewOverdueError says when the patient was last reviewed.
func reviewOverdue(last *time.Time, months int) error {
	details := map[string]any{"review_interval_months": months, "last_review": nil}
	msg := fmt.Sprintf("This patient has not had a signed consultation in the last %d months. Give an override reason to prescribe anyway.", months)
	if last == nil {
		msg = "This patient has no signed consultation on record. Give an override reason to prescribe anyway."
	} else {
		details["last_review"] = last.UTC()
	}
	return &Error{Kind: KindConflict, Code: "review_overdue", Message: msg,
		Fields: []model.FieldError{{Field: "override_reason", Message: "is required while the patient is overdue for review"}}, Details: details}
}

// checkPatient enforces the chronic register and the review interval.
func (s *PrescriptionService) checkPatient(ctx context.Context, p *model.Patient, overrideReason string) error {
	chronic, err := s.patients().HasActiveChronicCondition(ctx, p.ID)
	if err != nil {
		return err
	}
	if !chronic {
		return ErrNotChronic
	}
	months := s.reg.cfg.DefaultReviewMonths
	if p.ReviewIntervalMonths != nil {
		months = *p.ReviewIntervalMonths
	}
	last, err := s.encounters().LastReview(ctx, p.ID)
	if err != nil {
		return err
	}
	overdue := last == nil || last.AddDate(0, months, 0).Before(s.now())
	if overdue && overrideReason == "" {
		return reviewOverdue(last, months)
	}
	return nil
}

// Issue authorises a new script, copying each item from the patient's
// regimen as it stands now.
func (s *PrescriptionService) Issue(ctx context.Context, patientID model.ID, in PrescriptionInput) (*IssuedPrescription, error) {
	if err := s.requireDoctor(); err != nil {
		return nil, err
	}
	today := s.today()
	in.OverrideReason, in.Notes = strings.TrimSpace(in.OverrideReason), strings.TrimSpace(in.Notes)
	var v model.ValidationErrors
	if len(in.Items) == 0 || len(in.Items) > 10 {
		v.Add("items", "a prescription has between 1 and 10 items")
	}
	if in.RepeatsAuthorised < 0 || in.RepeatsAuthorised > 5 {
		v.Add("repeats_authorised", "must be between 0 and 5")
	}
	limit := model.DateOf(today.Time().AddDate(0, maxValidityMonths, 0))
	validUntil := limit
	if in.ValidUntil != nil && !in.ValidUntil.IsZero() {
		validUntil = *in.ValidUntil
		if validUntil.Before(today) {
			v.Add("valid_until", "cannot be in the past")
		} else if validUntil.After(limit) {
			v.Add("valid_until", "cannot be more than %d months from today", maxValidityMonths)
		}
	}
	if len(in.OverrideReason) > 500 {
		v.Add("override_reason", "must be at most 500 characters")
	}
	if len(in.Notes) > 1000 {
		v.Add("notes", "must be at most 1000 characters")
	}
	seen := map[model.ID]bool{}
	for i := range in.Items {
		it := &in.Items[i]
		it.Quantity, it.Instructions = collapseSpaces(it.Quantity), strings.TrimSpace(it.Instructions)
		if it.Quantity == "" || len(it.Quantity) > 50 {
			v.Add(fmt.Sprintf("items[%d].quantity", i), "is required (at most 50 characters), e.g. \"60 tablets\"")
		}
		if len(it.Instructions) > 500 {
			v.Add(fmt.Sprintf("items[%d].instructions", i), "must be at most 500 characters")
		}
		if seen[it.RegimenID] {
			v.Add(fmt.Sprintf("items[%d].regimen_id", i), "appears twice")
		}
		seen[it.RegimenID] = true
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}

	patient, err := s.patients().Get(ctx, patientID)
	if err != nil {
		return nil, fromRepo(err)
	}
	if err := s.checkPatient(ctx, patient, in.OverrideReason); err != nil {
		return nil, err
	}

	now := s.now()
	p := &model.Prescription{
		ID: model.MustNewID(), PatientID: patientID, PrescriberID: s.actor.UserID, IssuedAt: now,
		ValidUntil: validUntil, RepeatsAuthorised: in.RepeatsAuthorised, Status: model.PrescriptionActive,
		OverrideReason: in.OverrideReason, Notes: in.Notes, CreatedAt: now,
	}
	for i, it := range in.Items {
		g, err := s.regimens().Get(ctx, it.RegimenID)
		if errors.Is(err, repository.ErrNotFound) || (err == nil && g.PatientID != patientID) {
			v.Add(fmt.Sprintf("items[%d].regimen_id", i), "is not one of this patient's regimens")
			continue
		}
		if err != nil {
			return nil, err
		}
		if g.Status != model.RegimenActive {
			v.Add(fmt.Sprintf("items[%d].regimen_id", i), "is not active")
			continue
		}
		instructions := it.Instructions
		if instructions == "" {
			instructions = g.Instructions
		}
		regimenID, medicationID := g.ID, g.MedicationID
		p.Items = append(p.Items, model.PrescriptionItem{
			ID: model.MustNewID(), LineNo: i + 1, RegimenID: &regimenID, MedicationID: &medicationID,
			MedicationName: g.MedicationName, Dose: g.Dose, Route: string(g.Route), Frequency: g.Frequency.Label(),
			Quantity: it.Quantity, Instructions: instructions,
		})
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	if err := s.prescriptions().Create(ctx, p); err != nil {
		return nil, fromRepo(err)
	}
	meta := map[string]any{"prescription_id": p.ID.String(), "items": len(p.Items), "repeats": p.RepeatsAuthorised}
	if p.OverrideReason != "" {
		meta["override_reason"] = p.OverrideReason
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionPrescriptionIssued, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID), Metadata: meta})
	return s.issued(ctx, p.ID)
}

// Repeat issues the next repeat against an authorisation: the same items,
// the same expiry, one fewer repeat left. The bounds check happens in the
// database, so two people clicking at once cannot both succeed.
func (s *PrescriptionService) Repeat(ctx context.Context, id model.ID, overrideReason string) (*IssuedPrescription, error) {
	if err := s.requireDoctor(); err != nil {
		return nil, err
	}
	overrideReason = strings.TrimSpace(overrideReason)
	if len(overrideReason) > 500 {
		return nil, invalidField("override_reason", "must be at most 500 characters")
	}
	repo := s.prescriptions()
	root, err := repo.Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if root.RepeatOfID != nil {
		if root, err = repo.Get(ctx, *root.RepeatOfID); err != nil {
			return nil, fromRepo(err)
		}
	}
	today := s.today()
	if err := explainUnrepeatable(root, today); err != nil {
		return nil, err
	}
	patient, err := s.patients().Get(ctx, root.PatientID)
	if err != nil {
		return nil, fromRepo(err)
	}
	if err := s.checkPatient(ctx, patient, overrideReason); err != nil {
		return nil, err
	}
	now := s.now()
	repeat := &model.Prescription{
		ID: model.MustNewID(), PatientID: root.PatientID, PrescriberID: s.actor.UserID, RepeatOfID: &root.ID,
		IssuedAt: now, ValidUntil: root.ValidUntil, Status: model.PrescriptionActive,
		OverrideReason: overrideReason, Notes: root.Notes, CreatedAt: now,
	}
	for _, it := range root.Items {
		it.ID = model.MustNewID()
		repeat.Items = append(repeat.Items, it)
	}
	if err := repo.IssueRepeat(ctx, root.ID, today, repeat); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			// Someone else got there first, or the state changed since the
			// read above; re-read to say which.
			if fresh, gerr := repo.Get(ctx, root.ID); gerr == nil {
				if e := explainUnrepeatable(fresh, today); e != nil {
					return nil, e
				}
			}
			return nil, ErrRepeatsExhausted
		}
		return nil, fromRepo(err)
	}
	meta := map[string]any{"prescription_id": repeat.ID.String(), "repeat_of": root.ID.String(),
		"repeat_number": root.RepeatsUsed + 1, "of": root.RepeatsAuthorised}
	if overrideReason != "" {
		meta["override_reason"] = overrideReason
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionPrescriptionRepeated, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(root.PatientID), Metadata: meta})
	return s.issued(ctx, repeat.ID)
}

func explainUnrepeatable(root *model.Prescription, today model.Date) error {
	switch {
	case root.Status == model.PrescriptionCancelled:
		return ErrPrescriptionCancelled
	case root.Status == model.PrescriptionExpired || root.ValidUntil.Before(today):
		return ErrPrescriptionExpired
	case root.RepeatsUsed >= root.RepeatsAuthorised:
		return ErrRepeatsExhausted
	}
	return nil
}

func (s *PrescriptionService) issued(ctx context.Context, id model.ID) (*IssuedPrescription, error) {
	p, err := s.prescriptions().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &IssuedPrescription{Prescription: p, Notice: PrescriptionNotice,
		RecordedAllergies: []model.Allergy{}, RecordedConditions: []model.Condition{}}
	allergies, err := s.patients().Allergies(ctx, p.PatientID)
	if err != nil {
		return nil, err
	}
	for _, a := range allergies {
		if a.Status == model.ClinicalActive {
			out.RecordedAllergies = append(out.RecordedAllergies, a)
		}
	}
	conditions, err := s.patients().Conditions(ctx, p.PatientID)
	if err != nil {
		return nil, err
	}
	for _, c := range conditions {
		if c.Status == model.ClinicalActive {
			out.RecordedConditions = append(out.RecordedConditions, c)
		}
	}
	return out, nil
}

// Cancel stops an authorisation; no further repeats can be issued from it.
func (s *PrescriptionService) Cancel(ctx context.Context, id model.ID, reason string) (*model.Prescription, error) {
	if err := s.requireDoctor(); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 255 {
		return nil, invalidField("reason", "is required (at most 255 characters)")
	}
	repo := s.prescriptions()
	p, err := repo.Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if err := repo.Cancel(ctx, id, reason, s.actor.UserID, s.now()); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, &Error{Kind: KindConflict, Code: "prescription_state", Message: "Only an active prescription can be cancelled."}
		}
		return nil, err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionPrescriptionCancelled, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(p.PatientID), Reason: reason,
		Metadata: map[string]any{"prescription_id": id.String()}})
	return repo.Get(ctx, id)
}

// Get returns one script. Nurses may read scripts to prepare them.
func (s *PrescriptionService) Get(ctx context.Context, id model.ID) (*model.Prescription, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	p, err := s.prescriptions().Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if err := s.readAudited(ctx, model.ActionPrescriptionRead, "patient", p.PatientID,
		map[string]any{"prescription_id": id.String()}); err != nil {
		return nil, err
	}
	return p, nil
}

// List returns a patient's scripts, newest first.
func (s *PrescriptionService) List(ctx context.Context, patientID model.ID) ([]model.Prescription, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	list, err := s.prescriptions().ForPatient(ctx, patientID, 100)
	if err != nil {
		return nil, err
	}
	if err := s.readAudited(ctx, model.ActionPrescriptionRead, "patient", patientID, map[string]any{"count": len(list)}); err != nil {
		return nil, err
	}
	return list, nil
}
