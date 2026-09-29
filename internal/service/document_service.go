package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/yourorg/medical-expert-system/internal/document"
	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/repository"
)

// DocumentService drafts, issues and cancels referral letters and medical
// certificates. Both leave the practice and are relied on by someone outside
// it, so the text is rendered once, at issue, and frozen.
type DocumentService struct{ *base }

// ReferralInput drafts a referral letter.
type ReferralInput struct {
	ToName            string
	ToSpeciality      string
	ToPractice        string
	Urgency           string // routine | urgent
	Reason            string
	ClinicalSummary   string
	IncludeMedication bool
	IncludeAllergies  bool
	EncounterID       *model.ID
}

// SickNoteInput drafts a medical certificate.
type SickNoteInput struct {
	Basis             model.CertificateBasis
	ExaminedOn        model.Date
	ValidFrom         model.Date
	ValidTo           model.Date
	Fitness           string // unfit | light_duties
	CertificateFor    string // work | school | other
	DiscloseDiagnosis bool
	Diagnosis         string
	BackdateReason    string
	EmployeeNumber    string
	Remarks           string
	EncounterID       *model.ID
}

func (s *DocumentService) checkEncounter(ctx context.Context, patientID model.ID, id *model.ID) error {
	if id == nil {
		return nil
	}
	e, err := s.encounters().Get(ctx, *id)
	if errors.Is(err, repository.ErrNotFound) || (err == nil && e.PatientID != patientID) {
		return invalidField("encounter_id", "is not a consultation for this patient")
	}
	return err
}

// CreateReferral drafts a referral letter.
func (s *DocumentService) CreateReferral(ctx context.Context, patientID model.ID, in ReferralInput) (*model.ClinicalDocument, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	in.ToName, in.ToSpeciality, in.ToPractice = collapseSpaces(in.ToName), collapseSpaces(in.ToSpeciality), collapseSpaces(in.ToPractice)
	in.Reason, in.ClinicalSummary = strings.TrimSpace(in.Reason), strings.TrimSpace(in.ClinicalSummary)
	in.Urgency = strings.ToLower(strings.TrimSpace(in.Urgency))
	var v model.ValidationErrors
	if in.ToName == "" || len(in.ToName) > 200 {
		v.Add("to_name", "is required (at most 200 characters)")
	}
	if len(in.ToSpeciality) > 100 {
		v.Add("to_speciality", "must be at most 100 characters")
	}
	if len(in.ToPractice) > 200 {
		v.Add("to_practice", "must be at most 200 characters")
	}
	if in.Urgency == "" {
		in.Urgency = "routine"
	}
	if in.Urgency != "routine" && in.Urgency != "urgent" {
		v.Add("urgency", "must be routine or urgent")
	}
	if in.Reason == "" || len(in.Reason) > 5000 {
		v.Add("reason", "is required (at most 5000 characters)")
	}
	if len(in.ClinicalSummary) > 10000 {
		v.Add("clinical_summary", "must be at most 10000 characters")
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	if err := s.checkEncounter(ctx, patientID, in.EncounterID); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(model.ReferralBody{
		ToName: in.ToName, ToSpeciality: in.ToSpeciality, ToPractice: in.ToPractice, Urgency: in.Urgency,
		Reason: in.Reason, ClinicalSummary: in.ClinicalSummary,
		IncludeMedication: in.IncludeMedication, IncludeAllergies: in.IncludeAllergies,
	})
	return s.createDraft(ctx, &model.ClinicalDocument{PatientID: patientID, Type: model.DocReferral,
		EncounterID: in.EncounterID, Body: body})
}

// CreateSickNote drafts a medical certificate. The basis must be stated,
// the diagnosis is left off unless the patient agreed to disclose it, and a
// certificate whose period starts before the consultation needs a reason.
func (s *DocumentService) CreateSickNote(ctx context.Context, patientID model.ID, in SickNoteInput) (*model.ClinicalDocument, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	in.Diagnosis, in.BackdateReason = strings.TrimSpace(in.Diagnosis), strings.TrimSpace(in.BackdateReason)
	in.EmployeeNumber, in.Remarks = strings.TrimSpace(in.EmployeeNumber), strings.TrimSpace(in.Remarks)
	var v model.ValidationErrors
	if !in.Basis.Valid() {
		v.Add("basis", "is required: examination, or patient_report when the certificate rests on what the patient said")
	}
	today := s.today()
	switch {
	case in.ExaminedOn.IsZero():
		v.Add("examined_on", "is required")
	case in.ExaminedOn.After(today):
		v.Add("examined_on", "cannot be in the future")
	}
	if in.ValidFrom.IsZero() {
		v.Add("valid_from", "is required")
	}
	if in.ValidTo.IsZero() {
		v.Add("valid_to", "is required")
	}
	if !in.ValidFrom.IsZero() && !in.ValidTo.IsZero() {
		if in.ValidTo.Before(in.ValidFrom) {
			v.Add("valid_to", "cannot be before valid_from")
		} else if in.ValidTo.Time().Sub(in.ValidFrom.Time()).Hours() > 365*24 {
			v.Add("valid_to", "the period is longer than a year; check the dates")
		}
	}
	if in.Fitness == "" {
		in.Fitness = "unfit"
	}
	if in.Fitness != "unfit" && in.Fitness != "light_duties" {
		v.Add("fitness", "must be unfit or light_duties")
	}
	if in.CertificateFor == "" {
		in.CertificateFor = "work"
	}
	if in.CertificateFor != "work" && in.CertificateFor != "school" && in.CertificateFor != "other" {
		v.Add("certificate_for", "must be work, school or other")
	}
	if in.DiscloseDiagnosis && (in.Diagnosis == "" || len(in.Diagnosis) > 500) {
		v.Add("diagnosis", "is required (at most 500 characters) when the patient agrees to disclose it")
	}
	if !in.DiscloseDiagnosis {
		// Not disclosed means not stored on the certificate at all.
		in.Diagnosis = ""
	}
	if len(in.EmployeeNumber) > 50 {
		v.Add("employee_number", "must be at most 50 characters")
	}
	if len(in.Remarks) > 1000 {
		v.Add("remarks", "must be at most 1000 characters")
	}
	backdated := !in.ValidFrom.IsZero() && !in.ExaminedOn.IsZero() && in.ValidFrom.Before(in.ExaminedOn)
	if !backdated {
		in.BackdateReason = ""
	}
	if len(in.BackdateReason) > 500 {
		v.Add("backdate_reason", "must be at most 500 characters")
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	if backdated && in.BackdateReason == "" {
		return nil, &Error{Kind: KindInvalid, Code: "backdate_reason_required",
			Message: "The certificate starts before the consultation. Say why, and the certificate will state that it is backdated.",
			Fields:  []model.FieldError{{Field: "backdate_reason", Message: "is required when valid_from is before examined_on"}}}
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	if err := s.checkEncounter(ctx, patientID, in.EncounterID); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(model.SickNoteBody{
		Basis: in.Basis, ExaminedOn: in.ExaminedOn, ValidFrom: in.ValidFrom, ValidTo: in.ValidTo,
		Fitness: in.Fitness, CertificateFor: in.CertificateFor, DiscloseDiagnosis: in.DiscloseDiagnosis,
		Diagnosis: in.Diagnosis, BackdateReason: in.BackdateReason, EmployeeNumber: in.EmployeeNumber, Remarks: in.Remarks,
	})
	examined, from, to := in.ExaminedOn, in.ValidFrom, in.ValidTo
	return s.createDraft(ctx, &model.ClinicalDocument{PatientID: patientID, Type: model.DocSickNote,
		EncounterID: in.EncounterID, Body: body, Basis: in.Basis, ExaminedOn: &examined, ValidFrom: &from, ValidTo: &to,
		DisclosesDiagnosis: in.DiscloseDiagnosis, BackdateReason: in.BackdateReason})
}

func (s *DocumentService) createDraft(ctx context.Context, d *model.ClinicalDocument) (*model.ClinicalDocument, error) {
	now := s.now()
	d.ID, d.AuthorID, d.Status, d.CreatedAt, d.UpdatedAt = model.MustNewID(), s.actor.UserID, model.DocumentDraft, now, now
	if err := s.documents().Create(ctx, d); err != nil {
		return nil, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionDocumentDrafted, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(d.PatientID),
		Metadata: map[string]any{"document_id": d.ID.String(), "type": string(d.Type)}})
	return s.documents().Get(ctx, d.ID)
}

// Issue numbers the document, renders it once and freezes it. The issuing
// doctor is the one who signs it.
func (s *DocumentService) Issue(ctx context.Context, id model.ID) (*model.ClinicalDocument, error) {
	if err := s.requireDoctor(); err != nil {
		return nil, err
	}
	repo := s.documents()
	d, err := repo.Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if d.Status != model.DocumentDraft {
		return nil, ErrDocumentState
	}
	tenant, err := s.reg.platform.TenantByID(ctx, s.scope.TenantID())
	if err != nil {
		return nil, err
	}
	issuer, err := s.users().GetByID(ctx, s.actor.UserID)
	if err != nil {
		return nil, fromRepo(err)
	}
	patient, err := s.patients().Get(ctx, d.PatientID)
	if err != nil {
		return nil, fromRepo(err)
	}
	now := s.now()
	today := s.today()
	practice := document.Practice{Name: tenant.Name, Address: tenant.Address, Phone: tenant.Phone,
		Email: tenant.Email, PracticeNumber: tenant.PracticeNumber}
	signer := document.Practitioner{Name: issuer.FullName, RegistrationNumber: issuer.ProfessionalRegNo}
	pat := document.Patient{FullName: patient.FullName(), DateOfBirth: patient.DateOfBirth, Sex: patient.Sex,
		IDType: patient.IDType, IDNumber: patient.NationalID, MedicalAidName: patient.MedicalAidName,
		MedicalAidNumber: patient.MedicalAidNumber, AgeYears: patient.AgeAt(now)}

	var render func(number string) (string, error)
	switch d.Type {
	case model.DocReferral:
		var body model.ReferralBody
		if err := json.Unmarshal(d.Body, &body); err != nil {
			return nil, err
		}
		data := document.ReferralData{Practice: practice, Author: signer, Patient: pat, IssuedOn: today, Body: body}
		// Only what the referring clinician chose to include is even read.
		if body.IncludeMedication {
			regimens, err := s.regimens().Current(ctx, d.PatientID)
			if err != nil {
				return nil, err
			}
			for _, r := range regimens {
				if r.Status == model.RegimenActive {
					data.Medication = append(data.Medication, r.Describe())
				}
			}
		}
		if body.IncludeAllergies {
			allergies, err := s.patients().Allergies(ctx, d.PatientID)
			if err != nil {
				return nil, err
			}
			for _, a := range allergies {
				if a.Status == model.ClinicalActive {
					line := a.Substance
					if a.Reaction != "" {
						line += ": " + a.Reaction
					}
					data.Allergies = append(data.Allergies, line+" ("+strings.ReplaceAll(string(a.Severity), "_", " ")+")")
				}
			}
		}
		render = func(number string) (string, error) { data.Number = number; return document.Referral(data), nil }
	case model.DocSickNote:
		var body model.SickNoteBody
		if err := json.Unmarshal(d.Body, &body); err != nil {
			return nil, err
		}
		data := document.SickNoteData{Practice: practice, Practitioner: signer, Patient: pat, IssuedOn: today, Body: body}
		render = func(number string) (string, error) { data.Number = number; return document.SickNote(data), nil }
	default:
		return nil, ErrDocumentState
	}

	number, _, err := repo.Issue(ctx, d, s.actor.UserID, now, today.Year, render)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrDocumentState
		}
		return nil, err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionDocumentIssued, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(d.PatientID),
		Metadata: map[string]any{"document_id": id.String(), "number": number, "type": string(d.Type)}})
	return repo.Get(ctx, id)
}

// Cancel withdraws a document. An issued one keeps its number and text,
// and the record shows that it was withdrawn and why.
func (s *DocumentService) Cancel(ctx context.Context, id model.ID, reason string) (*model.ClinicalDocument, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 255 {
		return nil, invalidField("reason", "must be at most 255 characters")
	}
	repo := s.documents()
	d, err := repo.Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	switch d.Status {
	case model.DocumentDraft:
	case model.DocumentIssued:
		if reason == "" {
			return nil, invalidField("reason", "is required to withdraw an issued document")
		}
	default:
		return nil, ErrDocumentState
	}
	if err := repo.Cancel(ctx, id, d.Status, reason, s.actor.UserID, s.now()); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrDocumentState
		}
		return nil, err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionDocumentCancelled, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(d.PatientID), Reason: reason,
		Metadata: map[string]any{"document_id": id.String(), "was": string(d.Status), "number": d.Number}})
	return repo.Get(ctx, id)
}

// Get returns one document.
func (s *DocumentService) Get(ctx context.Context, id model.ID) (*model.ClinicalDocument, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	d, err := s.documents().Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if err := s.readAudited(ctx, model.ActionDocumentRead, "patient", d.PatientID, map[string]any{"document_id": id.String()}); err != nil {
		return nil, err
	}
	return d, nil
}

// ForPatient lists a patient's documents, newest first.
func (s *DocumentService) ForPatient(ctx context.Context, patientID model.ID) ([]model.ClinicalDocument, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	list, err := s.documents().ForPatient(ctx, patientID, 100)
	if err != nil {
		return nil, err
	}
	if err := s.readAudited(ctx, model.ActionDocumentRead, "patient", patientID, map[string]any{"count": len(list)}); err != nil {
		return nil, err
	}
	return list, nil
}
