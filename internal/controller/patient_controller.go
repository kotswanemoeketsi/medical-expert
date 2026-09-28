package controller

import (
	"net/http"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/service"
)

type patientRequest struct {
	FirstName            string              `json:"first_name"`
	LastName             string              `json:"last_name"`
	DateOfBirth          model.Date          `json:"date_of_birth"`
	Sex                  model.Sex           `json:"sex"`
	IDType               model.IDType        `json:"id_type"`
	NationalID           string              `json:"national_id"`
	Mobile               string              `json:"mobile"`
	Email                string              `json:"email"`
	Address              string              `json:"address"`
	PreferredLanguage    string              `json:"preferred_language"`
	PreferredChannel     model.Channel       `json:"preferred_channel"`
	WakingStart          *model.TimeOfDay    `json:"waking_start"`
	WakingEnd            *model.TimeOfDay    `json:"waking_end"`
	ReviewIntervalMonths *int                `json:"review_interval_months"`
	MedicalAidName       string              `json:"medical_aid_name"`
	MedicalAidNumber     string              `json:"medical_aid_number"`
	NOKName              string              `json:"nok_name"`
	NOKRelationship      string              `json:"nok_relationship"`
	NOKPhone             string              `json:"nok_phone"`
	SmokingStatus        model.SmokingStatus `json:"smoking_status"`
	AlcoholUse           model.AlcoholUse    `json:"alcohol_use"`
	PastSurgicalHistory  string              `json:"past_surgical_history"`
	FamilyHistory        string              `json:"family_history"`
	Status               model.PatientStatus `json:"status"`
	// ConfirmNotDuplicate is sent after a person has looked at the possible
	// duplicates a first attempt returned and decided this is someone else.
	ConfirmNotDuplicate bool `json:"confirm_not_duplicate"`
}

func (req *patientRequest) input() service.PatientInput {
	return service.PatientInput{
		FirstName: req.FirstName, LastName: req.LastName, DateOfBirth: req.DateOfBirth, Sex: req.Sex,
		IDType: req.IDType, NationalID: req.NationalID, Mobile: req.Mobile, Email: req.Email,
		Address: req.Address, PreferredLanguage: req.PreferredLanguage, PreferredChannel: req.PreferredChannel,
		WakingStart: req.WakingStart, WakingEnd: req.WakingEnd, ReviewIntervalMonths: req.ReviewIntervalMonths,
		MedicalAidName: req.MedicalAidName, MedicalAidNumber: req.MedicalAidNumber, NOKName: req.NOKName,
		NOKRelationship: req.NOKRelationship, NOKPhone: req.NOKPhone, SmokingStatus: req.SmokingStatus,
		AlcoholUse: req.AlcoholUse, PastSurgicalHistory: req.PastSurgicalHistory,
		FamilyHistory: req.FamilyHistory, Status: req.Status,
	}
}

func (a *API) createPatient(w http.ResponseWriter, r *http.Request) {
	var req patientRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	p, err := principalFrom(r).Services.Patients.Create(r.Context(), req.input(), req.ConfirmNotDuplicate)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (a *API) updatePatient(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req patientRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	p, err := principalFrom(r).Services.Patients.Update(r.Context(), id, req.input())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) searchPatients(w http.ResponseWriter, r *http.Request) {
	limit, offset := queryInt(r, "limit", 25), queryInt(r, "offset", 0)
	items, err := principalFrom(r).Services.Patients.Search(r.Context(), r.URL.Query().Get("q"), limit, offset)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset})
}

func (a *API) getPatient(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	p, err := principalFrom(r).Services.Patients.Get(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) patientSummary(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	s, err := principalFrom(r).Services.Patients.Summary(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

type allergyRequest struct {
	Substance string                `json:"substance"`
	Reaction  string                `json:"reaction"`
	Severity  model.AllergySeverity `json:"severity"`
}

func (a *API) addAllergy(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req allergyRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	al, err := principalFrom(r).Services.Patients.AddAllergy(r.Context(), id, service.AllergyInput{
		Substance: req.Substance, Reaction: req.Reaction, Severity: req.Severity,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, al)
}

type clinicalStatusRequest struct {
	Status model.ClinicalStatus `json:"status"`
}

func (a *API) setAllergyStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	allergyID, err := pathID(r, "allergyId")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req clinicalStatusRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := principalFrom(r).Services.Patients.SetAllergyStatus(r.Context(), id, allergyID, req.Status); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type conditionRequest struct {
	ICD10Code   string      `json:"icd10_code"`
	Description string      `json:"description"`
	IsChronic   bool        `json:"is_chronic"`
	OnsetDate   *model.Date `json:"onset_date"`
}

func (a *API) addCondition(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req conditionRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	c, err := principalFrom(r).Services.Patients.AddCondition(r.Context(), id, service.ConditionInput{
		ICD10Code: req.ICD10Code, Description: req.Description, IsChronic: req.IsChronic, OnsetDate: req.OnsetDate,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

type conditionUpdateRequest struct {
	Status       model.ClinicalStatus `json:"status"`
	IsChronic    *bool                `json:"is_chronic"`
	ResolvedDate *model.Date          `json:"resolved_date"`
}

func (a *API) updateCondition(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	conditionID, err := pathID(r, "conditionId")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req conditionUpdateRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if req.IsChronic == nil {
		// Required rather than defaulted: silently clearing the chronic flag
		// would drop the patient off every chronic worklist.
		a.fail(w, r, &service.Error{Kind: service.KindInvalid, Code: "validation_failed", Message: "Some fields need attention.",
			Fields: []model.FieldError{{Field: "is_chronic", Message: "is required"}}})
		return
	}
	err = principalFrom(r).Services.Patients.UpdateCondition(r.Context(), id, conditionID, service.ConditionUpdate{
		Status: req.Status, IsChronic: *req.IsChronic, ResolvedDate: req.ResolvedDate,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type consentRequest struct {
	Purpose model.ConsentPurpose `json:"purpose"`
	Granted *bool                `json:"granted"`
	Method  model.ConsentMethod  `json:"method"`
	Note    string               `json:"note"`
}

func (a *API) recordConsent(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req consentRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	c, err := principalFrom(r).Services.Patients.RecordConsent(r.Context(), id, service.ConsentInput{
		Purpose: req.Purpose, Granted: req.Granted, Method: req.Method, Note: req.Note,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (a *API) listConsents(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	view, err := principalFrom(r).Services.Patients.Consents(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"current": view.Current, "history": view.History})
}

type reviewIntervalRequest struct {
	Months *int `json:"review_interval_months"`
}

func (a *API) setReviewInterval(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req reviewIntervalRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := principalFrom(r).Services.Patients.SetReviewInterval(r.Context(), id, req.Months); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
