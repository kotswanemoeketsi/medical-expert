package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// RegimenRepository stores what patients have been told to take, and the
// record of each scheduled dose.
type RegimenRepository struct {
	scope *tenant.Scope
}

func NewRegimenRepository(s *tenant.Scope) *RegimenRepository { return &RegimenRepository{scope: s} }

// regimenFrom is a fragment; each query prefixes SELECT and adds its own
// tenant predicate. The catalogue is shared, so it is joined on id alone.
const regimenFrom = ` r.id, r.patient_id, r.medication_id, m.generic_name, m.brand_name, m.strength, m.dosage_form,
       r.dose, r.route, r.frequency, r.instructions, r.start_date, r.end_date, r.status, r.status_reason,
       r.status_changed_by, r.status_changed_at, r.prescribed_by, r.created_at, r.updated_at
  FROM regimens r
  JOIN medications m ON m.id = r.medication_id`

func scanRegimen(sc scanner) (*model.Regimen, error) {
	var r model.Regimen
	var med model.Medication
	var instructions, reason sql.Null[string]
	var endDate sql.Null[model.Date]
	var changedBy sql.Null[model.ID]
	var changedAt sql.Null[time.Time]
	err := sc.Scan(&r.ID, &r.PatientID, &r.MedicationID, &med.GenericName, &med.BrandName, &med.Strength, &med.DosageForm,
		&r.Dose, &r.Route, &r.Frequency, &instructions, &r.StartDate, &endDate, &r.Status, &reason,
		&changedBy, &changedAt, &r.PrescribedBy, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, translate(err)
	}
	r.MedicationName = med.DisplayName()
	r.Instructions, r.StatusReason = instructions.V, reason.V
	if endDate.Valid {
		d := endDate.V
		r.EndDate = &d
	}
	r.StatusChangedBy, r.StatusChangedAt = idPtr(changedBy), timePtr(changedAt)
	return &r, nil
}

func collectRegimens(rows *sql.Rows, err error) ([]model.Regimen, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Regimen{}
	for rows.Next() {
		r, err := scanRegimen(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

const insertRegimen = `
INSERT INTO regimens (tenant_id, id, patient_id, medication_id, dose, route, frequency, instructions,
                      start_date, end_date, status, prescribed_by, created_at, updated_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func (r *RegimenRepository) Create(ctx context.Context, g *model.Regimen) error {
	_, err := r.scope.Exec(ctx, insertRegimen, g.ID.Bytes(), g.PatientID.Bytes(), g.MedicationID.Bytes(), g.Dose,
		string(g.Route), string(g.Frequency), nullString(g.Instructions), g.StartDate.String(), nullDate(g.EndDate),
		string(g.Status), g.PrescribedBy.Bytes(), g.CreatedAt, g.UpdatedAt)
	return translate(err)
}

const selectRegimen = `SELECT` + regimenFrom + ` WHERE r.tenant_id = :tenant AND r.id = ?`

func (r *RegimenRepository) Get(ctx context.Context, id model.ID) (*model.Regimen, error) {
	return scanRegimen(r.scope.QueryRow(ctx, selectRegimen, id.Bytes()))
}

const selectPatientRegimens = `SELECT` + regimenFrom + `
 WHERE r.tenant_id = :tenant AND r.patient_id = ?
 ORDER BY FIELD(r.status, 'active', 'paused', 'completed', 'stopped'), r.start_date DESC, m.generic_name`

// ForPatient lists every regimen, current ones first.
func (r *RegimenRepository) ForPatient(ctx context.Context, patientID model.ID) ([]model.Regimen, error) {
	return collectRegimens(r.scope.Query(ctx, selectPatientRegimens, patientID.Bytes()))
}

const selectCurrentRegimens = `SELECT` + regimenFrom + `
 WHERE r.tenant_id = :tenant AND r.patient_id = ? AND r.status IN ('active', 'paused')
 ORDER BY FIELD(r.status, 'active', 'paused'), m.generic_name`

// Current lists active and paused regimens: the patient's medication list.
func (r *RegimenRepository) Current(ctx context.Context, patientID model.ID) ([]model.Regimen, error) {
	return collectRegimens(r.scope.Query(ctx, selectCurrentRegimens, patientID.Bytes()))
}

// The old status is part of the WHERE clause, so two clinicians changing
// the same regimen at once cannot both succeed from the same starting point.
const updateRegimenStatus = `
UPDATE regimens SET status = ?, status_reason = ?, status_changed_by = ?, status_changed_at = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = ?`

func (r *RegimenRepository) SetStatus(ctx context.Context, id model.ID, from, to model.RegimenStatus, reason string, by model.ID, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, updateRegimenStatus, string(to), nullString(reason), by.Bytes(), now, now,
		id.Bytes(), string(from)))
}

// --- doses --------------------------------------------------------------------

const insertDose = `
INSERT INTO dose_events (tenant_id, id, patient_id, regimen_id, scheduled_at, status, created_at)
VALUES (:tenant, ?, ?, ?, ?, 'pending', ?)`

// ScheduleDoses adds pending doses. A dose already scheduled for the same
// regimen at the same moment is skipped rather than duplicated. Returns how
// many were added.
func (r *RegimenRepository) ScheduleDoses(ctx context.Context, g *model.Regimen, times []time.Time, now time.Time) (int, error) {
	added := 0
	err := r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		for _, at := range times {
			id, err := model.NewID()
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, insertDose, id.Bytes(), g.PatientID.Bytes(), g.ID.Bytes(), at.UTC(), now)
			if err != nil {
				if errors.Is(translate(err), ErrDuplicate) {
					continue
				}
				return translate(err)
			}
			added++
		}
		return nil
	})
	return added, err
}

// doseFrom is a fragment; each query prefixes SELECT and adds its own
// tenant predicate.
const doseFrom = ` d.id, d.patient_id, d.regimen_id, m.generic_name, m.brand_name, m.strength, m.dosage_form,
       d.scheduled_at, d.source, d.reminded_at, d.status, d.responded_at, d.response_channel, d.responded_by,
       d.skip_reason, d.created_at
  FROM dose_events d
  JOIN regimens r    ON r.tenant_id = d.tenant_id AND r.id = d.regimen_id
  JOIN medications m ON m.id = r.medication_id`

func scanDose(sc scanner) (*model.DoseEvent, error) {
	var d model.DoseEvent
	var med model.Medication
	var respondedAt, remindedAt sql.Null[time.Time]
	var channel, reason sql.Null[string]
	var by sql.Null[model.ID]
	if err := sc.Scan(&d.ID, &d.PatientID, &d.RegimenID, &med.GenericName, &med.BrandName, &med.Strength, &med.DosageForm,
		&d.ScheduledAt, &d.Source, &remindedAt, &d.Status, &respondedAt, &channel, &by, &reason, &d.CreatedAt); err != nil {
		return nil, translate(err)
	}
	d.MedicationName = med.DisplayName()
	d.RemindedAt = timePtr(remindedAt)
	d.RespondedAt, d.RespondedBy = timePtr(respondedAt), idPtr(by)
	d.ResponseChannel, d.SkipReason = model.ResponseChannel(channel.V), reason.V
	return &d, nil
}

const selectDose = `SELECT` + doseFrom + ` WHERE d.tenant_id = :tenant AND d.id = ?`

func (r *RegimenRepository) Dose(ctx context.Context, id model.ID) (*model.DoseEvent, error) {
	return scanDose(r.scope.QueryRow(ctx, selectDose, id.Bytes()))
}

const selectPatientDoses = `SELECT` + doseFrom + `
 WHERE d.tenant_id = :tenant AND d.patient_id = ? AND d.scheduled_at >= ? AND d.scheduled_at < ?
 ORDER BY d.scheduled_at DESC
 LIMIT 1000`

// Doses lists a patient's doses scheduled in [from, to), newest first
// (BR-4.11: scheduled against confirmed).
func (r *RegimenRepository) Doses(ctx context.Context, patientID model.ID, from, to time.Time) ([]model.DoseEvent, error) {
	rows, err := r.scope.Query(ctx, selectPatientDoses, patientID.Bytes(), from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.DoseEvent{}
	for rows.Next() {
		d, err := scanDose(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// Only a pending dose can be answered: a missed dose was already counted
// as missed, and a second answer would rewrite what the first one said.
const respondToDose = `
UPDATE dose_events
   SET status = ?, responded_at = ?, response_channel = ?, responded_by = ?, skip_reason = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'pending'`

// Respond records a patient's answer. ErrConflict means the dose was not
// pending.
func (r *RegimenRepository) Respond(ctx context.Context, id model.ID, status model.DoseStatus, channel model.ResponseChannel, by *model.ID, reason string, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, respondToDose, string(status), now, string(channel), nullID(by),
		nullString(reason), id.Bytes()))
}

// --- the dose scheduler -------------------------------------------------------

// ScheduleCandidate is an active regimen with what the scheduler needs to
// know about its patient: when they are awake, and whether they can answer
// at all.
type ScheduleCandidate struct {
	Regimen         model.Regimen
	PatientActive   bool
	Channel         model.Channel
	HasMobile       bool
	Waking          model.WakingHours
	ReminderConsent bool // the latest reminder_communications decision
	HasPortal       bool // an active patient login
}

// candidateFrom is a fragment; each query prefixes SELECT and adds its own
// tenant predicate. The consent subquery takes the latest decision, the same
// rule ConsentState applies.
const candidateFrom = ` r.id, r.patient_id, r.frequency, r.start_date, r.end_date, r.status,
       p.status, p.preferred_channel, p.mobile, p.waking_start, p.waking_end,
       COALESCE((SELECT c.granted FROM patient_consents c
                  WHERE c.tenant_id = r.tenant_id AND c.patient_id = r.patient_id
                    AND c.purpose = 'reminder_communications'
                  ORDER BY c.seq DESC LIMIT 1), 0),
       EXISTS (SELECT 1 FROM users u
                WHERE u.tenant_id = r.tenant_id AND u.patient_id = r.patient_id
                  AND u.role = 'patient' AND u.status = 'active')
  FROM regimens r
  JOIN patients p ON p.tenant_id = r.tenant_id AND p.id = r.patient_id`

const selectScheduleCandidates = `SELECT` + candidateFrom + `
 WHERE r.tenant_id = :tenant AND r.status = 'active' AND r.frequency <> 'as_needed'
   AND (r.end_date IS NULL OR r.end_date >= ?)
 ORDER BY r.patient_id, r.id`

const selectPatientScheduleCandidates = `SELECT` + candidateFrom + `
 WHERE r.tenant_id = :tenant AND r.patient_id = ? AND r.status = 'active' AND r.frequency <> 'as_needed'
   AND (r.end_date IS NULL OR r.end_date >= ?)
 ORDER BY r.id`

func collectCandidates(rows *sql.Rows, err error) ([]ScheduleCandidate, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduleCandidate
	for rows.Next() {
		var c ScheduleCandidate
		var endDate sql.Null[model.Date]
		var patientStatus string
		var mobile sql.Null[string]
		var wakeStart, wakeEnd sql.Null[model.TimeOfDay]
		if err := rows.Scan(&c.Regimen.ID, &c.Regimen.PatientID, &c.Regimen.Frequency, &c.Regimen.StartDate, &endDate,
			&c.Regimen.Status, &patientStatus, &c.Channel, &mobile, &wakeStart, &wakeEnd,
			&c.ReminderConsent, &c.HasPortal); err != nil {
			return nil, err
		}
		if endDate.Valid {
			d := endDate.V
			c.Regimen.EndDate = &d
		}
		c.PatientActive = patientStatus == string(model.PatientActive)
		c.HasMobile = mobile.Valid && mobile.V != ""
		p := model.Patient{}
		if wakeStart.Valid && wakeEnd.Valid {
			s, e := wakeStart.V, wakeEnd.V
			p.WakingStart, p.WakingEnd = &s, &e
		}
		c.Waking = p.Waking()
		out = append(out, c)
	}
	return out, rows.Err()
}

// ScheduleCandidates lists every active, scheduled regimen still running on
// or after today.
func (r *RegimenRepository) ScheduleCandidates(ctx context.Context, today model.Date) ([]ScheduleCandidate, error) {
	return collectCandidates(r.scope.Query(ctx, selectScheduleCandidates, today.String()))
}

// PatientScheduleCandidates is ScheduleCandidates for one patient.
func (r *RegimenRepository) PatientScheduleCandidates(ctx context.Context, patientID model.ID, today model.Date) ([]ScheduleCandidate, error) {
	return collectCandidates(r.scope.Query(ctx, selectPatientScheduleCandidates, patientID.Bytes(), today.String()))
}

// ScheduledDose is a future dose the scheduler owns.
type ScheduledDose struct {
	ID          model.ID
	ScheduledAt time.Time
}

// Only doses the scheduler may still change: its own, not yet reminded, not
// yet answered.
const selectFutureScheduledDoses = `
SELECT id, scheduled_at FROM dose_events
 WHERE tenant_id = :tenant AND regimen_id = ? AND scheduled_at >= ?
   AND source = 'schedule' AND status = 'pending' AND reminded_at IS NULL
 ORDER BY scheduled_at`

func (r *RegimenRepository) FutureScheduledDoses(ctx context.Context, regimenID model.ID, from time.Time) ([]ScheduledDose, error) {
	rows, err := r.scope.Query(ctx, selectFutureScheduledDoses, regimenID.Bytes(), from.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledDose
	for rows.Next() {
		var d ScheduledDose
		if err := rows.Scan(&d.ID, &d.ScheduledAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const deleteScheduledDose = `
DELETE FROM dose_events
 WHERE tenant_id = :tenant AND id = ? AND source = 'schedule' AND status = 'pending' AND reminded_at IS NULL`

// DeleteScheduledDose removes a dose the schedule no longer wants. A dose
// that has been reminded or answered in the meantime is left alone.
func (r *RegimenRepository) DeleteScheduledDose(ctx context.Context, id model.ID) (bool, error) {
	res, err := r.scope.Exec(ctx, deleteScheduledDose, id.Bytes())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

const insertScheduledDose = `
INSERT INTO dose_events (tenant_id, id, patient_id, regimen_id, scheduled_at, source, status, created_at)
VALUES (:tenant, ?, ?, ?, ?, 'schedule', 'pending', ?)`

// AddScheduledDoses inserts the scheduler's doses; one already there for
// the same regimen and moment is skipped. Returns how many were added.
func (r *RegimenRepository) AddScheduledDoses(ctx context.Context, g *model.Regimen, times []time.Time, now time.Time) (int, error) {
	added := 0
	err := r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		added = 0 // the transaction may be retried
		for _, at := range times {
			id, err := model.NewID()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, insertScheduledDose, id.Bytes(), g.PatientID.Bytes(), g.ID.Bytes(), at.UTC(), now); err != nil {
				if errors.Is(translate(err), ErrDuplicate) {
					continue
				}
				return translate(err)
			}
			added++
		}
		return nil
	})
	return added, err
}

// Stopping, pausing or completing a regimen withdraws its future doses at
// once (BR-4.10), whoever added them: nobody should be reminded about a
// medicine they have been told to stop. Doses already reminded or answered
// stay, as a record of what happened.
const deleteFutureRegimenDoses = `
DELETE FROM dose_events
 WHERE tenant_id = :tenant AND regimen_id = ? AND scheduled_at > ? AND status = 'pending' AND reminded_at IS NULL`

// SetStatusAndWithdraw changes a regimen's status and, unless it is now
// active, removes its future doses, in one transaction.
func (r *RegimenRepository) SetStatusAndWithdraw(ctx context.Context, id model.ID, from, to model.RegimenStatus, reason string, by model.ID, now time.Time) (int64, error) {
	var withdrawn int64
	err := r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		withdrawn = 0
		if err := expectOne(tx.Exec(ctx, updateRegimenStatus, string(to), nullString(reason), by.Bytes(), now, now,
			id.Bytes(), string(from))); err != nil {
			return err
		}
		if to == model.RegimenActive {
			return nil
		}
		res, err := tx.Exec(ctx, deleteFutureRegimenDoses, id.Bytes(), now)
		if err != nil {
			return err
		}
		withdrawn, err = res.RowsAffected()
		return err
	})
	return withdrawn, err
}

// The same clean-up for regimens that stopped being active some other way,
// or before this code existed.
const deleteFutureInactiveDoses = `
DELETE d FROM dose_events d
  JOIN regimens r ON r.tenant_id = d.tenant_id AND r.id = d.regimen_id
 WHERE d.tenant_id = :tenant AND d.scheduled_at > ? AND d.status = 'pending' AND d.reminded_at IS NULL
   AND r.status <> 'active'`

func (r *RegimenRepository) WithdrawInactiveDoses(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.scope.Exec(ctx, deleteFutureInactiveDoses, now.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Closing the answer window. An unanswered dose stays pending, and pending
// doses are left out of adherence, so without this a patient who ignores
// every reminder would look the same as one whose doses are not due yet.
const markOverdueMissed = `
UPDATE dose_events SET status = 'missed'
 WHERE tenant_id = :tenant AND status = 'pending' AND scheduled_at < ?
 LIMIT 5000`

// CloseOverdue marks doses due before cutoff and still unanswered as
// missed, in batches.
func (r *RegimenRepository) CloseOverdue(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for {
		res, err := r.scope.Exec(ctx, markOverdueMissed, cutoff.UTC())
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < 5000 {
			return total, nil
		}
	}
}
