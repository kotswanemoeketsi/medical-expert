package model

import (
	"fmt"
	"time"
)

// EncounterType describes how the patient was seen.
type EncounterType string

const (
	EncounterConsultation EncounterType = "consultation"
	EncounterFollowUp     EncounterType = "follow_up"
	EncounterTelephonic   EncounterType = "telephonic"
	EncounterHomeVisit    EncounterType = "home_visit"
	EncounterProcedure    EncounterType = "procedure"
	EncounterOther        EncounterType = "other"
)

func (t EncounterType) Valid() bool {
	switch t {
	case EncounterConsultation, EncounterFollowUp, EncounterTelephonic, EncounterHomeVisit, EncounterProcedure, EncounterOther:
		return true
	}
	return false
}

// EncounterStatus moves one way: draft → signed.
type EncounterStatus string

const (
	EncounterDraft  EncounterStatus = "draft"
	EncounterSigned EncounterStatus = "signed"
)

// NoteField names one of the five body fields of a note — the only fields
// an amendment may change. Who saw the patient and when are not
// correctable after the fact: if those are wrong, the note is wrong and the
// answer is a new note.
type NoteField string

const (
	FieldPresentingComplaint NoteField = "presenting_complaint"
	FieldHistory             NoteField = "history"
	FieldExamination         NoteField = "examination"
	FieldAssessment          NoteField = "assessment"
	FieldPlan                NoteField = "plan"
)

// NoteFields lists the amendable fields in note order.
var NoteFields = []NoteField{FieldPresentingComplaint, FieldHistory, FieldExamination, FieldAssessment, FieldPlan}

func (f NoteField) Valid() bool {
	for _, x := range NoteFields {
		if f == x {
			return true
		}
	}
	return false
}

// Vitals are the observations recorded at a consultation. Every field is
// optional; the ranges catch typing slips (a systolic of 1200), not
// clinical abnormality — an abnormal value is recorded as given.
type Vitals struct {
	SystolicBP      *int     `json:"systolic_bp,omitempty"`
	DiastolicBP     *int     `json:"diastolic_bp,omitempty"`
	PulseRate       *int     `json:"pulse_rate,omitempty"`
	RespiratoryRate *int     `json:"respiratory_rate,omitempty"`
	SpO2            *int     `json:"spo2,omitempty"`
	TemperatureC    *float64 `json:"temperature_c,omitempty"`
	WeightKg        *float64 `json:"weight_kg,omitempty"`
	HeightCm        *float64 `json:"height_cm,omitempty"`
	GlucoseMmolL    *float64 `json:"glucose_mmol_l,omitempty"`
}

// IsEmpty reports whether nothing was recorded.
func (v *Vitals) IsEmpty() bool {
	return v == nil || (v.SystolicBP == nil && v.DiastolicBP == nil && v.PulseRate == nil && v.RespiratoryRate == nil &&
		v.SpO2 == nil && v.TemperatureC == nil && v.WeightKg == nil && v.HeightCm == nil && v.GlucoseMmolL == nil)
}

// Validate checks plausibility bounds.
func (v *Vitals) Validate() ValidationErrors {
	var errs ValidationErrors
	if v == nil {
		return nil
	}
	intRange := func(name string, p *int, lo, hi int, unit string) {
		if p != nil && (*p < lo || *p > hi) {
			errs.Add("vitals."+name, "must be between %d and %d %s", lo, hi, unit)
		}
	}
	floatRange := func(name string, p *float64, lo, hi float64, unit string) {
		if p != nil && (*p < lo || *p > hi) {
			errs.Add("vitals."+name, "must be between %g and %g %s", lo, hi, unit)
		}
	}
	intRange("systolic_bp", v.SystolicBP, 40, 300, "mmHg")
	intRange("diastolic_bp", v.DiastolicBP, 20, 200, "mmHg")
	intRange("pulse_rate", v.PulseRate, 20, 250, "beats per minute")
	intRange("respiratory_rate", v.RespiratoryRate, 4, 80, "breaths per minute")
	intRange("spo2", v.SpO2, 50, 100, "%")
	floatRange("temperature_c", v.TemperatureC, 30, 45, "°C")
	floatRange("weight_kg", v.WeightKg, 0.3, 400, "kg")
	floatRange("height_cm", v.HeightCm, 20, 250, "cm")
	floatRange("glucose_mmol_l", v.GlucoseMmolL, 0.5, 60, "mmol/L")
	if v.SystolicBP != nil && v.DiastolicBP != nil && *v.DiastolicBP >= *v.SystolicBP {
		errs.Add("vitals.diastolic_bp", "must be lower than the systolic pressure")
	}
	if (v.SystolicBP == nil) != (v.DiastolicBP == nil) {
		errs.Add("vitals.diastolic_bp", "a blood pressure needs both systolic and diastolic values")
	}
	return errs
}

// Encounter is a consultation note (BR-2.4).
type Encounter struct {
	ID                  ID              `json:"id"`
	PatientID           ID              `json:"patient_id"`
	ClinicianID         ID              `json:"clinician_id"`
	ClinicianName       string          `json:"clinician_name,omitempty"`
	Type                EncounterType   `json:"encounter_type"`
	EncounterAt         time.Time       `json:"encounter_at"`
	PresentingComplaint string          `json:"presenting_complaint"`
	History             string          `json:"history"`
	Examination         string          `json:"examination"`
	Assessment          string          `json:"assessment"`
	Plan                string          `json:"plan"`
	Vitals              *Vitals         `json:"vitals,omitempty"`
	Status              EncounterStatus `json:"status"`
	SignedAt            *time.Time      `json:"signed_at,omitempty"`
	SignedBy            *ID             `json:"signed_by,omitempty"`
	AIAssisted          bool            `json:"ai_assisted"`
	AmendedAt           *time.Time      `json:"amended_at,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// Field returns the text of one body field.
func (e *Encounter) Field(f NoteField) string {
	switch f {
	case FieldPresentingComplaint:
		return e.PresentingComplaint
	case FieldHistory:
		return e.History
	case FieldExamination:
		return e.Examination
	case FieldAssessment:
		return e.Assessment
	case FieldPlan:
		return e.Plan
	}
	panic(fmt.Sprintf("unknown note field %q", f))
}

// SetField replaces the text of one body field.
func (e *Encounter) SetField(f NoteField, v string) {
	switch f {
	case FieldPresentingComplaint:
		e.PresentingComplaint = v
	case FieldHistory:
		e.History = v
	case FieldExamination:
		e.Examination = v
	case FieldAssessment:
		e.Assessment = v
	case FieldPlan:
		e.Plan = v
	default:
		panic(fmt.Sprintf("unknown note field %q", f))
	}
}

// Amendment is one change to a signed note (BR-2.7).
type Amendment struct {
	ID            ID        `json:"id"`
	EncounterID   ID        `json:"encounter_id"`
	Field         NoteField `json:"field"`
	PreviousValue string    `json:"previous_value"`
	NewValue      string    `json:"new_value"`
	Reason        string    `json:"reason"`
	AmendedBy     ID        `json:"amended_by"`
	AmendedByName string    `json:"amended_by_name,omitempty"`
	AmendedAt     time.Time `json:"amended_at"`
}

// DraftContent is what an AI draft may contain: the five note fields and a
// list of things for the clinician to check. There is nowhere to put a
// diagnosis or a recommendation.
type DraftContent struct {
	PresentingComplaint string   `json:"presenting_complaint"`
	History             string   `json:"history"`
	Examination         string   `json:"examination"`
	Assessment          string   `json:"assessment"`
	Plan                string   `json:"plan"`
	Flags               []string `json:"flags"`
}

type NoteDraftStatus string

const (
	DraftGenerated NoteDraftStatus = "generated"
	DraftFailed    NoteDraftStatus = "failed"
)

// NoteDraft is a stored AI draft, kept beside the note and never inside it.
type NoteDraft struct {
	ID           ID              `json:"id"`
	EncounterID  ID              `json:"encounter_id"`
	Dictation    string          `json:"dictation"`
	Draft        *DraftContent   `json:"draft,omitempty"`
	Model        string          `json:"model"`
	Status       NoteDraftStatus `json:"status"`
	Error        string          `json:"error,omitempty"`
	InputTokens  int             `json:"input_tokens,omitempty"`
	OutputTokens int             `json:"output_tokens,omitempty"`
	CreatedBy    ID              `json:"created_by"`
	CreatedAt    time.Time       `json:"created_at"`
	FlaggedAt    *time.Time      `json:"flagged_at,omitempty"`
	FlaggedBy    *ID             `json:"flagged_by,omitempty"`
	FlagReason   string          `json:"flag_reason,omitempty"`
}
