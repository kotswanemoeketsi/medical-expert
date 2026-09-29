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
       d.scheduled_at, d.status, d.responded_at, d.response_channel, d.responded_by, d.skip_reason, d.created_at
  FROM dose_events d
  JOIN regimens r    ON r.tenant_id = d.tenant_id AND r.id = d.regimen_id
  JOIN medications m ON m.id = r.medication_id`

func scanDose(sc scanner) (*model.DoseEvent, error) {
	var d model.DoseEvent
	var med model.Medication
	var respondedAt sql.Null[time.Time]
	var channel, reason sql.Null[string]
	var by sql.Null[model.ID]
	if err := sc.Scan(&d.ID, &d.PatientID, &d.RegimenID, &med.GenericName, &med.BrandName, &med.Strength, &med.DosageForm,
		&d.ScheduledAt, &d.Status, &respondedAt, &channel, &by, &reason, &d.CreatedAt); err != nil {
		return nil, translate(err)
	}
	d.MedicationName = med.DisplayName()
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
