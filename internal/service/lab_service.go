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

// LabService captures laboratory results by hand and tracks whether a
// clinician has seen them. Values, units, ranges and flags are stored as the
// laboratory reported them; nothing is derived.
type LabService struct{ *base }

// LabItemInput is one analyte as printed on the report.
type LabItemInput struct {
	Analyte        string
	Value          string
	Unit           string
	ReferenceRange string
	AbnormalFlag   model.AbnormalFlag
}

// LabResultInput is one report.
type LabResultInput struct {
	LabName             string
	LabReference        string
	Panel               string
	SpecimenCollectedAt *time.Time
	ReportedAt          *time.Time // empty: now
	Status              model.LabStatus
	Comment             string
	Items               []LabItemInput
}

func (in *LabResultInput) validate(now time.Time) model.ValidationErrors {
	var v model.ValidationErrors
	in.LabName, in.LabReference, in.Panel = collapseSpaces(in.LabName), strings.TrimSpace(in.LabReference), collapseSpaces(in.Panel)
	in.Comment = strings.TrimSpace(in.Comment)
	if in.LabName == "" || len(in.LabName) > 100 {
		v.Add("lab_name", "is required (at most 100 characters)")
	}
	if len(in.LabReference) > 100 {
		v.Add("lab_reference", "must be at most 100 characters")
	}
	if in.Panel == "" || len(in.Panel) > 200 {
		v.Add("panel", "is required (at most 200 characters), e.g. \"HbA1c\" or \"Lipid profile\"")
	}
	if in.ReportedAt == nil || in.ReportedAt.IsZero() {
		t := now
		in.ReportedAt = &t
	}
	if in.ReportedAt.After(now.Add(5 * time.Minute)) {
		v.Add("reported_at", "cannot be in the future")
	}
	if in.SpecimenCollectedAt != nil && in.SpecimenCollectedAt.After(*in.ReportedAt) {
		v.Add("specimen_collected_at", "cannot be after the report")
	}
	if in.Status == "" {
		in.Status = model.LabFinal
	}
	if !in.Status.Valid() {
		v.Add("status", "must be preliminary, final or corrected")
	}
	if len(in.Comment) > 5000 {
		v.Add("comment", "must be at most 5000 characters")
	}
	if len(in.Items) == 0 || len(in.Items) > 100 {
		v.Add("items", "a result has between 1 and 100 analytes")
	}
	for i := range in.Items {
		it := &in.Items[i]
		it.Analyte, it.Value = collapseSpaces(it.Analyte), strings.TrimSpace(it.Value)
		it.Unit, it.ReferenceRange = strings.TrimSpace(it.Unit), strings.TrimSpace(it.ReferenceRange)
		if it.Analyte == "" || len(it.Analyte) > 100 {
			v.Add(fmt.Sprintf("items[%d].analyte", i), "is required (at most 100 characters)")
		}
		if it.Value == "" || len(it.Value) > 50 {
			v.Add(fmt.Sprintf("items[%d].value", i), "is required (at most 50 characters), exactly as reported")
		}
		if len(it.Unit) > 30 {
			v.Add(fmt.Sprintf("items[%d].unit", i), "must be at most 30 characters")
		}
		if len(it.ReferenceRange) > 60 {
			v.Add(fmt.Sprintf("items[%d].reference_range", i), "must be at most 60 characters")
		}
		if it.AbnormalFlag == "" {
			it.AbnormalFlag = model.FlagNone
		}
		if !it.AbnormalFlag.Valid() {
			v.Add(fmt.Sprintf("items[%d].abnormal_flag", i), "must be the flag the laboratory printed: none, low, high, abnormal, critical_low, critical_high or critical")
		}
	}
	return v
}

// Record stores a result. has_abnormal and has_critical summarise the flags
// the laboratory printed; they are never computed from the values.
func (s *LabService) Record(ctx context.Context, patientID model.ID, in LabResultInput) (*model.LabResult, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	now := s.now()
	if v := in.validate(now); len(v) > 0 {
		return nil, invalid(v)
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	l := &model.LabResult{
		ID: model.MustNewID(), PatientID: patientID, LabName: in.LabName, LabReference: in.LabReference,
		Panel: in.Panel, SpecimenCollectedAt: in.SpecimenCollectedAt, ReportedAt: in.ReportedAt.UTC().Truncate(time.Millisecond),
		Status: in.Status, Comment: in.Comment, RecordedBy: s.actor.UserID, RecordedAt: now,
	}
	for i, it := range in.Items {
		l.Items = append(l.Items, model.LabResultItem{ID: model.MustNewID(), LineNo: i + 1, Analyte: it.Analyte,
			Value: it.Value, Unit: it.Unit, ReferenceRange: it.ReferenceRange, AbnormalFlag: it.AbnormalFlag})
		l.HasAbnormal = l.HasAbnormal || it.AbnormalFlag.IsAbnormal()
		l.HasCritical = l.HasCritical || it.AbnormalFlag.IsCritical()
	}
	if err := s.labs().Create(ctx, l); err != nil {
		return nil, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionLabRecorded, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID),
		Metadata: map[string]any{"lab_result_id": l.ID.String(), "abnormal": l.HasAbnormal, "critical": l.HasCritical}})
	return s.labs().Get(ctx, l.ID)
}

// Get returns one result with its analytes.
func (s *LabService) Get(ctx context.Context, id model.ID) (*model.LabResult, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	l, err := s.labs().Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if err := s.readAudited(ctx, model.ActionLabRead, "patient", l.PatientID, map[string]any{"lab_result_id": id.String()}); err != nil {
		return nil, err
	}
	return l, nil
}

// ForPatient lists a patient's results, newest first.
func (s *LabService) ForPatient(ctx context.Context, patientID model.ID) ([]model.LabResult, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	list, err := s.labs().ForPatient(ctx, patientID, 100)
	if err != nil {
		return nil, err
	}
	if err := s.readAudited(ctx, model.ActionLabRead, "patient", patientID, map[string]any{"count": len(list)}); err != nil {
		return nil, err
	}
	return list, nil
}

// Inbox lists results nobody has acknowledged: critical first, then
// abnormal, oldest first within each.
func (s *LabService) Inbox(ctx context.Context, abnormalOnly bool, limit int) ([]model.LabResult, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	list, err := s.labs().Inbox(ctx, abnormalOnly, limit)
	if err != nil {
		return nil, err
	}
	if err := s.record(ctx, model.AuditEntry{Action: model.ActionLabInboxRead, Outcome: model.OutcomeSuccess,
		Metadata: map[string]any{"count": len(list), "abnormal_only": abnormalOnly}}); err != nil {
		return nil, err
	}
	return list, nil
}

// Acknowledge records that a clinician has seen a result and, optionally,
// what they did about it.
func (s *LabService) Acknowledge(ctx context.Context, id model.ID, note string) (*model.LabResult, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	note = strings.TrimSpace(note)
	if len(note) > 1000 {
		return nil, invalidField("note", "must be at most 1000 characters")
	}
	repo := s.labs()
	l, err := repo.Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if err := repo.Acknowledge(ctx, id, s.actor.UserID, note, s.now()); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrAlreadyAcknowledged
		}
		return nil, err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionLabAcknowledged, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(l.PatientID),
		Metadata: map[string]any{"lab_result_id": id.String(), "abnormal": l.HasAbnormal, "critical": l.HasCritical}})
	return repo.Get(ctx, id)
}
