package model

import (
	"encoding/json"
	"time"
)

// ---------------------------------------------------------------------------
// Prescriptions
// ---------------------------------------------------------------------------

type PrescriptionStatus string

const (
	PrescriptionActive    PrescriptionStatus = "active"
	PrescriptionCancelled PrescriptionStatus = "cancelled"
	PrescriptionExpired   PrescriptionStatus = "expired"
)

// Prescription is an authorisation, or a repeat issued against one.
type Prescription struct {
	ID                ID                 `json:"id"`
	PatientID         ID                 `json:"patient_id"`
	PrescriberID      ID                 `json:"prescriber_id"`
	PrescriberName    string             `json:"prescriber_name,omitempty"`
	RepeatOfID        *ID                `json:"repeat_of_id,omitempty"`
	IssuedAt          time.Time          `json:"issued_at"`
	ValidUntil        Date               `json:"valid_until"`
	RepeatsAuthorised int                `json:"repeats_authorised"`
	RepeatsUsed       int                `json:"repeats_used"`
	Status            PrescriptionStatus `json:"status"`
	OverrideReason    string             `json:"override_reason,omitempty"`
	Notes             string             `json:"notes,omitempty"`
	CancelReason      string             `json:"cancel_reason,omitempty"`
	CancelledBy       *ID                `json:"cancelled_by,omitempty"`
	CancelledAt       *time.Time         `json:"cancelled_at,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	Items             []PrescriptionItem `json:"items"`
}

// RepeatsRemaining is what is left on an original authorisation.
func (p *Prescription) RepeatsRemaining() int {
	if p.RepeatOfID != nil {
		return 0
	}
	if n := p.RepeatsAuthorised - p.RepeatsUsed; n > 0 {
		return n
	}
	return 0
}

// PrescriptionItem is one line on a script, copied at issue.
type PrescriptionItem struct {
	ID             ID     `json:"id"`
	LineNo         int    `json:"line_no"`
	RegimenID      *ID    `json:"regimen_id,omitempty"`
	MedicationID   *ID    `json:"medication_id,omitempty"`
	MedicationName string `json:"medication_name"`
	Dose           string `json:"dose"`
	Route          string `json:"route"`
	Frequency      string `json:"frequency"`
	Quantity       string `json:"quantity"`
	Instructions   string `json:"instructions,omitempty"`
}

// ---------------------------------------------------------------------------
// Laboratory results
// ---------------------------------------------------------------------------

type LabStatus string

const (
	LabPreliminary LabStatus = "preliminary"
	LabFinal       LabStatus = "final"
	LabCorrected   LabStatus = "corrected"
)

func (s LabStatus) Valid() bool { return s == LabPreliminary || s == LabFinal || s == LabCorrected }

// AbnormalFlag is the flag the laboratory printed against a value.
type AbnormalFlag string

const (
	FlagNone         AbnormalFlag = "none"
	FlagLow          AbnormalFlag = "low"
	FlagHigh         AbnormalFlag = "high"
	FlagAbnormal     AbnormalFlag = "abnormal"
	FlagCriticalLow  AbnormalFlag = "critical_low"
	FlagCriticalHigh AbnormalFlag = "critical_high"
	FlagCritical     AbnormalFlag = "critical"
)

func (f AbnormalFlag) Valid() bool {
	switch f {
	case FlagNone, FlagLow, FlagHigh, FlagAbnormal, FlagCriticalLow, FlagCriticalHigh, FlagCritical:
		return true
	}
	return false
}

func (f AbnormalFlag) IsAbnormal() bool { return f != FlagNone && f != "" }

func (f AbnormalFlag) IsCritical() bool {
	return f == FlagCriticalLow || f == FlagCriticalHigh || f == FlagCritical
}

// LabResult is one laboratory report.
type LabResult struct {
	ID                  ID              `json:"id"`
	PatientID           ID              `json:"patient_id"`
	PatientName         string          `json:"patient_name,omitempty"`
	LabName             string          `json:"lab_name"`
	LabReference        string          `json:"lab_reference,omitempty"`
	Panel               string          `json:"panel"`
	SpecimenCollectedAt *time.Time      `json:"specimen_collected_at,omitempty"`
	ReportedAt          time.Time       `json:"reported_at"`
	Status              LabStatus       `json:"status"`
	HasAbnormal         bool            `json:"has_abnormal"`
	HasCritical         bool            `json:"has_critical"`
	Comment             string          `json:"comment,omitempty"`
	RecordedBy          ID              `json:"recorded_by"`
	RecordedAt          time.Time       `json:"recorded_at"`
	AcknowledgedAt      *time.Time      `json:"acknowledged_at,omitempty"`
	AcknowledgedBy      *ID             `json:"acknowledged_by,omitempty"`
	AcknowledgementNote string          `json:"acknowledgement_note,omitempty"`
	Items               []LabResultItem `json:"items"`
}

// LabResultItem is one analyte, as reported.
type LabResultItem struct {
	ID             ID           `json:"id"`
	LineNo         int          `json:"line_no"`
	Analyte        string       `json:"analyte"`
	Value          string       `json:"value"`
	Unit           string       `json:"unit,omitempty"`
	ReferenceRange string       `json:"reference_range,omitempty"`
	AbnormalFlag   AbnormalFlag `json:"abnormal_flag"`
}

// ---------------------------------------------------------------------------
// Clinical documents
// ---------------------------------------------------------------------------

type DocumentType string

const (
	DocReferral DocumentType = "referral"
	DocSickNote DocumentType = "sick_note"
)

// NumberPrefix is the prefix of a document number: REF-2026-0001.
func (t DocumentType) NumberPrefix() string {
	if t == DocSickNote {
		return "MC"
	}
	return "REF"
}

type DocumentStatus string

const (
	DocumentDraft     DocumentStatus = "draft"
	DocumentIssued    DocumentStatus = "issued"
	DocumentCancelled DocumentStatus = "cancelled"
)

// CertificateBasis is what a medical certificate rests on. HPCSA guidance
// requires the practitioner to say which, and the wording differs.
type CertificateBasis string

const (
	BasisExamination   CertificateBasis = "examination"
	BasisPatientReport CertificateBasis = "patient_report"
)

func (b CertificateBasis) Valid() bool { return b == BasisExamination || b == BasisPatientReport }

// ClinicalDocument is a referral letter or medical certificate.
type ClinicalDocument struct {
	ID                 ID               `json:"id"`
	PatientID          ID               `json:"patient_id"`
	Type               DocumentType     `json:"doc_type"`
	Status             DocumentStatus   `json:"status"`
	Number             string           `json:"document_number,omitempty"`
	AuthorID           ID               `json:"author_id"`
	EncounterID        *ID              `json:"encounter_id,omitempty"`
	Body               json.RawMessage  `json:"body"`
	RenderedText       string           `json:"rendered_text,omitempty"`
	Basis              CertificateBasis `json:"basis,omitempty"`
	ExaminedOn         *Date            `json:"examined_on,omitempty"`
	ValidFrom          *Date            `json:"valid_from,omitempty"`
	ValidTo            *Date            `json:"valid_to,omitempty"`
	DisclosesDiagnosis bool             `json:"discloses_diagnosis"`
	BackdateReason     string           `json:"backdate_reason,omitempty"`
	IssuedAt           *time.Time       `json:"issued_at,omitempty"`
	IssuedBy           *ID              `json:"issued_by,omitempty"`
	CancelledAt        *time.Time       `json:"cancelled_at,omitempty"`
	CancelledBy        *ID              `json:"cancelled_by,omitempty"`
	CancelReason       string           `json:"cancel_reason,omitempty"`
	CreatedAt          time.Time        `json:"created_at"`
	UpdatedAt          time.Time        `json:"updated_at"`
}

// ReferralBody is the content of a referral letter.
type ReferralBody struct {
	ToName            string `json:"to_name"`
	ToSpeciality      string `json:"to_speciality,omitempty"`
	ToPractice        string `json:"to_practice,omitempty"`
	Urgency           string `json:"urgency"` // routine | urgent
	Reason            string `json:"reason"`
	ClinicalSummary   string `json:"clinical_summary,omitempty"`
	IncludeMedication bool   `json:"include_medication"`
	IncludeAllergies  bool   `json:"include_allergies"`
}

// SickNoteBody is the content of a medical certificate.
type SickNoteBody struct {
	Basis             CertificateBasis `json:"basis"`
	ExaminedOn        Date             `json:"examined_on"`
	ValidFrom         Date             `json:"valid_from"`
	ValidTo           Date             `json:"valid_to"`
	Fitness           string           `json:"fitness"`         // unfit | light_duties
	CertificateFor    string           `json:"certificate_for"` // work | school | other
	DiscloseDiagnosis bool             `json:"disclose_diagnosis"`
	Diagnosis         string           `json:"diagnosis,omitempty"`
	BackdateReason    string           `json:"backdate_reason,omitempty"`
	EmployeeNumber    string           `json:"employee_number,omitempty"`
	Remarks           string           `json:"remarks,omitempty"`
}
