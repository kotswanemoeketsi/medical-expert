package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// PrescriptionRepository stores repeat prescriptions and their items.
type PrescriptionRepository struct {
	scope *tenant.Scope
}

func NewPrescriptionRepository(s *tenant.Scope) *PrescriptionRepository {
	return &PrescriptionRepository{scope: s}
}

// prescriptionFrom is a fragment; each query prefixes SELECT and adds its
// own tenant predicate.
const prescriptionFrom = ` p.id, p.patient_id, p.prescriber_id, u.full_name, p.repeat_of_id, p.issued_at, p.valid_until,
       p.repeats_authorised, p.repeats_used, p.status, p.override_reason, p.notes, p.cancel_reason,
       p.cancelled_by, p.cancelled_at, p.created_at
  FROM prescriptions p
  JOIN users u ON u.tenant_id = p.tenant_id AND u.id = p.prescriber_id`

func scanPrescription(sc scanner) (*model.Prescription, error) {
	var p model.Prescription
	var repeatOf, cancelledBy sql.Null[model.ID]
	var override, notes, cancelReason sql.Null[string]
	var cancelledAt sql.Null[time.Time]
	err := sc.Scan(&p.ID, &p.PatientID, &p.PrescriberID, &p.PrescriberName, &repeatOf, &p.IssuedAt, &p.ValidUntil,
		&p.RepeatsAuthorised, &p.RepeatsUsed, &p.Status, &override, &notes, &cancelReason,
		&cancelledBy, &cancelledAt, &p.CreatedAt)
	if err != nil {
		return nil, translate(err)
	}
	p.RepeatOfID, p.CancelledBy, p.CancelledAt = idPtr(repeatOf), idPtr(cancelledBy), timePtr(cancelledAt)
	p.OverrideReason, p.Notes, p.CancelReason = override.V, notes.V, cancelReason.V
	p.Items = []model.PrescriptionItem{}
	return &p, nil
}

const insertPrescription = `
INSERT INTO prescriptions (tenant_id, id, patient_id, prescriber_id, repeat_of_id, issued_at, valid_until,
       repeats_authorised, repeats_used, status, override_reason, notes, created_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, 0, 'active', ?, ?, ?)`

const insertPrescriptionItem = `
INSERT INTO prescription_items (tenant_id, id, prescription_id, line_no, regimen_id, medication_id, medication_name,
       dose, route, frequency, quantity, instructions)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func insertScript(ctx context.Context, tx *tenant.Scope, p *model.Prescription) error {
	_, err := tx.Exec(ctx, insertPrescription, p.ID.Bytes(), p.PatientID.Bytes(), p.PrescriberID.Bytes(),
		nullID(p.RepeatOfID), p.IssuedAt, p.ValidUntil.String(), p.RepeatsAuthorised,
		nullString(p.OverrideReason), nullString(p.Notes), p.CreatedAt)
	if err != nil {
		return translate(err)
	}
	for i := range p.Items {
		it := &p.Items[i]
		if _, err := tx.Exec(ctx, insertPrescriptionItem, it.ID.Bytes(), p.ID.Bytes(), it.LineNo, nullID(it.RegimenID),
			nullID(it.MedicationID), it.MedicationName, it.Dose, it.Route, it.Frequency, it.Quantity,
			nullString(it.Instructions)); err != nil {
			return translate(err)
		}
	}
	return nil
}

// Create stores a new authorisation with its items, in one transaction.
func (r *PrescriptionRepository) Create(ctx context.Context, p *model.Prescription) error {
	return r.scope.InTx(ctx, func(tx *tenant.Scope) error { return insertScript(ctx, tx, p) })
}

// The bounds check is in the WHERE clause, so two people clicking "repeat"
// at the same moment cannot both spend the last repeat: only one UPDATE can
// match. A repeat of a repeat is not a thing; repeats count against the
// original authorisation only.
const useRepeat = `
UPDATE prescriptions SET repeats_used = repeats_used + 1
 WHERE tenant_id = :tenant AND id = ? AND repeat_of_id IS NULL AND status = 'active'
   AND repeats_used < repeats_authorised AND valid_until >= ?`

// Taken first, by primary key, so concurrent repeats queue on one record
// lock. Without it the conditional UPDATE could wait on a range lock that
// the other transaction's INSERT then needs, and InnoDB would call it a
// deadlock.
const lockPrescription = `SELECT id FROM prescriptions WHERE tenant_id = :tenant AND id = ? FOR UPDATE`

// IssueRepeat spends one repeat on the original and stores the repeat
// script, together. ErrConflict means no repeat could be spent: cancelled,
// expired or exhausted — the caller reads the original to say which.
func (r *PrescriptionRepository) IssueRepeat(ctx context.Context, originalID model.ID, today model.Date, repeat *model.Prescription) error {
	return r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		var locked model.ID
		if err := tx.QueryRow(ctx, lockPrescription, originalID.Bytes()).Scan(&locked); err != nil {
			return translate(err)
		}
		if err := expectOne(tx.Exec(ctx, useRepeat, originalID.Bytes(), today.String())); err != nil {
			return err
		}
		return insertScript(ctx, tx, repeat)
	})
}

const selectPrescription = `SELECT` + prescriptionFrom + ` WHERE p.tenant_id = :tenant AND p.id = ?`

const selectPrescriptionItems = `
SELECT id, prescription_id, line_no, regimen_id, medication_id, medication_name, dose, route, frequency, quantity, instructions
  FROM prescription_items
 WHERE tenant_id = :tenant AND prescription_id = ?
 ORDER BY line_no`

// Get returns a prescription with its items.
func (r *PrescriptionRepository) Get(ctx context.Context, id model.ID) (*model.Prescription, error) {
	p, err := scanPrescription(r.scope.QueryRow(ctx, selectPrescription, id.Bytes()))
	if err != nil {
		return nil, err
	}
	items, err := collectItems(r.scope.Query(ctx, selectPrescriptionItems, id.Bytes()))
	if err != nil {
		return nil, err
	}
	p.Items = items[p.ID]
	if p.Items == nil {
		p.Items = []model.PrescriptionItem{}
	}
	return p, nil
}

const selectPatientPrescriptions = `SELECT` + prescriptionFrom + `
 WHERE p.tenant_id = :tenant AND p.patient_id = ?
 ORDER BY p.issued_at DESC
 LIMIT ?`

// Items for the same patient in one query rather than one query per script.
const selectPatientPrescriptionItems = `
SELECT i.id, i.prescription_id, i.line_no, i.regimen_id, i.medication_id, i.medication_name, i.dose, i.route,
       i.frequency, i.quantity, i.instructions
  FROM prescription_items i
  JOIN prescriptions p ON p.tenant_id = i.tenant_id AND p.id = i.prescription_id
 WHERE i.tenant_id = :tenant AND p.patient_id = ?
 ORDER BY i.prescription_id, i.line_no`

// ForPatient lists a patient's scripts, newest first, each with its items.
func (r *PrescriptionRepository) ForPatient(ctx context.Context, patientID model.ID, limit int) ([]model.Prescription, error) {
	var out []model.Prescription
	err := r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		rows, err := tx.Query(ctx, selectPatientPrescriptions, patientID.Bytes(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = []model.Prescription{}
		for rows.Next() {
			p, err := scanPrescription(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		items, err := collectItems(tx.Query(ctx, selectPatientPrescriptionItems, patientID.Bytes()))
		if err != nil {
			return err
		}
		for i := range out {
			if list, ok := items[out[i].ID]; ok {
				out[i].Items = list
			}
		}
		return nil
	})
	return out, err
}

func collectItems(rows *sql.Rows, err error) (map[model.ID][]model.PrescriptionItem, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[model.ID][]model.PrescriptionItem{}
	for rows.Next() {
		var it model.PrescriptionItem
		var scriptID model.ID
		var regimen, medication sql.Null[model.ID]
		var instructions sql.Null[string]
		if err := rows.Scan(&it.ID, &scriptID, &it.LineNo, &regimen, &medication, &it.MedicationName, &it.Dose,
			&it.Route, &it.Frequency, &it.Quantity, &instructions); err != nil {
			return nil, err
		}
		it.RegimenID, it.MedicationID, it.Instructions = idPtr(regimen), idPtr(medication), instructions.V
		out[scriptID] = append(out[scriptID], it)
	}
	return out, rows.Err()
}

const cancelPrescription = `
UPDATE prescriptions SET status = 'cancelled', cancel_reason = ?, cancelled_by = ?, cancelled_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'active'`

// Cancel stops an authorisation. ErrConflict means it was not active.
func (r *PrescriptionRepository) Cancel(ctx context.Context, id model.ID, reason string, by model.ID, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, cancelPrescription, reason, by.Bytes(), now, id.Bytes()))
}

const expirePrescriptions = `
UPDATE prescriptions SET status = 'expired'
 WHERE tenant_id = :tenant AND status = 'active' AND valid_until < ?`

// ExpireBefore marks lapsed scripts expired. A convenience for listings
// only: repeats check the date directly, so a missed run cannot cause an
// expired script to be repeated.
func (r *PrescriptionRepository) ExpireBefore(ctx context.Context, today model.Date) (int64, error) {
	res, err := r.scope.Exec(ctx, expirePrescriptions, today.String())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
