package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/repository"
)

// MedicationService covers the shared catalogue, regimens (BR-4.1, BR-4.10)
// and the dose record (BR-4.4, BR-4.11).
type MedicationService struct{ *base }

// SearchCatalogue finds medicines in the shared catalogue.
func (s *MedicationService) SearchCatalogue(ctx context.Context, q string, limit, offset int) ([]model.Medication, error) {
	if err := s.requireStaff(); err != nil {
		return nil, err
	}
	q = collapseSpaces(q)
	if len(q) > 100 {
		return nil, invalidField("q", "must be at most 100 characters")
	}
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	return s.reg.platform.SearchMedications(ctx, q, limit, offset)
}

// RegimenInput starts a regimen.
type RegimenInput struct {
	MedicationID model.ID
	Dose         string
	Route        model.Route // empty: the medicine's usual route
	Frequency    model.Frequency
	Instructions string
	StartDate    *model.Date // empty: today
	EndDate      *model.Date
}

// CreateRegimen records what a patient has been told to take. Doctors only.
func (s *MedicationService) CreateRegimen(ctx context.Context, patientID model.ID, in RegimenInput) (*model.Regimen, error) {
	if err := s.requireDoctor(); err != nil {
		return nil, err
	}
	var v model.ValidationErrors
	med, err := s.reg.platform.MedicationByID(ctx, in.MedicationID)
	switch {
	case errors.Is(err, repository.ErrNotFound) || (err == nil && !med.IsActive):
		v.Add("medication_id", "is not in the medication catalogue")
	case err != nil:
		return nil, err
	}
	in.Dose, in.Instructions = collapseSpaces(in.Dose), strings.TrimSpace(in.Instructions)
	if in.Dose == "" || len(in.Dose) > 100 {
		v.Add("dose", "is required (at most 100 characters), e.g. \"1 tablet\" or \"10 units\"")
	}
	if !in.Frequency.Valid() {
		v.Add("frequency", "must be once_daily, twice_daily, three_times_daily, four_times_daily, every_other_day, weekly or as_needed")
	}
	if in.Route == "" && med != nil {
		in.Route = med.Route
	}
	if !in.Route.Valid() {
		v.Add("route", "is not a recognised route")
	}
	if len(in.Instructions) > 500 {
		v.Add("instructions", "must be at most 500 characters")
	}
	today := s.today()
	start := today
	if in.StartDate != nil && !in.StartDate.IsZero() {
		start = *in.StartDate
	}
	if start.Year < 1900 {
		v.Add("start_date", "is not a plausible date")
	}
	if in.EndDate != nil && in.EndDate.Before(start) {
		v.Add("end_date", "cannot be before the start date")
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	now := s.now()
	g := &model.Regimen{
		ID: model.MustNewID(), PatientID: patientID, MedicationID: med.ID, MedicationName: med.DisplayName(),
		Dose: in.Dose, Route: in.Route, Frequency: in.Frequency, Instructions: in.Instructions,
		StartDate: start, EndDate: in.EndDate, Status: model.RegimenActive, PrescribedBy: s.actor.UserID,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.regimens().Create(ctx, g); err != nil {
		return nil, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionRegimenCreated, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID),
		Metadata: map[string]any{"regimen_id": g.ID.String(), "medication": g.MedicationName, "frequency": string(g.Frequency)}})
	s.refreshSchedule(ctx, patientID)
	return g, nil
}

// Regimens lists a patient's regimens, current ones first.
func (s *MedicationService) Regimens(ctx context.Context, patientID model.ID) ([]model.Regimen, error) {
	if err := s.canSeePatientClinical(ctx, patientID); err != nil {
		return nil, err
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	list, err := s.regimens().ForPatient(ctx, patientID)
	if err != nil {
		return nil, err
	}
	if err := s.readAudited(ctx, model.ActionRegimensRead, "patient", patientID, nil); err != nil {
		return nil, err
	}
	return list, nil
}

// SetRegimenStatus pauses, resumes, stops or completes a regimen, recording
// who and why (BR-4.10). Stopped and completed are final. Reminders adjust
// at once: a regimen that stops being active loses its future doses in the
// same transaction, and one resumed is scheduled again straight away.
func (s *MedicationService) SetRegimenStatus(ctx context.Context, id model.ID, status model.RegimenStatus, reason string) (*model.Regimen, error) {
	if err := s.requireDoctor(); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	var v model.ValidationErrors
	if !status.Valid() {
		v.Add("status", "must be active, paused, stopped or completed")
	}
	if reason == "" || len(reason) > 255 {
		v.Add("reason", "is required (at most 255 characters)")
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	repo := s.regimens()
	g, err := repo.Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if !g.Status.CanBecome(status) {
		return nil, ErrRegimenState
	}
	now := s.now()
	withdrawn, err := repo.SetStatusAndWithdraw(ctx, id, g.Status, status, reason, s.actor.UserID, now)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrRegimenState
		}
		return nil, err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionRegimenStatusChanged, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(g.PatientID), Reason: reason,
		Metadata: map[string]any{"regimen_id": id.String(), "from": string(g.Status), "to": string(status), "doses_withdrawn": withdrawn}})
	if status == model.RegimenActive {
		s.refreshSchedule(ctx, g.PatientID)
	}
	return repo.Get(ctx, id)
}

// ScheduleDoses adds pending doses to an active regimen by hand, for a
// schedule the frequencies do not cover, such as a tapering dose. The
// scheduler generates the usual ones (BR-4.2) and never touches these.
func (s *MedicationService) ScheduleDoses(ctx context.Context, regimenID model.ID, times []time.Time) (int, error) {
	if err := s.requireClinical(); err != nil {
		return 0, err
	}
	if len(times) == 0 || len(times) > 100 {
		return 0, invalidField("times", "give between 1 and 100 dose times")
	}
	repo := s.regimens()
	g, err := repo.Get(ctx, regimenID)
	if err != nil {
		return 0, fromRepo(err)
	}
	if g.Status != model.RegimenActive {
		return 0, ErrRegimenState
	}
	// Regimen dates are South African calendar days.
	earliest := time.Date(g.StartDate.Year, g.StartDate.Month, g.StartDate.Day, 0, 0, 0, 0, johannesburg)
	latest := s.now().AddDate(1, 0, 0)
	if g.EndDate != nil {
		latest = time.Date(g.EndDate.Year, g.EndDate.Month, g.EndDate.Day+1, 0, 0, 0, 0, johannesburg)
	}
	var v model.ValidationErrors
	for i, t := range times {
		if t.Before(earliest) || t.After(latest) {
			v.Add("times", "entry %d falls outside the regimen's dates", i+1)
		}
	}
	if len(v) > 0 {
		return 0, invalid(v)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	n, err := repo.ScheduleDoses(ctx, g, times, s.now())
	if err != nil {
		return 0, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionDosesScheduled, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(g.PatientID),
		Metadata: map[string]any{"regimen_id": regimenID.String(), "added": n, "requested": len(times)}})
	return n, nil
}

// DoseResponse is an answer to "did you take it?".
type DoseResponse struct {
	Status model.DoseStatus // taken or skipped
	Reason string           // why a dose was skipped, optional
}

// earlyWindow is how long before its time a dose may be confirmed.
const earlyWindow = 2 * time.Hour

// RespondToDose records a patient's answer, given by the patient in the
// portal or by staff on their behalf. The channel records which: a
// patient's own answer and a staff member's tick are different evidence.
func (s *MedicationService) RespondToDose(ctx context.Context, doseID model.ID, in DoseResponse) (*model.DoseEvent, error) {
	var channel model.ResponseChannel
	switch {
	case s.actor.Role.IsClinical():
		channel = model.ResponseStaff
	case s.actor.Role == model.RolePatient:
		channel = model.ResponsePortal
	default:
		return nil, ErrForbidden
	}
	in.Reason = strings.TrimSpace(in.Reason)
	var v model.ValidationErrors
	if in.Status != model.DoseTaken && in.Status != model.DoseSkipped {
		v.Add("status", "must be taken or skipped")
	}
	if len(in.Reason) > 255 {
		v.Add("reason", "must be at most 255 characters")
	}
	if in.Status == model.DoseTaken && in.Reason != "" {
		v.Add("reason", "is only recorded for skipped doses")
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	repo := s.regimens()
	d, err := repo.Dose(ctx, doseID)
	if err != nil {
		return nil, fromRepo(err)
	}
	if channel == model.ResponsePortal && !s.isOwnPatient(d.PatientID) {
		return nil, ErrNotFound
	}
	now := s.now()
	if d.ScheduledAt.After(now.Add(earlyWindow)) {
		return nil, &Error{Kind: KindConflict, Code: "dose_not_due", Message: "This dose is not due yet."}
	}
	if err := repo.Respond(ctx, doseID, in.Status, channel, idp(s.actor.UserID), in.Reason, now); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrDoseState
		}
		return nil, err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionDoseResponded, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(d.PatientID),
		Metadata: map[string]any{"dose_id": doseID.String(), "status": string(in.Status), "channel": string(channel)}})
	return repo.Dose(ctx, doseID)
}

// Doses lists a patient's doses in a window, newest first: scheduled
// against confirmed (BR-4.11). The default window is the last 30 days.
func (s *MedicationService) Doses(ctx context.Context, patientID model.ID, from, to time.Time) ([]model.DoseEvent, error) {
	if err := s.canSeePatientClinical(ctx, patientID); err != nil {
		return nil, err
	}
	now := s.now()
	if to.IsZero() {
		to = now.Add(24 * time.Hour)
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -30)
	}
	if !from.Before(to) || to.Sub(from) > 366*24*time.Hour {
		return nil, invalidField("from", "the window must run forwards and cover at most a year")
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	list, err := s.regimens().Doses(ctx, patientID, from, to)
	if err != nil {
		return nil, err
	}
	if err := s.readAudited(ctx, model.ActionDosesRead, "patient", patientID, nil); err != nil {
		return nil, err
	}
	return list, nil
}
