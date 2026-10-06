package model

import (
	"fmt"
	"strings"
	"time"
)

// Route is how a medicine enters the body.
type Route string

var routes = []Route{"oral", "sublingual", "topical", "inhaled", "subcutaneous", "intramuscular", "intravenous",
	"rectal", "vaginal", "ophthalmic", "otic", "nasal", "transdermal", "other"}

func (r Route) Valid() bool {
	for _, x := range routes {
		if r == x {
			return true
		}
	}
	return false
}

// Medication is an entry in the shared catalogue (BR-3.1, BR-0.10).
type Medication struct {
	ID          ID     `json:"id"`
	GenericName string `json:"generic_name"`
	BrandName   string `json:"brand_name,omitempty"`
	Strength    string `json:"strength"`
	DosageForm  string `json:"dosage_form"`
	Route       Route  `json:"route"`
	Schedule    string `json:"schedule,omitempty"`
	NAPPICode   string `json:"nappi_code,omitempty"`
	PackSize    string `json:"pack_size,omitempty"`
	IsActive    bool   `json:"is_active"`
}

// DisplayName is how the medicine appears on a script or letter:
// "Metformin 500 mg tablet", or "Glucophage (metformin) 500 mg tablet".
func (m *Medication) DisplayName() string {
	name := m.GenericName
	if m.BrandName != "" {
		name = m.BrandName + " (" + strings.ToLower(m.GenericName) + ")"
	}
	return strings.TrimSpace(name + " " + m.Strength + " " + m.DosageForm)
}

// Frequency is how often a dose is taken.
type Frequency string

const (
	FreqOnceDaily       Frequency = "once_daily"
	FreqTwiceDaily      Frequency = "twice_daily"
	FreqThreeTimesDaily Frequency = "three_times_daily"
	FreqFourTimesDaily  Frequency = "four_times_daily"
	FreqEveryOtherDay   Frequency = "every_other_day"
	FreqWeekly          Frequency = "weekly"
	FreqAsNeeded        Frequency = "as_needed"
)

var frequencyLabels = map[Frequency]string{
	FreqOnceDaily: "once daily", FreqTwiceDaily: "twice daily", FreqThreeTimesDaily: "three times daily",
	FreqFourTimesDaily: "four times daily", FreqEveryOtherDay: "every other day", FreqWeekly: "once a week",
	FreqAsNeeded: "when needed",
}

func (f Frequency) Valid() bool { _, ok := frequencyLabels[f]; return ok }

// Label is the frequency in words, as printed on a script.
func (f Frequency) Label() string {
	if l, ok := frequencyLabels[f]; ok {
		return l
	}
	return string(f)
}

// DosesPerDay is the scheduled rate: 0 for as-needed medicine, which has no
// schedule to adhere to.
func (f Frequency) DosesPerDay() float64 {
	switch f {
	case FreqOnceDaily:
		return 1
	case FreqTwiceDaily:
		return 2
	case FreqThreeTimesDaily:
		return 3
	case FreqFourTimesDaily:
		return 4
	case FreqEveryOtherDay:
		return 0.5
	case FreqWeekly:
		return 1.0 / 7
	}
	return 0
}

// RegimenStatus is where a regimen is in its life.
type RegimenStatus string

const (
	RegimenActive    RegimenStatus = "active"
	RegimenPaused    RegimenStatus = "paused"
	RegimenStopped   RegimenStatus = "stopped"
	RegimenCompleted RegimenStatus = "completed"
)

func (s RegimenStatus) Valid() bool {
	switch s {
	case RegimenActive, RegimenPaused, RegimenStopped, RegimenCompleted:
		return true
	}
	return false
}

// CanBecome reports whether a change of status is allowed. Stopped and
// completed are final: restarting a medicine is a new regimen with its own
// start date, not an edit of an old one.
func (s RegimenStatus) CanBecome(next RegimenStatus) bool {
	switch s {
	case RegimenActive:
		return next == RegimenPaused || next == RegimenStopped || next == RegimenCompleted
	case RegimenPaused:
		return next == RegimenActive || next == RegimenStopped || next == RegimenCompleted
	}
	return false
}

// Regimen is a medicine a patient has been told to take (BR-4.1).
type Regimen struct {
	ID              ID            `json:"id"`
	PatientID       ID            `json:"patient_id"`
	MedicationID    ID            `json:"medication_id"`
	MedicationName  string        `json:"medication_name"`
	Dose            string        `json:"dose"`
	Route           Route         `json:"route"`
	Frequency       Frequency     `json:"frequency"`
	Instructions    string        `json:"instructions,omitempty"`
	StartDate       Date          `json:"start_date"`
	EndDate         *Date         `json:"end_date,omitempty"`
	Status          RegimenStatus `json:"status"`
	StatusReason    string        `json:"status_reason,omitempty"`
	StatusChangedBy *ID           `json:"status_changed_by,omitempty"`
	StatusChangedAt *time.Time    `json:"status_changed_at,omitempty"`
	PrescribedBy    ID            `json:"prescribed_by"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// Describe is one line for a letter or a model's background:
// "Metformin 500 mg tablet: 1 tablet twice daily, oral (with meals)".
func (r *Regimen) Describe() string {
	s := fmt.Sprintf("%s: %s %s, %s", r.MedicationName, r.Dose, r.Frequency.Label(), r.Route)
	if r.Instructions != "" {
		s += " (" + r.Instructions + ")"
	}
	return s
}

// ActiveOn reports whether the regimen covers a date.
func (r *Regimen) ActiveOn(d Date) bool {
	if r.Status != RegimenActive || d.Before(r.StartDate) {
		return false
	}
	return r.EndDate == nil || !d.After(*r.EndDate)
}

// DoseStatus is the state of one scheduled dose.
type DoseStatus string

const (
	DosePending DoseStatus = "pending"
	DoseTaken   DoseStatus = "taken"
	DoseSkipped DoseStatus = "skipped"
	DoseMissed  DoseStatus = "missed"
)

// ResponseChannel is how a dose confirmation arrived.
type ResponseChannel string

const (
	ResponseWhatsApp ResponseChannel = "whatsapp"
	ResponseSMS      ResponseChannel = "sms"
	ResponsePush     ResponseChannel = "push"
	ResponsePortal   ResponseChannel = "portal"
	ResponseStaff    ResponseChannel = "staff"
)

// DoseSource says where a dose came from. Scheduled doses are generated from
// the regimen and kept in step with it; manual ones were added by hand and
// the scheduler never touches them.
type DoseSource string

const (
	DoseFromSchedule DoseSource = "schedule"
	DoseManual       DoseSource = "manual"
)

// DoseEvent is one scheduled dose and what happened to it.
type DoseEvent struct {
	ID              ID              `json:"id"`
	PatientID       ID              `json:"patient_id"`
	RegimenID       ID              `json:"regimen_id"`
	MedicationName  string          `json:"medication_name,omitempty"`
	ScheduledAt     time.Time       `json:"scheduled_at"`
	Source          DoseSource      `json:"source"`
	RemindedAt      *time.Time      `json:"reminded_at,omitempty"`
	Status          DoseStatus      `json:"status"`
	RespondedAt     *time.Time      `json:"responded_at,omitempty"`
	ResponseChannel ResponseChannel `json:"response_channel,omitempty"`
	RespondedBy     *ID             `json:"responded_by,omitempty"`
	SkipReason      string          `json:"skip_reason,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
}
