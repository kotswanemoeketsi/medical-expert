package controller

import (
	"net/http"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/service"
)

// ---------------------------------------------------------------------------
// Medication catalogue, regimens and doses
// ---------------------------------------------------------------------------

func (a *API) searchMedications(w http.ResponseWriter, r *http.Request) {
	limit, offset := queryInt(r, "limit", 25), queryInt(r, "offset", 0)
	items, err := principalFrom(r).Services.Medication.SearchCatalogue(r.Context(), r.URL.Query().Get("q"), limit, offset)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type regimenRequest struct {
	MedicationID model.ID        `json:"medication_id"`
	Dose         string          `json:"dose"`
	Route        model.Route     `json:"route"`
	Frequency    model.Frequency `json:"frequency"`
	Instructions string          `json:"instructions"`
	StartDate    *model.Date     `json:"start_date"`
	EndDate      *model.Date     `json:"end_date"`
}

func (a *API) createRegimen(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req regimenRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	g, err := principalFrom(r).Services.Medication.CreateRegimen(r.Context(), id, service.RegimenInput{
		MedicationID: req.MedicationID, Dose: req.Dose, Route: req.Route, Frequency: req.Frequency,
		Instructions: req.Instructions, StartDate: req.StartDate, EndDate: req.EndDate,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (a *API) listRegimens(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := principalFrom(r).Services.Medication.Regimens(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type regimenStatusRequest struct {
	Status model.RegimenStatus `json:"status"`
	Reason string              `json:"reason"`
}

func (a *API) setRegimenStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req regimenStatusRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	g, err := principalFrom(r).Services.Medication.SetRegimenStatus(r.Context(), id, req.Status, req.Reason)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

type scheduleRequest struct {
	Times []time.Time `json:"times"`
}

func (a *API) scheduleDoses(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req scheduleRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	n, err := principalFrom(r).Services.Medication.ScheduleDoses(r.Context(), id, req.Times)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": n})
}

func (a *API) listDoses(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	from, err := queryTime(r, "from")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	to, err := queryTime(r, "to")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := principalFrom(r).Services.Medication.Doses(r.Context(), id, from, to)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type doseResponseRequest struct {
	Status model.DoseStatus `json:"status"`
	Reason string           `json:"reason"`
}

func (a *API) respondToDose(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req doseResponseRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := principalFrom(r).Services.Medication.RespondToDose(r.Context(), id, service.DoseResponse{Status: req.Status, Reason: req.Reason})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ---------------------------------------------------------------------------
// Consultation notes
// ---------------------------------------------------------------------------

type encounterRequest struct {
	Type                model.EncounterType `json:"encounter_type"`
	EncounterAt         *time.Time          `json:"encounter_at"`
	PresentingComplaint string              `json:"presenting_complaint"`
	History             string              `json:"history"`
	Examination         string              `json:"examination"`
	Assessment          string              `json:"assessment"`
	Plan                string              `json:"plan"`
	Vitals              *model.Vitals       `json:"vitals"`
}

func (req *encounterRequest) input() service.EncounterInput {
	return service.EncounterInput{Type: req.Type, EncounterAt: req.EncounterAt,
		PresentingComplaint: req.PresentingComplaint, History: req.History, Examination: req.Examination,
		Assessment: req.Assessment, Plan: req.Plan, Vitals: req.Vitals}
}

func (a *API) createEncounter(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req encounterRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	e, err := principalFrom(r).Services.Encounters.Create(r.Context(), id, req.input())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (a *API) listEncounters(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := principalFrom(r).Services.Encounters.List(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func encounterView(v *service.EncounterView) map[string]any {
	return map[string]any{"encounter": v.Encounter, "amendments": v.Amendments}
}

func (a *API) getEncounter(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v, err := principalFrom(r).Services.Encounters.Get(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, encounterView(v))
}

func (a *API) updateEncounter(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req encounterRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	e, err := principalFrom(r).Services.Encounters.Update(r.Context(), id, req.input())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (a *API) signEncounter(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	e, err := principalFrom(r).Services.Encounters.Sign(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

type amendmentRequest struct {
	Field    model.NoteField `json:"field"`
	NewValue string          `json:"new_value"`
	Reason   string          `json:"reason"`
}

func (a *API) amendEncounter(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req amendmentRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	v, err := principalFrom(r).Services.Encounters.Amend(r.Context(), id, service.AmendmentInput{
		Field: req.Field, NewValue: req.NewValue, Reason: req.Reason,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, encounterView(v))
}

// DraftNotice travels with every draft, so it is never mistaken for a note.
const DraftNotice = "This draft only reorganises your dictation. It has not been checked for clinical accuracy and is not part of the record until you apply it to the note and sign."

type draftRequest struct {
	Dictation string `json:"dictation"`
}

func (a *API) draftNote(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req draftRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := principalFrom(r).Services.Encounters.DraftNote(r.Context(), id, req.Dictation)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"draft": d, "notice": DraftNotice})
}

func (a *API) listDrafts(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := principalFrom(r).Services.Encounters.Drafts(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "notice": DraftNotice})
}

func (a *API) flagDraft(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	draftID, err := pathID(r, "draftId")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req reasonRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := principalFrom(r).Services.Encounters.FlagDraft(r.Context(), id, draftID, req.Reason); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Prescriptions
// ---------------------------------------------------------------------------

type prescriptionItemRequest struct {
	RegimenID    model.ID `json:"regimen_id"`
	Quantity     string   `json:"quantity"`
	Instructions string   `json:"instructions"`
}

type prescriptionRequest struct {
	Items             []prescriptionItemRequest `json:"items"`
	RepeatsAuthorised int                       `json:"repeats_authorised"`
	ValidUntil        *model.Date               `json:"valid_until"`
	OverrideReason    string                    `json:"override_reason"`
	Notes             string                    `json:"notes"`
}

func issuedView(p *service.IssuedPrescription) map[string]any {
	return map[string]any{
		"prescription": p.Prescription, "recorded_allergies": p.RecordedAllergies,
		"recorded_conditions": p.RecordedConditions, "notice": p.Notice,
	}
}

func (a *API) issuePrescription(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req prescriptionRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	in := service.PrescriptionInput{RepeatsAuthorised: req.RepeatsAuthorised, ValidUntil: req.ValidUntil,
		OverrideReason: req.OverrideReason, Notes: req.Notes}
	for _, it := range req.Items {
		in.Items = append(in.Items, service.PrescriptionItemInput{RegimenID: it.RegimenID, Quantity: it.Quantity, Instructions: it.Instructions})
	}
	p, err := principalFrom(r).Services.Prescriptions.Issue(r.Context(), id, in)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, issuedView(p))
}

func (a *API) listPrescriptions(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := principalFrom(r).Services.Prescriptions.List(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) getPrescription(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	p, err := principalFrom(r).Services.Prescriptions.Get(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type overrideRequest struct {
	OverrideReason string `json:"override_reason"`
}

func (a *API) repeatPrescription(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req overrideRequest
	if err := decodeOptional(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	p, err := principalFrom(r).Services.Prescriptions.Repeat(r.Context(), id, req.OverrideReason)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, issuedView(p))
}

func (a *API) cancelPrescription(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req reasonRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	p, err := principalFrom(r).Services.Prescriptions.Cancel(r.Context(), id, req.Reason)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ---------------------------------------------------------------------------
// Laboratory results
// ---------------------------------------------------------------------------

type labItemRequest struct {
	Analyte        string             `json:"analyte"`
	Value          string             `json:"value"`
	Unit           string             `json:"unit"`
	ReferenceRange string             `json:"reference_range"`
	AbnormalFlag   model.AbnormalFlag `json:"abnormal_flag"`
}

type labResultRequest struct {
	LabName             string           `json:"lab_name"`
	LabReference        string           `json:"lab_reference"`
	Panel               string           `json:"panel"`
	SpecimenCollectedAt *time.Time       `json:"specimen_collected_at"`
	ReportedAt          *time.Time       `json:"reported_at"`
	Status              model.LabStatus  `json:"status"`
	Comment             string           `json:"comment"`
	Items               []labItemRequest `json:"items"`
}

func (a *API) recordLabResult(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req labResultRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	in := service.LabResultInput{LabName: req.LabName, LabReference: req.LabReference, Panel: req.Panel,
		SpecimenCollectedAt: req.SpecimenCollectedAt, ReportedAt: req.ReportedAt, Status: req.Status, Comment: req.Comment}
	for _, it := range req.Items {
		in.Items = append(in.Items, service.LabItemInput{Analyte: it.Analyte, Value: it.Value, Unit: it.Unit,
			ReferenceRange: it.ReferenceRange, AbnormalFlag: it.AbnormalFlag})
	}
	l, err := principalFrom(r).Services.Labs.Record(r.Context(), id, in)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

func (a *API) listPatientLabResults(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := principalFrom(r).Services.Labs.ForPatient(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) labInbox(w http.ResponseWriter, r *http.Request) {
	abnormalOnly := r.URL.Query().Get("abnormal_only") == "true"
	items, err := principalFrom(r).Services.Labs.Inbox(r.Context(), abnormalOnly, queryInt(r, "limit", 50))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) getLabResult(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	l, err := principalFrom(r).Services.Labs.Get(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

type noteRequest struct {
	Note string `json:"note"`
}

func (a *API) acknowledgeLabResult(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req noteRequest
	if err := decodeOptional(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	l, err := principalFrom(r).Services.Labs.Acknowledge(r.Context(), id, req.Note)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// ---------------------------------------------------------------------------
// Referral letters and medical certificates
// ---------------------------------------------------------------------------

type referralRequest struct {
	ToName            string    `json:"to_name"`
	ToSpeciality      string    `json:"to_speciality"`
	ToPractice        string    `json:"to_practice"`
	Urgency           string    `json:"urgency"`
	Reason            string    `json:"reason"`
	ClinicalSummary   string    `json:"clinical_summary"`
	IncludeMedication bool      `json:"include_medication"`
	IncludeAllergies  bool      `json:"include_allergies"`
	EncounterID       *model.ID `json:"encounter_id"`
}

func (a *API) createReferral(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req referralRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := principalFrom(r).Services.Documents.CreateReferral(r.Context(), id, service.ReferralInput{
		ToName: req.ToName, ToSpeciality: req.ToSpeciality, ToPractice: req.ToPractice, Urgency: req.Urgency,
		Reason: req.Reason, ClinicalSummary: req.ClinicalSummary, IncludeMedication: req.IncludeMedication,
		IncludeAllergies: req.IncludeAllergies, EncounterID: req.EncounterID,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

type sickNoteRequest struct {
	Basis             model.CertificateBasis `json:"basis"`
	ExaminedOn        model.Date             `json:"examined_on"`
	ValidFrom         model.Date             `json:"valid_from"`
	ValidTo           model.Date             `json:"valid_to"`
	Fitness           string                 `json:"fitness"`
	CertificateFor    string                 `json:"certificate_for"`
	DiscloseDiagnosis bool                   `json:"disclose_diagnosis"`
	Diagnosis         string                 `json:"diagnosis"`
	BackdateReason    string                 `json:"backdate_reason"`
	EmployeeNumber    string                 `json:"employee_number"`
	Remarks           string                 `json:"remarks"`
	EncounterID       *model.ID              `json:"encounter_id"`
}

func (a *API) createSickNote(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req sickNoteRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := principalFrom(r).Services.Documents.CreateSickNote(r.Context(), id, service.SickNoteInput{
		Basis: req.Basis, ExaminedOn: req.ExaminedOn, ValidFrom: req.ValidFrom, ValidTo: req.ValidTo,
		Fitness: req.Fitness, CertificateFor: req.CertificateFor, DiscloseDiagnosis: req.DiscloseDiagnosis,
		Diagnosis: req.Diagnosis, BackdateReason: req.BackdateReason, EmployeeNumber: req.EmployeeNumber,
		Remarks: req.Remarks, EncounterID: req.EncounterID,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (a *API) listDocuments(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := principalFrom(r).Services.Documents.ForPatient(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) getDocument(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := principalFrom(r).Services.Documents.Get(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *API) issueDocument(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := principalFrom(r).Services.Documents.Issue(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *API) cancelDocument(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req reasonRequest
	if err := decodeOptional(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := principalFrom(r).Services.Documents.Cancel(r.Context(), id, req.Reason)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}
