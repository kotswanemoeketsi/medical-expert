package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// PatientRepository stores the patient record: demographics, allergies,
// conditions and consent.
type PatientRepository struct {
	scope *tenant.Scope
}

func NewPatientRepository(s *tenant.Scope) *PatientRepository { return &PatientRepository{scope: s} }

const patientColumns = `id, tenant_id, first_name, last_name, date_of_birth, sex, id_type, national_id,
       mobile, email, address, preferred_language, preferred_channel, waking_start, waking_end,
       review_interval_months, medical_aid_name, medical_aid_number, nok_name, nok_relationship,
       nok_phone, smoking_status, alcohol_use, past_surgical_history, family_history, status,
       created_by, created_at, updated_at`

func scanPatient(sc scanner) (*model.Patient, error) {
	var p model.Patient
	var idType, nationalID, mobile, email, address, lang, aidName, aidNo sql.Null[string]
	var nokName, nokRel, nokPhone, surgical, family sql.Null[string]
	var wakeStart, wakeEnd sql.Null[model.TimeOfDay]
	var review sql.Null[int]
	var createdBy sql.Null[model.ID]
	err := sc.Scan(&p.ID, &p.TenantID, &p.FirstName, &p.LastName, &p.DateOfBirth, &p.Sex, &idType, &nationalID,
		&mobile, &email, &address, &lang, &p.PreferredChannel, &wakeStart, &wakeEnd,
		&review, &aidName, &aidNo, &nokName, &nokRel,
		&nokPhone, &p.SmokingStatus, &p.AlcoholUse, &surgical, &family, &p.Status,
		&createdBy, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, translate(err)
	}
	p.IDType = model.IDType(idType.V)
	p.NationalID, p.Mobile, p.Email, p.Address, p.PreferredLanguage = nationalID.V, mobile.V, email.V, address.V, lang.V
	p.MedicalAidName, p.MedicalAidNumber = aidName.V, aidNo.V
	p.NOKName, p.NOKRelationship, p.NOKPhone = nokName.V, nokRel.V, nokPhone.V
	p.PastSurgicalHistory, p.FamilyHistory = surgical.V, family.V
	if wakeStart.Valid {
		v := wakeStart.V
		p.WakingStart = &v
	}
	if wakeEnd.Valid {
		v := wakeEnd.V
		p.WakingEnd = &v
	}
	if review.Valid {
		v := review.V
		p.ReviewIntervalMonths = &v
	}
	p.CreatedBy = idPtr(createdBy)
	return &p, nil
}

const insertPatient = `
INSERT INTO patients (tenant_id, id, first_name, last_name, date_of_birth, sex, id_type, national_id,
       mobile, email, address, preferred_language, preferred_channel, waking_start, waking_end,
       review_interval_months, medical_aid_name, medical_aid_number, nok_name, nok_relationship,
       nok_phone, smoking_status, alcohol_use, past_surgical_history, family_history, status,
       created_by, created_at, updated_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// Create inserts a patient. A national ID already registered at this
// practice returns a *DuplicateKeyError for uq_patients_national_id.
func (r *PatientRepository) Create(ctx context.Context, p *model.Patient) error {
	_, err := r.scope.Exec(ctx, insertPatient,
		p.ID.Bytes(), p.FirstName, p.LastName, p.DateOfBirth.String(), string(p.Sex), nullString(string(p.IDType)),
		nullString(p.NationalID), nullString(p.Mobile), nullString(p.Email), nullString(p.Address),
		nullString(p.PreferredLanguage), string(p.PreferredChannel), nullTimeOfDay(p.WakingStart),
		nullTimeOfDay(p.WakingEnd), nullInt(p.ReviewIntervalMonths), nullString(p.MedicalAidName),
		nullString(p.MedicalAidNumber), nullString(p.NOKName), nullString(p.NOKRelationship),
		nullString(p.NOKPhone), string(p.SmokingStatus), string(p.AlcoholUse),
		nullString(p.PastSurgicalHistory), nullString(p.FamilyHistory), string(p.Status),
		nullID(p.CreatedBy), p.CreatedAt, p.UpdatedAt)
	return translate(err)
}

const updatePatient = `
UPDATE patients
   SET first_name = ?, last_name = ?, date_of_birth = ?, sex = ?, id_type = ?, national_id = ?,
       mobile = ?, email = ?, address = ?, preferred_language = ?, preferred_channel = ?,
       waking_start = ?, waking_end = ?, medical_aid_name = ?, medical_aid_number = ?,
       nok_name = ?, nok_relationship = ?, nok_phone = ?, smoking_status = ?, alcohol_use = ?,
       past_surgical_history = ?, family_history = ?, status = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ?`

// Update replaces the demographic and baseline fields. The review interval
// has its own method because it is a clinical decision, not a detail.
func (r *PatientRepository) Update(ctx context.Context, p *model.Patient) error {
	return expectFound(r.scope.Exec(ctx, updatePatient,
		p.FirstName, p.LastName, p.DateOfBirth.String(), string(p.Sex), nullString(string(p.IDType)),
		nullString(p.NationalID), nullString(p.Mobile), nullString(p.Email), nullString(p.Address),
		nullString(p.PreferredLanguage), string(p.PreferredChannel), nullTimeOfDay(p.WakingStart),
		nullTimeOfDay(p.WakingEnd), nullString(p.MedicalAidName), nullString(p.MedicalAidNumber),
		nullString(p.NOKName), nullString(p.NOKRelationship), nullString(p.NOKPhone),
		string(p.SmokingStatus), string(p.AlcoholUse), nullString(p.PastSurgicalHistory),
		nullString(p.FamilyHistory), string(p.Status), p.UpdatedAt, p.ID.Bytes()))
}

const updateReviewInterval = `
UPDATE patients SET review_interval_months = ?, updated_at = ? WHERE tenant_id = :tenant AND id = ?`

func (r *PatientRepository) SetReviewInterval(ctx context.Context, id model.ID, months *int, now time.Time) error {
	return expectFound(r.scope.Exec(ctx, updateReviewInterval, nullInt(months), now, id.Bytes()))
}

const selectPatient = `SELECT ` + patientColumns + ` FROM patients WHERE tenant_id = :tenant AND id = ?`

func (r *PatientRepository) Get(ctx context.Context, id model.ID) (*model.Patient, error) {
	return scanPatient(r.scope.QueryRow(ctx, selectPatient, id.Bytes()))
}

const patientExists = `SELECT COUNT(*) FROM patients WHERE tenant_id = :tenant AND id = ?`

// Exists answers "is this patient in this practice?" without reading the
// record, for endpoints whose list query would otherwise return an empty
// list rather than not-found for someone else's patient.
func (r *PatientRepository) Exists(ctx context.Context, id model.ID) (bool, error) {
	var n int
	if err := r.scope.QueryRow(ctx, patientExists, id.Bytes()).Scan(&n); err != nil {
		return false, translate(err)
	}
	return n > 0, nil
}

const listPatients = `
SELECT ` + patientColumns + `
  FROM patients
 WHERE tenant_id = :tenant
 ORDER BY last_name, first_name, id
 LIMIT ? OFFSET ?`

// Name searches match on prefixes so the (tenant_id, last_name, first_name)
// index is usable; identifiers and numbers match exactly.
const searchPatients = `
SELECT ` + patientColumns + `
  FROM patients
 WHERE tenant_id = :tenant
   AND (last_name LIKE ? OR first_name LIKE ? OR CONCAT(first_name, ' ', last_name) LIKE ?
        OR national_id = ? OR mobile = ? OR medical_aid_number = ?)
 ORDER BY last_name, first_name, id
 LIMIT ? OFFSET ?`

// Search finds patients by name prefix, national ID, mobile (already
// normalised by the caller) or medical aid number. An empty query lists.
func (r *PatientRepository) Search(ctx context.Context, q, mobile string, limit, offset int) ([]model.Patient, error) {
	var rows *sql.Rows
	var err error
	if q == "" {
		rows, err = r.scope.Query(ctx, listPatients, limit, offset)
	} else {
		like := likePrefix(q)
		rows, err = r.scope.Query(ctx, searchPatients, like, like, like, q, nullString(mobile), q, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Patient{}
	for rows.Next() {
		p, err := scanPatient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// DuplicateMatch says why an existing record might be the same person.
type DuplicateMatch string

const (
	MatchNationalID    DuplicateMatch = "national_id"    // high confidence
	MatchNameAndDOB    DuplicateMatch = "name_and_dob"   // medium
	MatchMobileSurname DuplicateMatch = "mobile_surname" // low
)

// DuplicateCandidate is an existing record resembling a new one.
type DuplicateCandidate struct {
	Patient model.Patient
	Match   DuplicateMatch
}

// The collation is case- and accent-insensitive, so "Nkosi" matches "NKOSI"
// and "Zoë" matches "Zoe" — the same person typed by two receptionists.
const selectDuplicates = `
SELECT ` + patientColumns + `, 'national_id' AS match_on
  FROM patients WHERE tenant_id = :tenant AND national_id IS NOT NULL AND national_id = ?
UNION ALL
SELECT ` + patientColumns + `, 'name_and_dob' AS match_on
  FROM patients WHERE tenant_id = :tenant AND last_name = ? AND first_name = ? AND date_of_birth = ?
UNION ALL
SELECT ` + patientColumns + `, 'mobile_surname' AS match_on
  FROM patients WHERE tenant_id = :tenant AND mobile IS NOT NULL AND mobile = ? AND last_name = ?
LIMIT 20`

// FindDuplicates returns existing records that may be the same person as p,
// strongest match first, each record once (BR-2.5).
func (r *PatientRepository) FindDuplicates(ctx context.Context, p *model.Patient) ([]DuplicateCandidate, error) {
	rows, err := r.scope.Query(ctx, selectDuplicates,
		nullString(p.NationalID),
		p.LastName, p.FirstName, p.DateOfBirth.String(),
		nullString(p.Mobile), p.LastName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	rank := map[DuplicateMatch]int{MatchNationalID: 0, MatchNameAndDOB: 1, MatchMobileSurname: 2}
	best := map[model.ID]DuplicateCandidate{}
	var order []model.ID
	for rows.Next() {
		var match string
		p, err := scanPatient(scanWithExtra{rows, &match})
		if err != nil {
			return nil, err
		}
		c := DuplicateCandidate{Patient: *p, Match: DuplicateMatch(match)}
		prev, seen := best[p.ID]
		if !seen {
			order = append(order, p.ID)
		}
		if !seen || rank[c.Match] < rank[prev.Match] {
			best[p.ID] = c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]DuplicateCandidate, 0, len(order))
	for _, id := range order {
		out = append(out, best[id])
	}
	// Strongest match first, stable within a match type.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && rank[out[j].Match] < rank[out[j-1].Match]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// scanWithExtra appends extra destinations after a standard column list.
type scanWithExtra struct {
	rows  *sql.Rows
	extra *string
}

func (s scanWithExtra) Scan(dest ...any) error {
	return s.rows.Scan(append(dest, s.extra)...)
}

// --- allergies ---------------------------------------------------------------

const insertAllergy = `
INSERT INTO patient_allergies (tenant_id, id, patient_id, substance, reaction, severity, status, recorded_by, recorded_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?)`

func (r *PatientRepository) AddAllergy(ctx context.Context, a *model.Allergy) error {
	_, err := r.scope.Exec(ctx, insertAllergy, a.ID.Bytes(), a.PatientID.Bytes(), a.Substance,
		nullString(a.Reaction), string(a.Severity), string(a.Status), a.RecordedBy.Bytes(), a.RecordedAt)
	return translate(err)
}

const selectAllergies = `
SELECT id, patient_id, substance, reaction, severity, status, recorded_by, recorded_at
  FROM patient_allergies
 WHERE tenant_id = :tenant AND patient_id = ?
 ORDER BY FIELD(severity, 'life_threatening', 'severe', 'moderate', 'mild', 'unknown'), substance`

// Allergies lists every allergy, most severe first, including resolved and
// withdrawn entries; callers filter by status.
func (r *PatientRepository) Allergies(ctx context.Context, patientID model.ID) ([]model.Allergy, error) {
	rows, err := r.scope.Query(ctx, selectAllergies, patientID.Bytes())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Allergy{}
	for rows.Next() {
		var a model.Allergy
		var reaction sql.Null[string]
		if err := rows.Scan(&a.ID, &a.PatientID, &a.Substance, &reaction, &a.Severity, &a.Status,
			&a.RecordedBy, &a.RecordedAt); err != nil {
			return nil, err
		}
		a.Reaction = reaction.V
		out = append(out, a)
	}
	return out, rows.Err()
}

const updateAllergyStatus = `
UPDATE patient_allergies SET status = ?, updated_by = ?, updated_at = ?
 WHERE tenant_id = :tenant AND patient_id = ? AND id = ?`

func (r *PatientRepository) SetAllergyStatus(ctx context.Context, patientID, id model.ID, status model.ClinicalStatus, by model.ID, now time.Time) error {
	return expectFound(r.scope.Exec(ctx, updateAllergyStatus, string(status), by.Bytes(), now, patientID.Bytes(), id.Bytes()))
}

// --- conditions ----------------------------------------------------------------

const insertCondition = `
INSERT INTO patient_conditions (tenant_id, id, patient_id, icd10_code, description, is_chronic, status,
       onset_date, resolved_date, recorded_by, recorded_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func (r *PatientRepository) AddCondition(ctx context.Context, c *model.Condition) error {
	_, err := r.scope.Exec(ctx, insertCondition, c.ID.Bytes(), c.PatientID.Bytes(), nullString(c.ICD10Code),
		c.Description, c.IsChronic, string(c.Status), nullDate(c.OnsetDate), nullDate(c.ResolvedDate),
		c.RecordedBy.Bytes(), c.RecordedAt)
	return translate(err)
}

const selectConditions = `
SELECT id, patient_id, icd10_code, description, is_chronic, status, onset_date, resolved_date, recorded_by, recorded_at
  FROM patient_conditions
 WHERE tenant_id = :tenant AND patient_id = ?
 ORDER BY FIELD(status, 'active', 'resolved', 'entered_in_error'), is_chronic DESC, recorded_at DESC`

func (r *PatientRepository) Conditions(ctx context.Context, patientID model.ID) ([]model.Condition, error) {
	rows, err := r.scope.Query(ctx, selectConditions, patientID.Bytes())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Condition{}
	for rows.Next() {
		var c model.Condition
		var code sql.Null[string]
		var onset, resolved sql.Null[model.Date]
		if err := rows.Scan(&c.ID, &c.PatientID, &code, &c.Description, &c.IsChronic, &c.Status,
			&onset, &resolved, &c.RecordedBy, &c.RecordedAt); err != nil {
			return nil, err
		}
		c.ICD10Code = code.V
		if onset.Valid {
			d := onset.V
			c.OnsetDate = &d
		}
		if resolved.Valid {
			d := resolved.V
			c.ResolvedDate = &d
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const updateConditionStatus = `
UPDATE patient_conditions SET status = ?, is_chronic = ?, resolved_date = ?, updated_by = ?, updated_at = ?
 WHERE tenant_id = :tenant AND patient_id = ? AND id = ?`

func (r *PatientRepository) UpdateCondition(ctx context.Context, patientID, id model.ID, status model.ClinicalStatus, chronic bool, resolved *model.Date, by model.ID, now time.Time) error {
	return expectFound(r.scope.Exec(ctx, updateConditionStatus, string(status), chronic, nullDate(resolved),
		by.Bytes(), now, patientID.Bytes(), id.Bytes()))
}

// --- consent (append-only) -----------------------------------------------------

const insertConsent = `
INSERT INTO patient_consents (tenant_id, id, patient_id, purpose, granted, method, note, recorded_by, recorded_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?)`

// AddConsent appends a decision. There is deliberately no update or delete.
func (r *PatientRepository) AddConsent(ctx context.Context, c *model.Consent) error {
	_, err := r.scope.Exec(ctx, insertConsent, c.ID.Bytes(), c.PatientID.Bytes(), string(c.Purpose),
		c.Granted, string(c.Method), nullString(c.Note), nullID(c.RecordedBy), c.RecordedAt)
	return translate(err)
}

const selectConsents = `
SELECT id, patient_id, purpose, granted, method, note, recorded_by, recorded_at
  FROM patient_consents
 WHERE tenant_id = :tenant AND patient_id = ?
 ORDER BY seq`

// ConsentHistory returns every decision, oldest first.
func (r *PatientRepository) ConsentHistory(ctx context.Context, patientID model.ID) ([]model.Consent, error) {
	rows, err := r.scope.Query(ctx, selectConsents, patientID.Bytes())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Consent{}
	for rows.Next() {
		var c model.Consent
		var note sql.Null[string]
		var by sql.Null[model.ID]
		if err := rows.Scan(&c.ID, &c.PatientID, &c.Purpose, &c.Granted, &c.Method, &note,
			&by, &c.RecordedAt); err != nil {
			return nil, err
		}
		c.Note, c.RecordedBy = note.V, idPtr(by)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConsentState folds the history into the latest decision per purpose.
func (r *PatientRepository) ConsentState(ctx context.Context, patientID model.ID) (model.ConsentState, error) {
	history, err := r.ConsentHistory(ctx, patientID)
	if err != nil {
		return nil, err
	}
	state := model.ConsentState{}
	for _, c := range history {
		state[c.Purpose] = c // later rows overwrite earlier ones
	}
	return state, nil
}

const countActiveChronic = `
SELECT COUNT(*) FROM patient_conditions
 WHERE tenant_id = :tenant AND patient_id = ? AND is_chronic = 1 AND status = 'active'`

// HasActiveChronicCondition reports whether a patient is on the chronic
// register: at least one active condition a clinician marked chronic.
func (r *PatientRepository) HasActiveChronicCondition(ctx context.Context, patientID model.ID) (bool, error) {
	var n int
	if err := r.scope.QueryRow(ctx, countActiveChronic, patientID.Bytes()).Scan(&n); err != nil {
		return false, translate(err)
	}
	return n > 0, nil
}
