package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// EncounterRepository stores consultation notes, their amendments and the
// AI drafts made for them.
type EncounterRepository struct {
	scope *tenant.Scope
}

func NewEncounterRepository(s *tenant.Scope) *EncounterRepository {
	return &EncounterRepository{scope: s}
}

// encounterFrom is a fragment, not a statement: each query below prefixes
// SELECT and adds its own tenant predicate.
const encounterFrom = ` e.id, e.patient_id, e.clinician_id, u.full_name, e.encounter_type, e.encounter_at,
       e.presenting_complaint, e.history, e.examination, e.assessment, e.plan, e.vitals_json, e.status,
       e.signed_at, e.signed_by, e.ai_assisted, e.amended_at, e.created_at, e.updated_at
  FROM encounters e
  JOIN users u ON u.tenant_id = e.tenant_id AND u.id = e.clinician_id`

func scanEncounter(sc scanner) (*model.Encounter, error) {
	var e model.Encounter
	var complaint, history, exam, assessment, plan, vitals sql.Null[string]
	var signedAt, amendedAt sql.Null[time.Time]
	var signedBy sql.Null[model.ID]
	err := sc.Scan(&e.ID, &e.PatientID, &e.ClinicianID, &e.ClinicianName, &e.Type, &e.EncounterAt,
		&complaint, &history, &exam, &assessment, &plan, &vitals, &e.Status,
		&signedAt, &signedBy, &e.AIAssisted, &amendedAt, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, translate(err)
	}
	e.PresentingComplaint, e.History, e.Examination, e.Assessment, e.Plan = complaint.V, history.V, exam.V, assessment.V, plan.V
	if vitals.Valid && vitals.V != "" && vitals.V != "null" {
		var v model.Vitals
		if err := json.Unmarshal([]byte(vitals.V), &v); err != nil {
			return nil, fmt.Errorf("encounter %s: unreadable vitals: %w", e.ID, err)
		}
		e.Vitals = &v
	}
	e.SignedAt, e.SignedBy, e.AmendedAt = timePtr(signedAt), idPtr(signedBy), timePtr(amendedAt)
	return &e, nil
}

func vitalsJSON(v *model.Vitals) (any, error) {
	if v.IsEmpty() {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

const insertEncounter = `
INSERT INTO encounters (tenant_id, id, patient_id, clinician_id, encounter_type, encounter_at,
       presenting_complaint, history, examination, assessment, plan, vitals_json, status, created_at, updated_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'draft', ?, ?)`

func (r *EncounterRepository) Create(ctx context.Context, e *model.Encounter) error {
	vitals, err := vitalsJSON(e.Vitals)
	if err != nil {
		return err
	}
	_, err = r.scope.Exec(ctx, insertEncounter, e.ID.Bytes(), e.PatientID.Bytes(), e.ClinicianID.Bytes(), string(e.Type),
		e.EncounterAt, nullString(e.PresentingComplaint), nullString(e.History), nullString(e.Examination),
		nullString(e.Assessment), nullString(e.Plan), vitals, e.CreatedAt, e.UpdatedAt)
	return translate(err)
}

const selectEncounter = `SELECT` + encounterFrom + ` WHERE e.tenant_id = :tenant AND e.id = ?`

func (r *EncounterRepository) Get(ctx context.Context, id model.ID) (*model.Encounter, error) {
	return scanEncounter(r.scope.QueryRow(ctx, selectEncounter, id.Bytes()))
}

const selectPatientEncounters = `SELECT` + encounterFrom + `
 WHERE e.tenant_id = :tenant AND e.patient_id = ?
 ORDER BY e.encounter_at DESC, e.created_at DESC
 LIMIT ?`

const selectPatientSignedEncounters = `SELECT` + encounterFrom + `
 WHERE e.tenant_id = :tenant AND e.patient_id = ? AND e.status = 'signed'
 ORDER BY e.encounter_at DESC, e.created_at DESC
 LIMIT ?`

// ForPatient lists a patient's notes, newest first. signedOnly leaves out
// unfinished drafts, which are not yet part of the record.
func (r *EncounterRepository) ForPatient(ctx context.Context, patientID model.ID, signedOnly bool, limit int) ([]model.Encounter, error) {
	var rows *sql.Rows
	var err error
	if signedOnly {
		rows, err = r.scope.Query(ctx, selectPatientSignedEncounters, patientID.Bytes(), limit)
	} else {
		rows, err = r.scope.Query(ctx, selectPatientEncounters, patientID.Bytes(), limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Encounter{}
	for rows.Next() {
		e, err := scanEncounter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// The status and the author are part of the WHERE clause: a signed note, or
// someone else's draft, simply does not match.
const updateEncounterDraft = `
UPDATE encounters
   SET encounter_type = ?, encounter_at = ?, presenting_complaint = ?, history = ?, examination = ?,
       assessment = ?, plan = ?, vitals_json = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'draft' AND clinician_id = ?`

// UpdateDraft replaces a draft's content. ErrConflict means it is signed or
// not this author's.
func (r *EncounterRepository) UpdateDraft(ctx context.Context, e *model.Encounter, author model.ID) error {
	vitals, err := vitalsJSON(e.Vitals)
	if err != nil {
		return err
	}
	return expectOne(r.scope.Exec(ctx, updateEncounterDraft, string(e.Type), e.EncounterAt,
		nullString(e.PresentingComplaint), nullString(e.History), nullString(e.Examination),
		nullString(e.Assessment), nullString(e.Plan), vitals, e.UpdatedAt, e.ID.Bytes(), author.Bytes()))
}

// A note cannot be signed without an assessment or a plan, and the rule is
// in the statement as well as the service.
const signEncounter = `
UPDATE encounters SET status = 'signed', signed_at = ?, signed_by = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'draft' AND clinician_id = ?
   AND (COALESCE(assessment, '') <> '' OR COALESCE(plan, '') <> '')`

func (r *EncounterRepository) Sign(ctx context.Context, id, author model.ID, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, signEncounter, now, author.Bytes(), now, id.Bytes(), author.Bytes()))
}

const markAIAssisted = `UPDATE encounters SET ai_assisted = 1 WHERE tenant_id = :tenant AND id = ?`

// MarkAIAssisted records, permanently, that a model drafted text for a note.
func (r *EncounterRepository) MarkAIAssisted(ctx context.Context, id model.ID) error {
	return expectFound(r.scope.Exec(ctx, markAIAssisted, id.Bytes()))
}

// --- amendments -----------------------------------------------------------------

const lockSignedEncounter = `
SELECT status, presenting_complaint, history, examination, assessment, plan
  FROM encounters
 WHERE tenant_id = :tenant AND id = ?
 FOR UPDATE`

const insertAmendment = `
INSERT INTO encounter_amendments (tenant_id, id, encounter_id, field, previous_value, new_value, reason, amended_by, amended_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?)`

// amendmentStatements holds one complete statement per amendable field.
// Building the column name into the SQL would be safe with a whitelist, but
// invisible to the tests that check every statement names the tenant — and
// a safe query that cannot be verified eventually becomes an unsafe one
// nobody notices.
var amendmentStatements = map[model.NoteField]string{
	model.FieldPresentingComplaint: `UPDATE encounters SET presenting_complaint = ?, amended_at = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'signed'`,
	model.FieldHistory: `UPDATE encounters SET history = ?, amended_at = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'signed'`,
	model.FieldExamination: `UPDATE encounters SET examination = ?, amended_at = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'signed'`,
	model.FieldAssessment: `UPDATE encounters SET assessment = ?, amended_at = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'signed'`,
	model.FieldPlan: `UPDATE encounters SET plan = ?, amended_at = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'signed'`,
}

// Amend changes one field of a signed note, recording the previous value.
// The row is locked while the previous value is read, so two amendments to
// the same field cannot both record the same "before".
// ErrConflict means the note is not signed.
func (r *EncounterRepository) Amend(ctx context.Context, a *model.Amendment) error {
	if _, ok := amendmentStatements[a.Field]; !ok {
		return fmt.Errorf("repository: %q is not an amendable field", a.Field)
	}
	return r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		var status string
		var fields [5]sql.Null[string]
		err := tx.QueryRow(ctx, lockSignedEncounter, a.EncounterID.Bytes()).Scan(&status,
			&fields[0], &fields[1], &fields[2], &fields[3], &fields[4])
		if err != nil {
			return translate(err)
		}
		if model.EncounterStatus(status) != model.EncounterSigned {
			return ErrConflict
		}
		current := model.Encounter{
			PresentingComplaint: fields[0].V, History: fields[1].V, Examination: fields[2].V,
			Assessment: fields[3].V, Plan: fields[4].V,
		}
		a.PreviousValue = current.Field(a.Field)
		if _, err := tx.Exec(ctx, insertAmendment, a.ID.Bytes(), a.EncounterID.Bytes(), string(a.Field),
			nullString(a.PreviousValue), nullString(a.NewValue), a.Reason, a.AmendedBy.Bytes(), a.AmendedAt); err != nil {
			return translate(err)
		}
		return expectOne(tx.Exec(ctx, amendmentStatements[a.Field], nullString(a.NewValue), a.AmendedAt, a.AmendedAt,
			a.EncounterID.Bytes()))
	})
}

const selectAmendments = `
SELECT a.id, a.encounter_id, a.field, a.previous_value, a.new_value, a.reason, a.amended_by, u.full_name, a.amended_at
  FROM encounter_amendments a
  JOIN users u ON u.tenant_id = a.tenant_id AND u.id = a.amended_by
 WHERE a.tenant_id = :tenant AND a.encounter_id = ?
 ORDER BY a.seq`

// Amendments lists changes to a note, oldest first.
func (r *EncounterRepository) Amendments(ctx context.Context, encounterID model.ID) ([]model.Amendment, error) {
	rows, err := r.scope.Query(ctx, selectAmendments, encounterID.Bytes())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Amendment{}
	for rows.Next() {
		var a model.Amendment
		var prev, next sql.Null[string]
		if err := rows.Scan(&a.ID, &a.EncounterID, &a.Field, &prev, &next, &a.Reason, &a.AmendedBy,
			&a.AmendedByName, &a.AmendedAt); err != nil {
			return nil, err
		}
		a.PreviousValue, a.NewValue = prev.V, next.V
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- review and summary helpers ---------------------------------------------------

const lastSignedEncounter = `
SELECT MAX(encounter_at) FROM encounters
 WHERE tenant_id = :tenant AND patient_id = ? AND status = 'signed'`

// LastReview returns when the patient was last seen in a signed note, or
// nil if never. Unfinished drafts do not count: a draft from three months
// ago is not evidence that the patient was reviewed.
func (r *EncounterRepository) LastReview(ctx context.Context, patientID model.ID) (*time.Time, error) {
	var at sql.Null[time.Time]
	if err := r.scope.QueryRow(ctx, lastSignedEncounter, patientID.Bytes()).Scan(&at); err != nil {
		return nil, translate(err)
	}
	return timePtr(at), nil
}

const countOpenDrafts = `SELECT COUNT(*) FROM encounters WHERE tenant_id = :tenant AND patient_id = ? AND status = 'draft'`

func (r *EncounterRepository) OpenDrafts(ctx context.Context, patientID model.ID) (int, error) {
	var n int
	err := r.scope.QueryRow(ctx, countOpenDrafts, patientID.Bytes()).Scan(&n)
	return n, translate(err)
}

// --- AI drafts ------------------------------------------------------------------

const insertNoteDraft = `
INSERT INTO encounter_note_drafts (tenant_id, id, encounter_id, dictation, context_json, draft_json, model, status,
       error, input_tokens, output_tokens, created_by, created_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// SaveDraft stores a draft beside the note. contextJSON is the background
// that was sent with the dictation.
func (r *EncounterRepository) SaveDraft(ctx context.Context, d *model.NoteDraft, contextJSON []byte) error {
	var draft, ctxJSON any
	if d.Draft != nil {
		b, err := json.Marshal(d.Draft)
		if err != nil {
			return err
		}
		draft = string(b)
	}
	if len(contextJSON) > 0 {
		ctxJSON = string(contextJSON)
	}
	_, err := r.scope.Exec(ctx, insertNoteDraft, d.ID.Bytes(), d.EncounterID.Bytes(), d.Dictation, ctxJSON, draft,
		d.Model, string(d.Status), nullString(truncate(d.Error, 500)), d.InputTokens, d.OutputTokens,
		d.CreatedBy.Bytes(), d.CreatedAt)
	return translate(err)
}

const selectNoteDrafts = `
SELECT id, encounter_id, dictation, draft_json, model, status, error, input_tokens, output_tokens,
       created_by, created_at, flagged_at, flagged_by, flag_reason
  FROM encounter_note_drafts
 WHERE tenant_id = :tenant AND encounter_id = ?
 ORDER BY created_at DESC`

func (r *EncounterRepository) Drafts(ctx context.Context, encounterID model.ID) ([]model.NoteDraft, error) {
	rows, err := r.scope.Query(ctx, selectNoteDrafts, encounterID.Bytes())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.NoteDraft{}
	for rows.Next() {
		var d model.NoteDraft
		var draft, errText, reason sql.Null[string]
		var in, outTok sql.Null[int]
		var flaggedAt sql.Null[time.Time]
		var flaggedBy sql.Null[model.ID]
		if err := rows.Scan(&d.ID, &d.EncounterID, &d.Dictation, &draft, &d.Model, &d.Status, &errText, &in, &outTok,
			&d.CreatedBy, &d.CreatedAt, &flaggedAt, &flaggedBy, &reason); err != nil {
			return nil, err
		}
		if draft.Valid && draft.V != "" {
			var c model.DraftContent
			if err := json.Unmarshal([]byte(draft.V), &c); err != nil {
				return nil, fmt.Errorf("draft %s: unreadable content: %w", d.ID, err)
			}
			d.Draft = &c
		}
		d.Error, d.InputTokens, d.OutputTokens = errText.V, in.V, outTok.V
		d.FlaggedAt, d.FlaggedBy, d.FlagReason = timePtr(flaggedAt), idPtr(flaggedBy), reason.V
		out = append(out, d)
	}
	return out, rows.Err()
}

const countDraftsSince = `SELECT COUNT(*) FROM encounter_note_drafts WHERE tenant_id = :tenant AND created_at >= ?`

// DraftsSince counts drafting requests since a moment, for the per-practice
// daily limit (BR-5.18). Failed attempts count: they may still have cost.
func (r *EncounterRepository) DraftsSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := r.scope.QueryRow(ctx, countDraftsSince, since.UTC()).Scan(&n)
	return n, translate(err)
}

const flagNoteDraft = `
UPDATE encounter_note_drafts SET flagged_at = ?, flagged_by = ?, flag_reason = ?
 WHERE tenant_id = :tenant AND encounter_id = ? AND id = ? AND flagged_at IS NULL`

// FlagDraft marks a draft as incorrect or unsafe for review (BR-5.15).
// ErrConflict means it does not exist on this note or is already flagged.
func (r *EncounterRepository) FlagDraft(ctx context.Context, encounterID, draftID, by model.ID, reason string, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, flagNoteDraft, now, by.Bytes(), reason, encounterID.Bytes(), draftID.Bytes()))
}
