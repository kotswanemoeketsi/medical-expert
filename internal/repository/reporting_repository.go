package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// ReportingRepository answers the dashboard (counts only) and the recall
// lists (patients to contact).
type ReportingRepository struct {
	scope *tenant.Scope
}

func NewReportingRepository(s *tenant.Scope) *ReportingRepository {
	return &ReportingRepository{scope: s}
}

// --- dashboard -------------------------------------------------------------------

const countActivePatients = `SELECT COUNT(*) FROM patients WHERE tenant_id = :tenant AND status = 'active'`

const countChronicPatients = `
SELECT COUNT(DISTINCT c.patient_id)
  FROM patient_conditions c
  JOIN patients p ON p.tenant_id = c.tenant_id AND p.id = c.patient_id
 WHERE c.tenant_id = :tenant AND c.is_chronic = 1 AND c.status = 'active' AND p.status = 'active'`

// Conditions are grouped by ICD-10 code where there is one, and by wording
// where there is not.
const countChronicByCondition = `
SELECT MIN(c.icd10_code), MIN(c.description), COUNT(DISTINCT c.patient_id)
  FROM patient_conditions c
  JOIN patients p ON p.tenant_id = c.tenant_id AND p.id = c.patient_id
 WHERE c.tenant_id = :tenant AND c.is_chronic = 1 AND c.status = 'active' AND p.status = 'active'
 GROUP BY COALESCE(c.icd10_code, LOWER(c.description))
 ORDER BY COUNT(DISTINCT c.patient_id) DESC, MIN(c.description)
 LIMIT 20`

// Up to the end of today: a note may be timed a few minutes ahead of the
// server's clock.
const countSignedEncounters = `
SELECT COUNT(*), COUNT(DISTINCT patient_id) FROM encounters
 WHERE tenant_id = :tenant AND status = 'signed' AND encounter_at >= ? AND encounter_at < ?`

const countOpenDraftNotes = `SELECT COUNT(*) FROM encounters WHERE tenant_id = :tenant AND status = 'draft'`

const countDoseStatuses = `
SELECT COALESCE(SUM(status = 'taken'), 0), COALESCE(SUM(status = 'skipped'), 0),
       COALESCE(SUM(status = 'missed'), 0), COALESCE(SUM(status = 'pending'), 0)
  FROM dose_events
 WHERE tenant_id = :tenant AND scheduled_at >= ? AND scheduled_at < ?`

const countAnswersByPatient = `
SELECT SUM(status IN ('taken', 'skipped')), SUM(status IN ('taken', 'skipped', 'missed'))
  FROM dose_events
 WHERE tenant_id = :tenant AND scheduled_at >= ? AND scheduled_at < ?
 GROUP BY patient_id`

const countUnacknowledgedResults = `
SELECT COUNT(*), COALESCE(SUM(has_abnormal), 0), COALESCE(SUM(has_critical), 0)
  FROM lab_results WHERE tenant_id = :tenant AND acknowledged_at IS NULL`

// Original authorisations only: a repeat carries its parent's dates.
const countScriptStates = `
SELECT COALESCE(SUM(valid_until <= ?), 0), COALESCE(SUM(repeats_used >= repeats_authorised), 0)
  FROM prescriptions
 WHERE tenant_id = :tenant AND status = 'active' AND repeat_of_id IS NULL AND valid_until >= ?`

// ConditionCount is how many patients have one chronic condition.
type ConditionCount struct {
	ICD10Code   string `json:"icd10_code,omitempty"`
	Description string `json:"description"`
	Patients    int    `json:"patients"`
}

// DashboardCounts is every figure the dashboard shows. None identifies a
// patient.
type DashboardCounts struct {
	ActivePatients, ChronicPatients        int
	ByCondition                            []ConditionCount
	SignedToday, SeenToday                 int
	SignedInWindow, SeenInWindow           int
	OpenDrafts                             int
	Doses                                  model.DoseCounts
	PendingDoses                           int
	Buckets                                model.ResponseBuckets
	UnackResults, UnackAbnormal, UnackCrit int
	ScriptsExpiring, ScriptsExhausted      int
}

// Dashboard counts over [windowStart, now), with "today" from todayStart.
func (r *ReportingRepository) Dashboard(ctx context.Context, windowStart, todayStart, now time.Time, today, expiringBy model.Date) (*DashboardCounts, error) {
	var d DashboardCounts
	if err := r.scope.QueryRow(ctx, countActivePatients).Scan(&d.ActivePatients); err != nil {
		return nil, translate(err)
	}
	if err := r.scope.QueryRow(ctx, countChronicPatients).Scan(&d.ChronicPatients); err != nil {
		return nil, translate(err)
	}
	byCondition, err := r.chronicByCondition(ctx)
	if err != nil {
		return nil, err
	}
	d.ByCondition = byCondition
	tomorrow := todayStart.AddDate(0, 0, 1)
	if err := r.scope.QueryRow(ctx, countSignedEncounters, todayStart, tomorrow).Scan(&d.SignedToday, &d.SeenToday); err != nil {
		return nil, translate(err)
	}
	if err := r.scope.QueryRow(ctx, countSignedEncounters, windowStart, tomorrow).Scan(&d.SignedInWindow, &d.SeenInWindow); err != nil {
		return nil, translate(err)
	}
	if err := r.scope.QueryRow(ctx, countOpenDraftNotes).Scan(&d.OpenDrafts); err != nil {
		return nil, translate(err)
	}
	if err := r.scope.QueryRow(ctx, countDoseStatuses, windowStart, now).Scan(&d.Doses.Taken, &d.Doses.Skipped,
		&d.Doses.Missed, &d.PendingDoses); err != nil {
		return nil, translate(err)
	}
	if d.Buckets, err = r.responseBuckets(ctx, windowStart, now); err != nil {
		return nil, err
	}
	if err := r.scope.QueryRow(ctx, countUnacknowledgedResults).Scan(&d.UnackResults, &d.UnackAbnormal, &d.UnackCrit); err != nil {
		return nil, translate(err)
	}
	if err := r.scope.QueryRow(ctx, countScriptStates, expiringBy.String(), today.String()).Scan(&d.ScriptsExpiring,
		&d.ScriptsExhausted); err != nil {
		return nil, translate(err)
	}
	return &d, nil
}

func (r *ReportingRepository) chronicByCondition(ctx context.Context) ([]ConditionCount, error) {
	rows, err := r.scope.Query(ctx, countChronicByCondition)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConditionCount{}
	for rows.Next() {
		var c ConditionCount
		var code sql.Null[string]
		if err := rows.Scan(&code, &c.Description, &c.Patients); err != nil {
			return nil, err
		}
		c.ICD10Code = code.V
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *ReportingRepository) responseBuckets(ctx context.Context, from, to time.Time) (model.ResponseBuckets, error) {
	var b model.ResponseBuckets
	rows, err := r.scope.Query(ctx, countAnswersByPatient, from, to)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	for rows.Next() {
		var answered, due int
		if err := rows.Scan(&answered, &due); err != nil {
			return b, err
		}
		b.Add(answered, due)
	}
	return b, rows.Err()
}

// --- adherence per regimen (BR-4.6) -------------------------------------------

const countRegimenAdherence = `
SELECT regimen_id,
       COALESCE(SUM(status = 'taken' AND scheduled_at >= ?), 0), COALESCE(SUM(status = 'skipped' AND scheduled_at >= ?), 0),
       COALESCE(SUM(status = 'missed' AND scheduled_at >= ?), 0),
       COALESCE(SUM(status = 'taken' AND scheduled_at >= ?), 0), COALESCE(SUM(status = 'skipped' AND scheduled_at >= ?), 0),
       COALESCE(SUM(status = 'missed' AND scheduled_at >= ?), 0),
       COALESCE(SUM(status = 'taken'), 0), COALESCE(SUM(status = 'skipped'), 0), COALESCE(SUM(status = 'missed'), 0)
  FROM dose_events
 WHERE tenant_id = :tenant AND patient_id = ? AND scheduled_at >= ? AND scheduled_at < ?
 GROUP BY regimen_id`

// RegimenDoseCounts are one regimen's closed doses over 7, 30 and 90 days.
type RegimenDoseCounts struct {
	RegimenID model.ID
	Days7     model.DoseCounts
	Days30    model.DoseCounts
	Days90    model.DoseCounts
}

// Adherence counts a patient's closed doses per regimen over the last 7, 30
// and 90 days before now.
func (r *ReportingRepository) Adherence(ctx context.Context, patientID model.ID, now time.Time) (map[model.ID]RegimenDoseCounts, error) {
	d7, d30, d90 := now.AddDate(0, 0, -7), now.AddDate(0, 0, -30), now.AddDate(0, 0, -90)
	rows, err := r.scope.Query(ctx, countRegimenAdherence, d7, d7, d7, d30, d30, d30, patientID.Bytes(), d90, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[model.ID]RegimenDoseCounts{}
	for rows.Next() {
		var c RegimenDoseCounts
		if err := rows.Scan(&c.RegimenID, &c.Days7.Taken, &c.Days7.Skipped, &c.Days7.Missed,
			&c.Days30.Taken, &c.Days30.Skipped, &c.Days30.Missed,
			&c.Days90.Taken, &c.Days90.Skipped, &c.Days90.Missed); err != nil {
			return nil, err
		}
		out[c.RegimenID] = c
	}
	return out, rows.Err()
}

// --- recall lists ----------------------------------------------------------------

// RecallRow is one patient to contact, with the facts behind the reason.
type RecallRow struct {
	PatientID       model.ID
	FirstName       string
	LastName        string
	Mobile          string
	Channel         model.Channel
	ReminderConsent bool
	LastRecallAt    *time.Time

	LastReview     *time.Time
	ReviewMonths   int
	PrescriptionID *model.ID
	ValidUntil     *model.Date
	RepeatsLeft    *int
	Answered, Due  int
	LabResultID    *model.ID
	Panel          string
	ResultRecorded *time.Time
	Critical       bool
}

// Every recall list is restricted to active patients with an active chronic
// condition: a list that pulls in everyone who ever attended is noise, and a
// worklist nobody trusts is a worklist nobody works.
//
// recallPatient is a fragment of the select list, and recallChronic a
// fragment of the WHERE clause; each query supplies its own tenant
// predicate.
const recallPatient = ` p.id, p.first_name, p.last_name, p.mobile, p.preferred_channel,
       COALESCE((SELECT c.granted FROM patient_consents c
                  WHERE c.tenant_id = p.tenant_id AND c.patient_id = p.id AND c.purpose = 'reminder_communications'
                  ORDER BY c.seq DESC LIMIT 1), 0),
       (SELECT MAX(n.created_at) FROM notifications n
         WHERE n.tenant_id = p.tenant_id AND n.patient_id = p.id AND n.purpose = 'recall' AND n.status <> 'failed')`

const recallChronic = ` p.status = 'active'
   AND EXISTS (SELECT 1 FROM patient_conditions k
                WHERE k.tenant_id = p.tenant_id AND k.patient_id = p.id AND k.is_chronic = 1 AND k.status = 'active')`

// Only signed notes count as a review: an unfinished draft from three
// months ago is not evidence that the patient was seen, and counting it
// would hide exactly the patients this list exists to find.
const recallReviewOverdue = `SELECT` + recallPatient + `, x.last_review, COALESCE(p.review_interval_months, ?)
  FROM patients p
  LEFT JOIN (SELECT patient_id, MAX(encounter_at) AS last_review FROM encounters
              WHERE tenant_id = :tenant AND status = 'signed' GROUP BY patient_id) x ON x.patient_id = p.id
 WHERE p.tenant_id = :tenant AND` + recallChronic + `
   AND (x.last_review IS NULL
        OR x.last_review < DATE_SUB(?, INTERVAL COALESCE(p.review_interval_months, ?) MONTH))
 ORDER BY x.last_review IS NOT NULL, x.last_review, p.last_name, p.first_name
 LIMIT ?`

// The latest original script per patient: a patient whose doctor has already
// written a new one is not on the list.
const recallLatestScript = `
   AND NOT EXISTS (SELECT 1 FROM prescriptions n
                    WHERE n.tenant_id = s.tenant_id AND n.patient_id = s.patient_id AND n.status = 'active'
                      AND n.repeat_of_id IS NULL AND n.issued_at > s.issued_at)`

const recallScriptExpiring = `SELECT` + recallPatient + `, s.id, s.valid_until, s.repeats_authorised - s.repeats_used
  FROM prescriptions s
  JOIN patients p ON p.tenant_id = s.tenant_id AND p.id = s.patient_id
 WHERE s.tenant_id = :tenant AND s.status = 'active' AND s.repeat_of_id IS NULL
   AND s.valid_until >= ? AND s.valid_until <= ?
   AND` + recallChronic + recallLatestScript + `
 ORDER BY s.valid_until, p.last_name, p.first_name
 LIMIT ?`

const recallRepeatsExhausted = `SELECT` + recallPatient + `, s.id, s.valid_until, s.repeats_authorised - s.repeats_used
  FROM prescriptions s
  JOIN patients p ON p.tenant_id = s.tenant_id AND p.id = s.patient_id
 WHERE s.tenant_id = :tenant AND s.status = 'active' AND s.repeat_of_id IS NULL
   AND s.repeats_used >= s.repeats_authorised AND s.valid_until >= ?
   AND` + recallChronic + recallLatestScript + `
 ORDER BY s.valid_until, p.last_name, p.first_name
 LIMIT ?`

const recallNotResponding = `SELECT` + recallPatient + `,
       SUM(d.status IN ('taken', 'skipped')), COUNT(*)
  FROM dose_events d
  JOIN patients p ON p.tenant_id = d.tenant_id AND p.id = d.patient_id
 WHERE d.tenant_id = :tenant AND d.status IN ('taken', 'skipped', 'missed')
   AND d.scheduled_at >= ? AND d.scheduled_at < ?
   AND` + recallChronic + `
 GROUP BY p.id, p.tenant_id, p.first_name, p.last_name, p.mobile, p.preferred_channel
HAVING COUNT(*) >= ? AND SUM(d.status IN ('taken', 'skipped')) * 100 < ? * COUNT(*)
 ORDER BY SUM(d.status IN ('taken', 'skipped')) / COUNT(*), p.last_name, p.first_name
 LIMIT ?`

const recallUnacknowledgedResult = `SELECT` + recallPatient + `, l.id, l.panel, l.recorded_at, l.has_critical
  FROM lab_results l
  JOIN patients p ON p.tenant_id = l.tenant_id AND p.id = l.patient_id
 WHERE l.tenant_id = :tenant AND l.acknowledged_at IS NULL AND l.has_abnormal = 1 AND l.recorded_at < ?
   AND` + recallChronic + `
 ORDER BY l.has_critical DESC, l.recorded_at, p.last_name
 LIMIT ?`

func scanRecall(rows *sql.Rows, extra ...any) (*RecallRow, error) {
	var r RecallRow
	var mobile sql.Null[string]
	var lastRecall sql.Null[time.Time]
	dest := append([]any{&r.PatientID, &r.FirstName, &r.LastName, &mobile, &r.Channel, &r.ReminderConsent, &lastRecall}, extra...)
	if err := rows.Scan(dest...); err != nil {
		return nil, err
	}
	r.Mobile, r.LastRecallAt = mobile.V, timePtr(lastRecall)
	return &r, nil
}

func collectRecalls(rows *sql.Rows, err error, fill func(rows *sql.Rows) (*RecallRow, error)) ([]RecallRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecallRow{}
	for rows.Next() {
		r, err := fill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ReviewOverdue: last signed consultation older than the patient's review
// interval (or the default), or none at all.
func (r *ReportingRepository) ReviewOverdue(ctx context.Context, now time.Time, defaultMonths, limit int) ([]RecallRow, error) {
	rows, err := r.scope.Query(ctx, recallReviewOverdue, defaultMonths, now, defaultMonths, limit)
	return collectRecalls(rows, err, func(rows *sql.Rows) (*RecallRow, error) {
		var last sql.Null[time.Time]
		var months int
		row, err := scanRecall(rows, &last, &months)
		if err != nil {
			return nil, err
		}
		row.LastReview, row.ReviewMonths = timePtr(last), months
		return row, nil
	})
}

func scriptRecall(rows *sql.Rows) (*RecallRow, error) {
	var id model.ID
	var until model.Date
	var left int
	row, err := scanRecall(rows, &id, &until, &left)
	if err != nil {
		return nil, err
	}
	row.PrescriptionID, row.ValidUntil, row.RepeatsLeft = &id, &until, &left
	return row, nil
}

// ScriptExpiring: the latest script runs out between today and by.
func (r *ReportingRepository) ScriptExpiring(ctx context.Context, today, by model.Date, limit int) ([]RecallRow, error) {
	rows, err := r.scope.Query(ctx, recallScriptExpiring, today.String(), by.String(), limit)
	return collectRecalls(rows, err, scriptRecall)
}

// RepeatsExhausted: the latest script is still valid but every repeat has
// been issued.
func (r *ReportingRepository) RepeatsExhausted(ctx context.Context, today model.Date, limit int) ([]RecallRow, error) {
	rows, err := r.scope.Query(ctx, recallRepeatsExhausted, today.String(), limit)
	return collectRecalls(rows, err, scriptRecall)
}

// NotResponding: answered fewer than pct percent of at least minDue closed
// doses in [from, to).
func (r *ReportingRepository) NotResponding(ctx context.Context, from, to time.Time, minDue, pct, limit int) ([]RecallRow, error) {
	rows, err := r.scope.Query(ctx, recallNotResponding, from, to, minDue, pct, limit)
	return collectRecalls(rows, err, func(rows *sql.Rows) (*RecallRow, error) {
		var answered, due int
		row, err := scanRecall(rows, &answered, &due)
		if err != nil {
			return nil, err
		}
		row.Answered, row.Due = answered, due
		return row, nil
	})
}

// UnacknowledgedResult: abnormal results nobody has reviewed, recorded
// before the cutoff.
func (r *ReportingRepository) UnacknowledgedResult(ctx context.Context, recordedBefore time.Time, limit int) ([]RecallRow, error) {
	rows, err := r.scope.Query(ctx, recallUnacknowledgedResult, recordedBefore, limit)
	return collectRecalls(rows, err, func(rows *sql.Rows) (*RecallRow, error) {
		var id model.ID
		var panel string
		var recorded time.Time
		var critical bool
		row, err := scanRecall(rows, &id, &panel, &recorded, &critical)
		if err != nil {
			return nil, err
		}
		row.LabResultID, row.Panel, row.ResultRecorded, row.Critical = &id, panel, &recorded, critical
		return row, nil
	})
}
