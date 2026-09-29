package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// LabRepository stores laboratory results as reported.
type LabRepository struct {
	scope *tenant.Scope
}

func NewLabRepository(s *tenant.Scope) *LabRepository { return &LabRepository{scope: s} }

// labFrom is a fragment; each query prefixes SELECT and adds its own tenant
// predicate.
const labFrom = ` r.id, r.patient_id, CONCAT(p.first_name, ' ', p.last_name), r.lab_name, r.lab_reference, r.panel,
       r.specimen_collected_at, r.reported_at, r.status, r.has_abnormal, r.has_critical, r.comment,
       r.recorded_by, r.recorded_at, r.acknowledged_at, r.acknowledged_by, r.acknowledgement_note
  FROM lab_results r
  JOIN patients p ON p.tenant_id = r.tenant_id AND p.id = r.patient_id`

func scanLab(sc scanner) (*model.LabResult, error) {
	var l model.LabResult
	var ref, comment, ackNote sql.Null[string]
	var collected, ackAt sql.Null[time.Time]
	var ackBy sql.Null[model.ID]
	err := sc.Scan(&l.ID, &l.PatientID, &l.PatientName, &l.LabName, &ref, &l.Panel,
		&collected, &l.ReportedAt, &l.Status, &l.HasAbnormal, &l.HasCritical, &comment,
		&l.RecordedBy, &l.RecordedAt, &ackAt, &ackBy, &ackNote)
	if err != nil {
		return nil, translate(err)
	}
	l.LabReference, l.Comment, l.AcknowledgementNote = ref.V, comment.V, ackNote.V
	l.SpecimenCollectedAt, l.AcknowledgedAt, l.AcknowledgedBy = timePtr(collected), timePtr(ackAt), idPtr(ackBy)
	l.Items = []model.LabResultItem{}
	return &l, nil
}

const insertLabResult = `
INSERT INTO lab_results (tenant_id, id, patient_id, lab_name, lab_reference, panel, specimen_collected_at, reported_at,
       status, has_abnormal, has_critical, comment, recorded_by, recorded_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const insertLabItem = `
INSERT INTO lab_result_items (tenant_id, id, result_id, line_no, analyte, value, unit, reference_range, abnormal_flag)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?)`

// Create stores a result and its analytes together.
func (r *LabRepository) Create(ctx context.Context, l *model.LabResult) error {
	return r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		if _, err := tx.Exec(ctx, insertLabResult, l.ID.Bytes(), l.PatientID.Bytes(), l.LabName, nullString(l.LabReference),
			l.Panel, nullTime(l.SpecimenCollectedAt), l.ReportedAt, string(l.Status), l.HasAbnormal, l.HasCritical,
			nullString(l.Comment), l.RecordedBy.Bytes(), l.RecordedAt); err != nil {
			return translate(err)
		}
		for _, it := range l.Items {
			if _, err := tx.Exec(ctx, insertLabItem, it.ID.Bytes(), l.ID.Bytes(), it.LineNo, it.Analyte, it.Value,
				nullString(it.Unit), nullString(it.ReferenceRange), string(it.AbnormalFlag)); err != nil {
				return translate(err)
			}
		}
		return nil
	})
}

const labItemColumns = `i.id, i.result_id, i.line_no, i.analyte, i.value, i.unit, i.reference_range, i.abnormal_flag`

func collectLabItems(rows *sql.Rows, err error) (map[model.ID][]model.LabResultItem, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[model.ID][]model.LabResultItem{}
	for rows.Next() {
		var it model.LabResultItem
		var resultID model.ID
		var unit, rng sql.Null[string]
		if err := rows.Scan(&it.ID, &resultID, &it.LineNo, &it.Analyte, &it.Value, &unit, &rng, &it.AbnormalFlag); err != nil {
			return nil, err
		}
		it.Unit, it.ReferenceRange = unit.V, rng.V
		out[resultID] = append(out[resultID], it)
	}
	return out, rows.Err()
}

// labListing names a pair of statements: one for the results and one that
// fetches the analytes for exactly those results. They live in maps of
// constants so every statement is still visible to the isolation tests.
type labListing int

const (
	labOne labListing = iota
	labForPatient
	labInbox
)

var labResultQueries = map[labListing]string{
	labOne:        selectLab,
	labForPatient: selectPatientLabs,
	labInbox:      selectInbox,
}

var labItemQueries = map[labListing]string{
	labOne:        selectLabItems,
	labForPatient: selectPatientLabItems,
	labInbox:      selectInboxItems,
}

// listWithItems runs a results query and its matching items query in one
// transaction, so both see the same snapshot and no result comes back with
// its analytes missing.
func (r *LabRepository) listWithItems(ctx context.Context, which labListing, args ...any) ([]model.LabResult, error) {
	var out []model.LabResult
	err := r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		rows, err := tx.Query(ctx, labResultQueries[which], args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = []model.LabResult{}
		for rows.Next() {
			l, err := scanLab(rows)
			if err != nil {
				return err
			}
			out = append(out, *l)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		items, err := collectLabItems(tx.Query(ctx, labItemQueries[which], args...))
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

const selectLab = `SELECT` + labFrom + ` WHERE r.tenant_id = :tenant AND r.id = ?`

const selectLabItems = `SELECT ` + labItemColumns + `
  FROM lab_result_items i
 WHERE i.tenant_id = :tenant AND i.result_id = ?
 ORDER BY i.line_no`

func (r *LabRepository) Get(ctx context.Context, id model.ID) (*model.LabResult, error) {
	list, err := r.listWithItems(ctx, labOne, id.Bytes())
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

const selectPatientLabs = `SELECT` + labFrom + `
 WHERE r.tenant_id = :tenant AND r.patient_id = ?
 ORDER BY r.reported_at DESC, r.id
 LIMIT ?`

// The items query selects the same results the same way, as a derived table
// (MySQL does not allow LIMIT inside IN), so items arrive for exactly the
// results listed, in one round trip.
const selectPatientLabItems = `SELECT ` + labItemColumns + `
  FROM lab_result_items i
  JOIN (SELECT r.id FROM lab_results r
         WHERE r.tenant_id = :tenant AND r.patient_id = ?
         ORDER BY r.reported_at DESC, r.id
         LIMIT ?) sel ON sel.id = i.result_id
 WHERE i.tenant_id = :tenant
 ORDER BY i.result_id, i.line_no`

// ForPatient lists a patient's results, newest first, each with its analytes.
func (r *LabRepository) ForPatient(ctx context.Context, patientID model.ID, limit int) ([]model.LabResult, error) {
	return r.listWithItems(ctx, labForPatient, patientID.Bytes(), limit)
}

// The inbox: results no clinician has acknowledged, critical first, then
// abnormal, oldest first within each — the one waiting longest is the one
// most likely to have been forgotten.
const selectInbox = `SELECT` + labFrom + `
 WHERE r.tenant_id = :tenant AND r.acknowledged_at IS NULL AND (? = 0 OR r.has_abnormal = 1)
 ORDER BY r.has_critical DESC, r.has_abnormal DESC, r.reported_at ASC, r.id
 LIMIT ?`

const selectInboxItems = `SELECT ` + labItemColumns + `
  FROM lab_result_items i
  JOIN (SELECT r.id FROM lab_results r
         WHERE r.tenant_id = :tenant AND r.acknowledged_at IS NULL AND (? = 0 OR r.has_abnormal = 1)
         ORDER BY r.has_critical DESC, r.has_abnormal DESC, r.reported_at ASC, r.id
         LIMIT ?) sel ON sel.id = i.result_id
 WHERE i.tenant_id = :tenant
 ORDER BY i.result_id, i.line_no`

// Inbox lists unacknowledged results across the practice.
func (r *LabRepository) Inbox(ctx context.Context, abnormalOnly bool, limit int) ([]model.LabResult, error) {
	return r.listWithItems(ctx, labInbox, abnormalOnly, limit)
}

const acknowledgeLab = `
UPDATE lab_results SET acknowledged_at = ?, acknowledged_by = ?, acknowledgement_note = ?
 WHERE tenant_id = :tenant AND id = ? AND acknowledged_at IS NULL`

// Acknowledge records that a clinician has seen a result. ErrConflict means
// someone already has.
func (r *LabRepository) Acknowledge(ctx context.Context, id, by model.ID, note string, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, acknowledgeLab, now, by.Bytes(), nullString(note), id.Bytes()))
}

const countUnacknowledgedAbnormal = `
SELECT COUNT(*) FROM lab_results
 WHERE tenant_id = :tenant AND patient_id = ? AND acknowledged_at IS NULL AND has_abnormal = 1`

func (r *LabRepository) UnacknowledgedAbnormal(ctx context.Context, patientID model.ID) (int, error) {
	var n int
	err := r.scope.QueryRow(ctx, countUnacknowledgedAbnormal, patientID.Bytes()).Scan(&n)
	return n, translate(err)
}
