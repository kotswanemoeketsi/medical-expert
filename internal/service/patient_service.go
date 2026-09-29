package service

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // zone data inside the binary: Windows hosts often lack it
	"unicode"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/phone"
	"github.com/yourorg/medical-expert-system/internal/repository"
)

// PatientService is patient capture and the clinical baseline (M2).
type PatientService struct{ *base }

// PatientInput is the demographic and baseline record as captured.
type PatientInput struct {
	FirstName            string
	LastName             string
	DateOfBirth          model.Date
	Sex                  model.Sex
	IDType               model.IDType
	NationalID           string
	Mobile               string
	Email                string
	Address              string
	PreferredLanguage    string
	PreferredChannel     model.Channel
	WakingStart          *model.TimeOfDay
	WakingEnd            *model.TimeOfDay
	ReviewIntervalMonths *int // used on create; afterwards it has its own endpoint
	MedicalAidName       string
	MedicalAidNumber     string
	NOKName              string
	NOKRelationship      string
	NOKPhone             string
	SmokingStatus        model.SmokingStatus
	AlcoholUse           model.AlcoholUse
	PastSurgicalHistory  string
	FamilyHistory        string
	Status               model.PatientStatus // used on update; new patients are active
}

var (
	passportPattern = regexp.MustCompile(`^[A-Z0-9]{5,20}$`)
	icd10Pattern    = regexp.MustCompile(`^[A-Z][0-9][0-9A-Z](\.[0-9A-Z]{1,4})?$`)
)

// Validate normalises the input in place and returns every problem found.
func (in *PatientInput) Validate(today model.Date) model.ValidationErrors {
	var v model.ValidationErrors
	in.FirstName = collapseSpaces(in.FirstName)
	in.LastName = collapseSpaces(in.LastName)
	if in.FirstName == "" || len(in.FirstName) > 100 {
		v.Add("first_name", "is required (at most 100 characters)")
	}
	if in.LastName == "" || len(in.LastName) > 100 {
		v.Add("last_name", "is required (at most 100 characters)")
	}
	switch {
	case in.DateOfBirth.IsZero():
		v.Add("date_of_birth", "is required")
	case in.DateOfBirth.After(today):
		v.Add("date_of_birth", "cannot be in the future")
	case in.DateOfBirth.Year < 1900:
		v.Add("date_of_birth", "is before 1900")
	}
	if !in.Sex.Valid() {
		v.Add("sex", "must be female, male, intersex or unknown")
	}

	in.NationalID = strings.ToUpper(strings.Join(strings.Fields(in.NationalID), ""))
	if in.NationalID == "" {
		in.IDType = ""
	} else {
		if in.IDType == "" && len(in.NationalID) == 13 && isDigits(in.NationalID) {
			in.IDType = model.IDTypeSAID
		}
		switch in.IDType {
		case model.IDTypeSAID:
			if err := model.ValidateSAID(in.NationalID); err != nil {
				v.Add("national_id", "%s", err.Error())
			} else if !in.DateOfBirth.IsZero() && !model.SAIDMatchesDOB(in.NationalID, in.DateOfBirth) {
				v.Add("national_id", "does not match the date of birth")
			}
		case model.IDTypePassport:
			if !passportPattern.MatchString(in.NationalID) {
				v.Add("national_id", "a passport number is 5-20 letters and digits")
			}
		case model.IDTypeOther:
			if len(in.NationalID) > 32 {
				v.Add("national_id", "must be at most 32 characters")
			}
		case "":
			v.Add("id_type", "is required when an ID number is given (sa_id, passport or other)")
		default:
			v.Add("id_type", "must be sa_id, passport or other")
		}
	}

	if strings.TrimSpace(in.Mobile) != "" {
		m, err := phone.NormalizeE164(in.Mobile)
		if err != nil {
			v.Add("mobile", "%s", err.Error())
		}
		in.Mobile = m
	} else {
		in.Mobile = ""
	}
	in.Email = normaliseEmail(in.Email)
	if in.Email != "" && !validEmail(in.Email) {
		v.Add("email", "is not a valid email address")
	}

	if in.PreferredChannel == "" {
		in.PreferredChannel = model.ChannelNone
	}
	switch {
	case !in.PreferredChannel.Valid():
		v.Add("preferred_channel", "must be whatsapp, sms, push, email or none")
	case in.PreferredChannel.NeedsMobile() && in.Mobile == "":
		// Otherwise the patient would silently never be reminded.
		v.Add("mobile", "is required when reminders are sent by %s", channelName(in.PreferredChannel))
	case in.PreferredChannel == model.ChannelEmail && in.Email == "":
		v.Add("email", "is required when reminders are sent by email")
	}

	if (in.WakingStart == nil) != (in.WakingEnd == nil) {
		v.Add("waking_end", "waking hours need both a start and an end")
	} else if in.WakingStart != nil && in.WakingStart.Minutes == in.WakingEnd.Minutes {
		v.Add("waking_end", "must differ from the start of waking hours")
	}
	if in.ReviewIntervalMonths != nil && (*in.ReviewIntervalMonths < 1 || *in.ReviewIntervalMonths > 24) {
		v.Add("review_interval_months", "must be between 1 and 24")
	}

	if strings.TrimSpace(in.NOKPhone) != "" {
		p, err := phone.NormalizeE164(in.NOKPhone)
		if err != nil {
			v.Add("nok_phone", "%s", err.Error())
		}
		in.NOKPhone = p
	} else {
		in.NOKPhone = ""
	}

	if in.SmokingStatus == "" {
		in.SmokingStatus = model.SmokingUnknown
	}
	if !in.SmokingStatus.Valid() {
		v.Add("smoking_status", "must be never, former, current or unknown")
	}
	if in.AlcoholUse == "" {
		in.AlcoholUse = model.AlcoholUnknown
	}
	if !in.AlcoholUse.Valid() {
		v.Add("alcohol_use", "must be none, occasional, regular, heavy or unknown")
	}
	if in.Status == "" {
		in.Status = model.PatientActive
	}
	if !in.Status.Valid() {
		v.Add("status", "must be active, inactive or deceased")
	}

	for _, f := range []struct {
		name string
		val  *string
		max  int
	}{
		{"address", &in.Address, 500},
		{"preferred_language", &in.PreferredLanguage, 32},
		{"medical_aid_name", &in.MedicalAidName, 100},
		{"medical_aid_number", &in.MedicalAidNumber, 50},
		{"nok_name", &in.NOKName, 200},
		{"nok_relationship", &in.NOKRelationship, 50},
		{"past_surgical_history", &in.PastSurgicalHistory, 5000},
		{"family_history", &in.FamilyHistory, 5000},
	} {
		*f.val = strings.TrimSpace(*f.val)
		if len(*f.val) > f.max {
			v.Add(f.name, "must be at most %d characters", f.max)
		}
	}
	return v
}

func (in *PatientInput) apply(p *model.Patient) {
	p.FirstName, p.LastName, p.DateOfBirth, p.Sex = in.FirstName, in.LastName, in.DateOfBirth, in.Sex
	p.IDType, p.NationalID = in.IDType, in.NationalID
	p.Mobile, p.Email, p.Address, p.PreferredLanguage = in.Mobile, in.Email, in.Address, in.PreferredLanguage
	p.PreferredChannel, p.WakingStart, p.WakingEnd = in.PreferredChannel, in.WakingStart, in.WakingEnd
	p.MedicalAidName, p.MedicalAidNumber = in.MedicalAidName, in.MedicalAidNumber
	p.NOKName, p.NOKRelationship, p.NOKPhone = in.NOKName, in.NOKRelationship, in.NOKPhone
	p.SmokingStatus, p.AlcoholUse = in.SmokingStatus, in.AlcoholUse
	p.PastSurgicalHistory, p.FamilyHistory, p.Status = in.PastSurgicalHistory, in.FamilyHistory, in.Status
}

// Create registers a patient. Possible duplicates are a prompt, not a block
// (BR-2.5): a practice legitimately has two Thabo Nkosis, so the caller may
// resend with confirmNotDuplicate once a person has looked. A national ID
// already on file is different — that is the same person — and cannot be
// confirmed past.
func (s *PatientService) Create(ctx context.Context, in PatientInput, confirmNotDuplicate bool) (*model.Patient, error) {
	if err := s.requireStaff(); err != nil {
		return nil, err
	}
	in.Status = model.PatientActive
	if v := in.Validate(s.today()); len(v) > 0 {
		return nil, invalid(v)
	}
	now := s.now()
	p := &model.Patient{ID: model.MustNewID(), TenantID: s.scope.TenantID(), CreatedBy: idp(s.actor.UserID),
		CreatedAt: now, UpdatedAt: now, ReviewIntervalMonths: in.ReviewIntervalMonths}
	in.apply(p)

	repo := s.patients()
	candidates, err := repo.FindDuplicates(ctx, p)
	if err != nil {
		return nil, err
	}
	if len(candidates) > 0 {
		if candidates[0].Match == repository.MatchNationalID {
			return nil, duplicateError(ErrNationalIDExists, candidates[:1], false)
		}
		if !confirmNotDuplicate {
			return nil, duplicateError(&Error{Kind: KindConflict, Code: "possible_duplicate",
				Message: "This may be a patient who is already registered. Check the matches, then confirm to register a new record anyway."},
				candidates, true)
		}
	}
	if err := repo.Create(ctx, p); err != nil {
		if errors.Is(err, repository.ErrDuplicate) {
			return nil, ErrNationalIDExists
		}
		return nil, err
	}
	meta := map[string]any{}
	if len(candidates) > 0 {
		ids := make([]string, len(candidates))
		for i, c := range candidates {
			ids[i] = c.Patient.ID.String()
		}
		meta["confirmed_not_duplicate_of"] = ids
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionPatientCreated, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(p.ID), Metadata: meta})
	return p, nil
}

func duplicateError(base *Error, candidates []repository.DuplicateCandidate, overridable bool) error {
	confidence := map[repository.DuplicateMatch]string{
		repository.MatchNationalID: "high", repository.MatchNameAndDOB: "medium", repository.MatchMobileSurname: "low",
	}
	list := make([]map[string]any, len(candidates))
	for i, c := range candidates {
		list[i] = map[string]any{
			"id": c.Patient.ID, "first_name": c.Patient.FirstName, "last_name": c.Patient.LastName,
			"date_of_birth": c.Patient.DateOfBirth, "match": string(c.Match), "confidence": confidence[c.Match],
		}
	}
	return &Error{Kind: base.Kind, Code: base.Code, Message: base.Message,
		Details: map[string]any{"candidates": list, "overridable": overridable}}
}

// Update replaces the demographic and baseline fields.
func (s *PatientService) Update(ctx context.Context, id model.ID, in PatientInput) (*model.Patient, error) {
	if err := s.requireStaff(); err != nil {
		return nil, err
	}
	repo := s.patients()
	p, err := repo.Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	in.ReviewIntervalMonths = nil // has its own endpoint
	if v := in.Validate(s.today()); len(v) > 0 {
		return nil, invalid(v)
	}
	before := *p
	in.apply(p)
	p.UpdatedAt = s.now()
	if err := repo.Update(ctx, p); err != nil {
		if errors.Is(err, repository.ErrDuplicate) {
			return nil, ErrNationalIDExists
		}
		return nil, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionPatientUpdated, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(id), Metadata: map[string]any{"fields": changedFields(&before, p)}})
	return p, nil
}

// changedFields names what changed, without the values: the audit log says
// that the mobile number changed, not what it changed to.
func changedFields(a, b *model.Patient) []string {
	var out []string
	add := func(name string, changed bool) {
		if changed {
			out = append(out, name)
		}
	}
	add("first_name", a.FirstName != b.FirstName)
	add("last_name", a.LastName != b.LastName)
	add("date_of_birth", a.DateOfBirth != b.DateOfBirth)
	add("sex", a.Sex != b.Sex)
	add("national_id", a.NationalID != b.NationalID || a.IDType != b.IDType)
	add("mobile", a.Mobile != b.Mobile)
	add("email", a.Email != b.Email)
	add("address", a.Address != b.Address)
	add("preferred_language", a.PreferredLanguage != b.PreferredLanguage)
	add("preferred_channel", a.PreferredChannel != b.PreferredChannel)
	add("waking_hours", fmtTOD(a.WakingStart) != fmtTOD(b.WakingStart) || fmtTOD(a.WakingEnd) != fmtTOD(b.WakingEnd))
	add("medical_aid", a.MedicalAidName != b.MedicalAidName || a.MedicalAidNumber != b.MedicalAidNumber)
	add("next_of_kin", a.NOKName != b.NOKName || a.NOKRelationship != b.NOKRelationship || a.NOKPhone != b.NOKPhone)
	add("smoking_status", a.SmokingStatus != b.SmokingStatus)
	add("alcohol_use", a.AlcoholUse != b.AlcoholUse)
	add("past_surgical_history", a.PastSurgicalHistory != b.PastSurgicalHistory)
	add("family_history", a.FamilyHistory != b.FamilyHistory)
	add("status", a.Status != b.Status)
	return out
}

func fmtTOD(t *model.TimeOfDay) string {
	if t == nil {
		return ""
	}
	return t.String()
}

// canRead decides whether the actor may see a patient's record. Staff may;
// a patient may see only their own (BR-1.9). Anyone else gets not-found,
// never forbidden: a 403 would confirm the record exists.
func (s *PatientService) canRead(ctx context.Context, id model.ID) error {
	if s.actor.Role.IsStaff() {
		return nil
	}
	if s.actor.Role == model.RolePatient && s.actor.PatientID != nil && *s.actor.PatientID == id {
		return nil
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionPatientAccessDenied, Outcome: model.OutcomeDenied,
		SubjectType: "patient", SubjectID: idp(id)})
	return ErrNotFound
}

// canReadClinical is canRead for clinical detail, which practice
// administrators do not need to see.
func (s *PatientService) canReadClinical(ctx context.Context, id model.ID) error {
	if s.actor.Role == model.RoleClinicAdmin {
		return ErrForbidden
	}
	return s.canRead(ctx, id)
}

// Get returns a patient's demographic record. The read is audited, and if
// the audit entry cannot be written the record is not returned.
func (s *PatientService) Get(ctx context.Context, id model.ID) (*model.Patient, error) {
	if err := s.canRead(ctx, id); err != nil {
		return nil, err
	}
	p, err := s.patients().Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if err := s.record(ctx, model.AuditEntry{Action: model.ActionPatientRead, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(id)}); err != nil {
		return nil, err
	}
	return p, nil
}

// Search finds patients by name, ID number, mobile or medical aid number.
func (s *PatientService) Search(ctx context.Context, q string, limit, offset int) ([]model.Patient, error) {
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
	mobile := ""
	if phone.LooksLikeNumber(q) {
		if m, err := phone.NormalizeE164(q); err == nil {
			mobile = m
		}
	}
	results, err := s.patients().Search(ctx, q, mobile, limit, offset)
	if err != nil {
		return nil, err
	}
	if err := s.record(ctx, model.AuditEntry{Action: model.ActionPatientSearched, Outcome: model.OutcomeSuccess,
		Metadata: map[string]any{"query": q, "results": len(results), "offset": offset}}); err != nil {
		return nil, err
	}
	return results, nil
}

// Summary is the consolidated view (BR-2.9). Withdrawn entries (entered in
// error) are left out; resolved ones stay, marked as such.
func (s *PatientService) Summary(ctx context.Context, id model.ID) (*model.PatientSummary, error) {
	if err := s.canReadClinical(ctx, id); err != nil {
		return nil, err
	}
	repo := s.patients()
	p, err := repo.Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	allergies, err := repo.Allergies(ctx, id)
	if err != nil {
		return nil, err
	}
	conditions, err := repo.Conditions(ctx, id)
	if err != nil {
		return nil, err
	}
	consents, err := repo.ConsentState(ctx, id)
	if err != nil {
		return nil, err
	}
	medications, err := s.regimens().Current(ctx, id)
	if err != nil {
		return nil, err
	}
	// Recent notes are signed notes: an unfinished draft is not yet part of
	// the record. Clinicians see how many drafts are open instead.
	recent, err := s.encounters().ForPatient(ctx, id, true, 5)
	if err != nil {
		return nil, err
	}
	sum := &model.PatientSummary{
		Patient: *p, AgeYears: p.AgeAt(s.now()),
		Allergies: []model.Allergy{}, Conditions: []model.Condition{}, Consents: consents.Statuses(),
		Medications: medications, RecentEncounters: recent,
	}
	if s.actor.Role.IsClinical() {
		if sum.OpenDraftNotes, err = s.encounters().OpenDrafts(ctx, id); err != nil {
			return nil, err
		}
		if sum.UnacknowledgedAbnormalResults, err = s.labs().UnacknowledgedAbnormal(ctx, id); err != nil {
			return nil, err
		}
	}
	if err := s.record(ctx, model.AuditEntry{Action: model.ActionPatientSummaryRead, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(id)}); err != nil {
		return nil, err
	}
	for _, a := range allergies {
		if a.Status != model.ClinicalEnteredInError {
			sum.Allergies = append(sum.Allergies, a)
		}
	}
	for _, c := range conditions {
		if c.Status != model.ClinicalEnteredInError {
			sum.Conditions = append(sum.Conditions, c)
		}
	}
	return sum, nil
}

// requirePatient returns not-found for a patient outside this practice. List
// queries return an empty list rather than an error for someone else's
// patient, so every child-record endpoint checks the parent first.
func (s *PatientService) requirePatient(ctx context.Context, id model.ID) error {
	ok, err := s.patients().Exists(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// AllergyInput records an allergy.
type AllergyInput struct {
	Substance string
	Reaction  string
	Severity  model.AllergySeverity
}

func (s *PatientService) AddAllergy(ctx context.Context, patientID model.ID, in AllergyInput) (*model.Allergy, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	var v model.ValidationErrors
	in.Substance, in.Reaction = collapseSpaces(in.Substance), strings.TrimSpace(in.Reaction)
	if in.Substance == "" || len(in.Substance) > 200 {
		v.Add("substance", "is required (at most 200 characters)")
	}
	if len(in.Reaction) > 500 {
		v.Add("reaction", "must be at most 500 characters")
	}
	if in.Severity == "" {
		in.Severity = model.SeverityUnknown
	}
	if !in.Severity.Valid() {
		v.Add("severity", "must be mild, moderate, severe, life_threatening or unknown")
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	if err := s.requirePatient(ctx, patientID); err != nil {
		return nil, err
	}
	a := &model.Allergy{ID: model.MustNewID(), PatientID: patientID, Substance: in.Substance, Reaction: in.Reaction,
		Severity: in.Severity, Status: model.ClinicalActive, RecordedBy: s.actor.UserID, RecordedAt: s.now()}
	if err := s.patients().AddAllergy(ctx, a); err != nil {
		return nil, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionAllergyRecorded, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID), Metadata: map[string]any{"allergy_id": a.ID.String(), "severity": string(a.Severity)}})
	return a, nil
}

// SetAllergyStatus resolves an allergy or withdraws one recorded in error.
// Nothing is deleted.
func (s *PatientService) SetAllergyStatus(ctx context.Context, patientID, allergyID model.ID, status model.ClinicalStatus) error {
	if err := s.requireClinical(); err != nil {
		return err
	}
	if !status.Valid() {
		return invalidField("status", "must be active, resolved or entered_in_error")
	}
	if err := s.patients().SetAllergyStatus(ctx, patientID, allergyID, status, s.actor.UserID, s.now()); err != nil {
		return fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionAllergyUpdated, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID), Metadata: map[string]any{"allergy_id": allergyID.String(), "status": string(status)}})
	return nil
}

// ConditionInput records a condition on the problem list.
type ConditionInput struct {
	ICD10Code   string
	Description string
	IsChronic   bool
	OnsetDate   *model.Date
}

func (s *PatientService) AddCondition(ctx context.Context, patientID model.ID, in ConditionInput) (*model.Condition, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	var v model.ValidationErrors
	in.Description = collapseSpaces(in.Description)
	in.ICD10Code = strings.ToUpper(strings.TrimSpace(in.ICD10Code))
	if in.Description == "" || len(in.Description) > 255 {
		v.Add("description", "is required (at most 255 characters)")
	}
	if in.ICD10Code != "" && !icd10Pattern.MatchString(in.ICD10Code) {
		v.Add("icd10_code", "is not in ICD-10 form (for example E11.9 or I10)")
	}
	if in.OnsetDate != nil && in.OnsetDate.After(s.today()) {
		v.Add("onset_date", "cannot be in the future")
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	if err := s.requirePatient(ctx, patientID); err != nil {
		return nil, err
	}
	c := &model.Condition{ID: model.MustNewID(), PatientID: patientID, ICD10Code: in.ICD10Code,
		Description: in.Description, IsChronic: in.IsChronic, Status: model.ClinicalActive, OnsetDate: in.OnsetDate,
		RecordedBy: s.actor.UserID, RecordedAt: s.now()}
	if err := s.patients().AddCondition(ctx, c); err != nil {
		return nil, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionConditionRecorded, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID),
		Metadata: map[string]any{"condition_id": c.ID.String(), "icd10": c.ICD10Code, "chronic": c.IsChronic}})
	return c, nil
}

// ConditionUpdate changes a condition's status or chronic flag.
type ConditionUpdate struct {
	Status       model.ClinicalStatus
	IsChronic    bool
	ResolvedDate *model.Date
}

func (s *PatientService) UpdateCondition(ctx context.Context, patientID, conditionID model.ID, in ConditionUpdate) error {
	if err := s.requireClinical(); err != nil {
		return err
	}
	if !in.Status.Valid() {
		return invalidField("status", "must be active, resolved or entered_in_error")
	}
	today := s.today()
	switch {
	case in.Status == model.ClinicalResolved && in.ResolvedDate == nil:
		in.ResolvedDate = &today
	case in.Status != model.ClinicalResolved:
		in.ResolvedDate = nil
	}
	if in.ResolvedDate != nil && in.ResolvedDate.After(today) {
		return invalidField("resolved_date", "cannot be in the future")
	}
	if err := s.patients().UpdateCondition(ctx, patientID, conditionID, in.Status, in.IsChronic, in.ResolvedDate,
		s.actor.UserID, s.now()); err != nil {
		return fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionConditionUpdated, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID),
		Metadata: map[string]any{"condition_id": conditionID.String(), "status": string(in.Status), "chronic": in.IsChronic}})
	return nil
}

// ConsentInput records one decision for one purpose.
type ConsentInput struct {
	Purpose model.ConsentPurpose
	Granted *bool
	Method  model.ConsentMethod
	Note    string
}

// RecordConsent appends a decision (BR-2.8). Withdrawal is a new decision,
// not an edit: the history of what the patient agreed to, and when, is kept.
func (s *PatientService) RecordConsent(ctx context.Context, patientID model.ID, in ConsentInput) (*model.Consent, error) {
	if err := s.requireStaff(); err != nil {
		return nil, err
	}
	var v model.ValidationErrors
	if !in.Purpose.Valid() {
		v.Add("purpose", "must be data_processing, ai_assisted_review or reminder_communications")
	}
	if in.Granted == nil {
		v.Add("granted", "is required: true to grant, false to refuse or withdraw")
	}
	if !in.Method.Valid() {
		v.Add("method", "must be written, verbal or electronic")
	}
	in.Note = strings.TrimSpace(in.Note)
	if len(in.Note) > 500 {
		v.Add("note", "must be at most 500 characters")
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	if err := s.requirePatient(ctx, patientID); err != nil {
		return nil, err
	}
	c := &model.Consent{ID: model.MustNewID(), PatientID: patientID, Purpose: in.Purpose, Granted: *in.Granted,
		Method: in.Method, Note: in.Note, RecordedBy: s.actor.UserID, RecordedAt: s.now()}
	if err := s.patients().AddConsent(ctx, c); err != nil {
		return nil, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionConsentRecorded, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID),
		Metadata: map[string]any{"purpose": string(c.Purpose), "granted": c.Granted, "method": string(c.Method)}})
	return c, nil
}

// ConsentView is the current decision per purpose plus the full history.
type ConsentView struct {
	Current []model.ConsentStatus
	History []model.Consent
}

func (s *PatientService) Consents(ctx context.Context, patientID model.ID) (*ConsentView, error) {
	if err := s.canRead(ctx, patientID); err != nil {
		return nil, err
	}
	if err := s.requirePatient(ctx, patientID); err != nil {
		return nil, err
	}
	repo := s.patients()
	history, err := repo.ConsentHistory(ctx, patientID)
	if err != nil {
		return nil, err
	}
	state := model.ConsentState{}
	for _, c := range history {
		state[c.Purpose] = c
	}
	if err := s.record(ctx, model.AuditEntry{Action: model.ActionConsentRead, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID)}); err != nil {
		return nil, err
	}
	return &ConsentView{Current: state.Statuses(), History: history}, nil
}

// SetReviewInterval sets how often a chronic patient should be reviewed.
// nil means the practice default applies.
func (s *PatientService) SetReviewInterval(ctx context.Context, patientID model.ID, months *int) error {
	if err := s.requireClinical(); err != nil {
		return err
	}
	if months != nil && (*months < 1 || *months > 24) {
		return invalidField("review_interval_months", "must be between 1 and 24, or null for the practice default")
	}
	if err := s.patients().SetReviewInterval(ctx, patientID, months, s.now()); err != nil {
		return fromRepo(err)
	}
	meta := map[string]any{"months": nil}
	if months != nil {
		meta["months"] = *months
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionReviewIntervalSet, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID), Metadata: meta})
	return nil
}

// johannesburg is South African Standard Time. The country has no daylight
// saving, so the fixed-offset fallback is exact.
var johannesburg = func() *time.Location {
	if loc, err := time.LoadLocation("Africa/Johannesburg"); err == nil {
		return loc
	}
	return time.FixedZone("SAST", 2*60*60)
}()

// today is the practice's calendar date in South Africa. Dates of birth and
// onset are calendar dates, and "today" must not flip at 22:00 UTC.
func (b *base) today() model.Date {
	return model.DateOf(b.now().In(johannesburg))
}

func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

func isDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

func channelName(c model.Channel) string {
	switch c {
	case model.ChannelWhatsApp:
		return "WhatsApp"
	case model.ChannelSMS:
		return "SMS"
	}
	return string(c)
}
