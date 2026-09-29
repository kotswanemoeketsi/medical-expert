package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// DocumentRepository stores referral letters and medical certificates.
type DocumentRepository struct {
	scope *tenant.Scope
}

func NewDocumentRepository(s *tenant.Scope) *DocumentRepository { return &DocumentRepository{scope: s} }

const documentColumns = `id, patient_id, doc_type, status, document_number, author_id, encounter_id, body_json,
       rendered_text, basis, examined_on, valid_from, valid_to, discloses_diagnosis, backdate_reason,
       issued_at, issued_by, cancelled_at, cancelled_by, cancel_reason, created_at, updated_at`

func scanDocument(sc scanner) (*model.ClinicalDocument, error) {
	var d model.ClinicalDocument
	var number, rendered, basis, backdate, cancelReason sql.Null[string]
	var encounter, issuedBy, cancelledBy sql.Null[model.ID]
	var examined, from, to sql.Null[model.Date]
	var issuedAt, cancelledAt sql.Null[time.Time]
	var body string
	err := sc.Scan(&d.ID, &d.PatientID, &d.Type, &d.Status, &number, &d.AuthorID, &encounter, &body,
		&rendered, &basis, &examined, &from, &to, &d.DisclosesDiagnosis, &backdate,
		&issuedAt, &issuedBy, &cancelledAt, &cancelledBy, &cancelReason, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, translate(err)
	}
	d.Body = []byte(body)
	d.Number, d.RenderedText, d.Basis = number.V, rendered.V, model.CertificateBasis(basis.V)
	d.BackdateReason, d.CancelReason = backdate.V, cancelReason.V
	d.EncounterID, d.IssuedBy, d.CancelledBy = idPtr(encounter), idPtr(issuedBy), idPtr(cancelledBy)
	d.IssuedAt, d.CancelledAt = timePtr(issuedAt), timePtr(cancelledAt)
	for _, pair := range []struct {
		src sql.Null[model.Date]
		dst **model.Date
	}{{examined, &d.ExaminedOn}, {from, &d.ValidFrom}, {to, &d.ValidTo}} {
		if pair.src.Valid {
			v := pair.src.V
			*pair.dst = &v
		}
	}
	return &d, nil
}

const insertDocument = `
INSERT INTO clinical_documents (tenant_id, id, patient_id, doc_type, status, author_id, encounter_id, body_json, basis,
       examined_on, valid_from, valid_to, discloses_diagnosis, backdate_reason, created_at, updated_at)
VALUES (:tenant, ?, ?, ?, 'draft', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func (r *DocumentRepository) Create(ctx context.Context, d *model.ClinicalDocument) error {
	_, err := r.scope.Exec(ctx, insertDocument, d.ID.Bytes(), d.PatientID.Bytes(), string(d.Type), d.AuthorID.Bytes(),
		nullID(d.EncounterID), string(d.Body), nullString(string(d.Basis)), nullDate(d.ExaminedOn), nullDate(d.ValidFrom),
		nullDate(d.ValidTo), d.DisclosesDiagnosis, nullString(d.BackdateReason), d.CreatedAt, d.UpdatedAt)
	return translate(err)
}

const selectDocument = `SELECT ` + documentColumns + ` FROM clinical_documents WHERE tenant_id = :tenant AND id = ?`

func (r *DocumentRepository) Get(ctx context.Context, id model.ID) (*model.ClinicalDocument, error) {
	return scanDocument(r.scope.QueryRow(ctx, selectDocument, id.Bytes()))
}

const selectPatientDocuments = `SELECT ` + documentColumns + `
  FROM clinical_documents
 WHERE tenant_id = :tenant AND patient_id = ?
 ORDER BY created_at DESC
 LIMIT ?`

func (r *DocumentRepository) ForPatient(ctx context.Context, patientID model.ID, limit int) ([]model.ClinicalDocument, error) {
	rows, err := r.scope.Query(ctx, selectPatientDocuments, patientID.Bytes(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ClinicalDocument{}
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// The upsert takes a row lock on the practice's counter for this type and
// year, held until the transaction ends, so numbers are never handed out
// twice and never skipped by a rolled-back issue.
const bumpDocumentSequence = `
INSERT INTO document_sequences (tenant_id, doc_type, year, last_number)
VALUES (:tenant, ?, ?, 1)
ON DUPLICATE KEY UPDATE last_number = last_number + 1`

const readDocumentSequence = `
SELECT last_number FROM document_sequences WHERE tenant_id = :tenant AND doc_type = ? AND year = ?`

const issueDocument = `
UPDATE clinical_documents
   SET status = 'issued', document_number = ?, rendered_text = ?, issued_at = ?, issued_by = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'draft'`

// Issue numbers and freezes a draft. render receives the number and must
// return the final text; it runs inside the transaction so a document is
// never numbered without being rendered, or rendered without a number.
// ErrConflict means the document is not a draft.
func (r *DocumentRepository) Issue(ctx context.Context, d *model.ClinicalDocument, by model.ID, now time.Time,
	year int, render func(number string) (string, error)) (string, string, error) {
	var number, text string
	err := r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		if _, err := tx.Exec(ctx, bumpDocumentSequence, string(d.Type), year); err != nil {
			return translate(err)
		}
		var n int
		if err := tx.QueryRow(ctx, readDocumentSequence, string(d.Type), year).Scan(&n); err != nil {
			return translate(err)
		}
		number = fmt.Sprintf("%s-%d-%04d", d.Type.NumberPrefix(), year, n)
		var err error
		if text, err = render(number); err != nil {
			return err
		}
		return expectOne(tx.Exec(ctx, issueDocument, number, text, now, by.Bytes(), now, d.ID.Bytes()))
	})
	return number, text, err
}

const cancelDocument = `
UPDATE clinical_documents SET status = 'cancelled', cancelled_at = ?, cancelled_by = ?, cancel_reason = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = ?`

// Cancel withdraws a document from the status it was read in. An issued
// document keeps its number and text: it was issued, and the record shows
// that it was later withdrawn and why.
func (r *DocumentRepository) Cancel(ctx context.Context, id model.ID, from model.DocumentStatus, reason string, by model.ID, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, cancelDocument, now, by.Bytes(), nullString(reason), now, id.Bytes(), string(from)))
}
