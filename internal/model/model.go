// Package model holds the entities and the invariants that belong to them.
// It depends on nothing above it: no repositories, no services, no HTTP.
package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Roles and users
// ---------------------------------------------------------------------------

// Role is a user's single primary role (BR-1.4).
type Role string

const (
	RoleSuperAdmin  Role = "super_admin"
	RoleClinicAdmin Role = "clinic_admin"
	RoleDoctor      Role = "doctor"
	RoleNurse       Role = "nurse"
	RolePatient     Role = "patient"
	RoleCaregiver   Role = "caregiver"
)

func (r Role) Valid() bool {
	switch r {
	case RoleSuperAdmin, RoleClinicAdmin, RoleDoctor, RoleNurse, RolePatient, RoleCaregiver:
		return true
	}
	return false
}

// IsClinical reports whether the role treats patients.
func (r Role) IsClinical() bool { return r == RoleDoctor || r == RoleNurse }

// IsAdministrative reports whether the role manages a practice or the platform.
func (r Role) IsAdministrative() bool { return r == RoleClinicAdmin || r == RoleSuperAdmin }

// IsStaff reports whether the role works for the practice, as opposed to
// being a patient or acting for one.
func (r Role) IsStaff() bool { return r.IsClinical() || r == RoleClinicAdmin }

// RequiresProfessionalRegistration: BR-1.5 — clinical access is not granted
// until registration details (e.g. an HPCSA number) are captured.
func (r Role) RequiresProfessionalRegistration() bool { return r.IsClinical() }

// RequiresMFA: BR-1.3 — every clinical and administrative role.
func (r Role) RequiresMFA() bool { return r.IsClinical() || r.IsAdministrative() }

// UserStatus is the lifecycle of an account.
type UserStatus string

const (
	UserPending     UserStatus = "pending"
	UserActive      UserStatus = "active"
	UserSuspended   UserStatus = "suspended"
	UserDeactivated UserStatus = "deactivated"
)

func (s UserStatus) Valid() bool {
	switch s {
	case UserPending, UserActive, UserSuspended, UserDeactivated:
		return true
	}
	return false
}

// User is a login: staff member or patient. Secrets never serialise.
type User struct {
	ID                ID         `json:"id"`
	TenantID          ID         `json:"tenant_id"`
	Email             string     `json:"email"`
	FullName          string     `json:"full_name"`
	Mobile            string     `json:"mobile,omitempty"`
	PasswordHash      string     `json:"-"`
	Role              Role       `json:"role"`
	ProfessionalRegNo string     `json:"professional_reg_no,omitempty"`
	PatientID         *ID        `json:"patient_id,omitempty"`
	Status            UserStatus `json:"status"`
	MFASecret         []byte     `json:"-"`
	MFAEnabled        bool       `json:"mfa_enabled"`
	MFALastCounter    *uint64    `json:"-"`
	MFAEnrolledAt     *time.Time `json:"mfa_enrolled_at,omitempty"`
	FailedAttempts    int        `json:"-"`
	LockedUntil       *time.Time `json:"-"`
	LastLoginAt       *time.Time `json:"last_login_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// IsLocked reports whether BR-1.6 lockout is in force at now.
func (u *User) IsLocked(now time.Time) bool {
	return u.LockedUntil != nil && u.LockedUntil.After(now)
}

// CanAuthenticate reports whether the account may sign in at now.
func (u *User) CanAuthenticate(now time.Time) bool {
	return u.Status == UserActive && !u.IsLocked(now)
}

// ---------------------------------------------------------------------------
// Tenants
// ---------------------------------------------------------------------------

type TenantStatus string

const (
	TenantActive      TenantStatus = "active"
	TenantSuspended   TenantStatus = "suspended"
	TenantOffboarding TenantStatus = "offboarding"
)

// Tenant is one practice. The contact details appear on letters and
// certificates, so they belong to the practice rather than to settings.
type Tenant struct {
	ID             ID               `json:"id"`
	Slug           string           `json:"slug"`
	Name           string           `json:"name"`
	Status         TenantStatus     `json:"status"`
	Timezone       string           `json:"timezone"`
	PracticeNumber string           `json:"practice_number,omitempty"`
	Address        string           `json:"address,omitempty"`
	Phone          string           `json:"phone,omitempty"`
	Email          string           `json:"email,omitempty"`
	Settings       PracticeSettings `json:"settings"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

// PracticeSettings is per-practice configuration, kept in
// tenants.settings_json (BR-0.6).
type PracticeSettings struct {
	// WhatsAppEnabled lets the practice send reminders and recalls. It is off
	// until an administrator turns it on: messaging patients is a step a
	// practice takes deliberately, not a side effect of the platform being
	// configured.
	WhatsAppEnabled bool `json:"whatsapp_enabled"`
	// MessagingName is how the practice is named in messages; empty means the
	// practice name. A practice whose name says what it treats sets a
	// neutral one, because the message is read by whoever holds the phone.
	MessagingName string `json:"messaging_name,omitempty"`
	// FollowUpMinutes is how long after a dose's time a second reminder goes
	// out if the dose is still unanswered: nil means the platform default, 0
	// means never.
	FollowUpMinutes *int `json:"follow_up_minutes,omitempty"`
	// Demo marks a practice created by cmd/seed. Its patients' numbers are
	// made up, so it never sends anything, whatever else is configured.
	Demo bool `json:"demo,omitempty"`
}

// SenderName is the practice's name as it appears in messages.
func (s PracticeSettings) SenderName(practiceName string) string {
	if n := strings.TrimSpace(s.MessagingName); n != "" {
		return n
	}
	return practiceName
}

// ---------------------------------------------------------------------------
// Patients
// ---------------------------------------------------------------------------

type Sex string

const (
	SexFemale   Sex = "female"
	SexMale     Sex = "male"
	SexIntersex Sex = "intersex"
	SexUnknown  Sex = "unknown"
)

func (s Sex) Valid() bool {
	switch s {
	case SexFemale, SexMale, SexIntersex, SexUnknown:
		return true
	}
	return false
}

type IDType string

const (
	IDTypeSAID     IDType = "sa_id"
	IDTypePassport IDType = "passport"
	IDTypeOther    IDType = "other"
)

func (t IDType) Valid() bool {
	switch t {
	case IDTypeSAID, IDTypePassport, IDTypeOther:
		return true
	}
	return false
}

// Channel is how a patient prefers to be reached.
type Channel string

const (
	ChannelWhatsApp Channel = "whatsapp"
	ChannelSMS      Channel = "sms"
	ChannelPush     Channel = "push"
	ChannelEmail    Channel = "email"
	ChannelNone     Channel = "none"
)

func (c Channel) Valid() bool {
	switch c {
	case ChannelWhatsApp, ChannelSMS, ChannelPush, ChannelEmail, ChannelNone:
		return true
	}
	return false
}

// NeedsMobile reports whether reminders on this channel go to a phone number.
func (c Channel) NeedsMobile() bool { return c == ChannelWhatsApp || c == ChannelSMS }

type PatientStatus string

const (
	PatientActive   PatientStatus = "active"
	PatientInactive PatientStatus = "inactive"
	PatientDeceased PatientStatus = "deceased"
)

func (s PatientStatus) Valid() bool {
	switch s {
	case PatientActive, PatientInactive, PatientDeceased:
		return true
	}
	return false
}

type SmokingStatus string

const (
	SmokingNever   SmokingStatus = "never"
	SmokingFormer  SmokingStatus = "former"
	SmokingCurrent SmokingStatus = "current"
	SmokingUnknown SmokingStatus = "unknown"
)

func (s SmokingStatus) Valid() bool {
	switch s {
	case SmokingNever, SmokingFormer, SmokingCurrent, SmokingUnknown:
		return true
	}
	return false
}

type AlcoholUse string

const (
	AlcoholNone       AlcoholUse = "none"
	AlcoholOccasional AlcoholUse = "occasional"
	AlcoholRegular    AlcoholUse = "regular"
	AlcoholHeavy      AlcoholUse = "heavy"
	AlcoholUnknown    AlcoholUse = "unknown"
)

func (a AlcoholUse) Valid() bool {
	switch a {
	case AlcoholNone, AlcoholOccasional, AlcoholRegular, AlcoholHeavy, AlcoholUnknown:
		return true
	}
	return false
}

// Patient is the demographic and baseline record (BR-2.1 to BR-2.3).
type Patient struct {
	ID                   ID            `json:"id"`
	TenantID             ID            `json:"tenant_id"`
	FirstName            string        `json:"first_name"`
	LastName             string        `json:"last_name"`
	DateOfBirth          Date          `json:"date_of_birth"`
	Sex                  Sex           `json:"sex"`
	IDType               IDType        `json:"id_type,omitempty"`
	NationalID           string        `json:"national_id,omitempty"`
	Mobile               string        `json:"mobile,omitempty"`
	Email                string        `json:"email,omitempty"`
	Address              string        `json:"address,omitempty"`
	PreferredLanguage    string        `json:"preferred_language,omitempty"`
	PreferredChannel     Channel       `json:"preferred_channel"`
	WakingStart          *TimeOfDay    `json:"waking_start,omitempty"`
	WakingEnd            *TimeOfDay    `json:"waking_end,omitempty"`
	ReviewIntervalMonths *int          `json:"review_interval_months,omitempty"`
	MedicalAidName       string        `json:"medical_aid_name,omitempty"`
	MedicalAidNumber     string        `json:"medical_aid_number,omitempty"`
	NOKName              string        `json:"nok_name,omitempty"`
	NOKRelationship      string        `json:"nok_relationship,omitempty"`
	NOKPhone             string        `json:"nok_phone,omitempty"`
	SmokingStatus        SmokingStatus `json:"smoking_status"`
	AlcoholUse           AlcoholUse    `json:"alcohol_use"`
	PastSurgicalHistory  string        `json:"past_surgical_history,omitempty"`
	FamilyHistory        string        `json:"family_history,omitempty"`
	Status               PatientStatus `json:"status"`
	CreatedBy            *ID           `json:"created_by,omitempty"`
	CreatedAt            time.Time     `json:"created_at"`
	UpdatedAt            time.Time     `json:"updated_at"`
}

func (p *Patient) FullName() string {
	return strings.TrimSpace(p.FirstName + " " + p.LastName)
}

// AgeAt returns completed years at the given moment.
func (p *Patient) AgeAt(at time.Time) int {
	return p.DateOfBirth.AgeAt(at)
}

type AllergySeverity string

const (
	SeverityMild            AllergySeverity = "mild"
	SeverityModerate        AllergySeverity = "moderate"
	SeveritySevere          AllergySeverity = "severe"
	SeverityLifeThreatening AllergySeverity = "life_threatening"
	SeverityUnknown         AllergySeverity = "unknown"
)

func (s AllergySeverity) Valid() bool {
	switch s {
	case SeverityMild, SeverityModerate, SeveritySevere, SeverityLifeThreatening, SeverityUnknown:
		return true
	}
	return false
}

// ClinicalStatus is shared by allergies and conditions. "entered_in_error"
// exists so a wrong entry can be withdrawn without being deleted.
type ClinicalStatus string

const (
	ClinicalActive         ClinicalStatus = "active"
	ClinicalResolved       ClinicalStatus = "resolved"
	ClinicalEnteredInError ClinicalStatus = "entered_in_error"
)

func (s ClinicalStatus) Valid() bool {
	switch s {
	case ClinicalActive, ClinicalResolved, ClinicalEnteredInError:
		return true
	}
	return false
}

type Allergy struct {
	ID         ID              `json:"id"`
	PatientID  ID              `json:"patient_id"`
	Substance  string          `json:"substance"`
	Reaction   string          `json:"reaction,omitempty"`
	Severity   AllergySeverity `json:"severity"`
	Status     ClinicalStatus  `json:"status"`
	RecordedBy ID              `json:"recorded_by"`
	RecordedAt time.Time       `json:"recorded_at"`
}

// Condition is a diagnosis on the problem list. IsChronic is set by the
// clinician rather than derived from the ICD-10 code: deriving it would mean
// shipping an opinion about which codes count as chronic and silently
// changing every practice's numbers.
type Condition struct {
	ID           ID             `json:"id"`
	PatientID    ID             `json:"patient_id"`
	ICD10Code    string         `json:"icd10_code,omitempty"`
	Description  string         `json:"description"`
	IsChronic    bool           `json:"is_chronic"`
	Status       ClinicalStatus `json:"status"`
	OnsetDate    *Date          `json:"onset_date,omitempty"`
	ResolvedDate *Date          `json:"resolved_date,omitempty"`
	RecordedBy   ID             `json:"recorded_by"`
	RecordedAt   time.Time      `json:"recorded_at"`
}

// ---------------------------------------------------------------------------
// Consent (BR-2.8, BR-5.14, BR-8.7)
// ---------------------------------------------------------------------------

type ConsentPurpose string

const (
	ConsentDataProcessing   ConsentPurpose = "data_processing"
	ConsentAIAssistedReview ConsentPurpose = "ai_assisted_review"
	ConsentReminderComms    ConsentPurpose = "reminder_communications"
)

// ConsentPurposes lists every purpose, in display order.
var ConsentPurposes = []ConsentPurpose{ConsentDataProcessing, ConsentAIAssistedReview, ConsentReminderComms}

func (p ConsentPurpose) Valid() bool {
	for _, q := range ConsentPurposes {
		if p == q {
			return true
		}
	}
	return false
}

type ConsentMethod string

const (
	ConsentWritten    ConsentMethod = "written"
	ConsentVerbal     ConsentMethod = "verbal"
	ConsentElectronic ConsentMethod = "electronic"
)

func (m ConsentMethod) Valid() bool {
	return m == ConsentWritten || m == ConsentVerbal || m == ConsentElectronic
}

// Consent is one decision. Rows are never updated: withdrawal is a new row.
type Consent struct {
	ID        ID             `json:"id"`
	PatientID ID             `json:"patient_id"`
	Purpose   ConsentPurpose `json:"purpose"`
	Granted   bool           `json:"granted"`
	Method    ConsentMethod  `json:"method"`
	Note      string         `json:"note,omitempty"`
	// RecordedBy is the staff member who recorded the decision; nil when the
	// patient made it themselves, by replying STOP to a message.
	RecordedBy *ID       `json:"recorded_by,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// ConsentState is the latest decision per purpose.
type ConsentState map[ConsentPurpose]Consent

// Granted reports the current decision. A purpose with no record reads as
// not granted: consent is never inferred from silence.
func (s ConsentState) Granted(p ConsentPurpose) bool {
	c, ok := s[p]
	return ok && c.Granted
}

// ConsentStatus is the per-purpose view the API returns.
type ConsentStatus struct {
	Purpose    ConsentPurpose `json:"purpose"`
	Granted    bool           `json:"granted"`
	Recorded   bool           `json:"recorded"`
	Method     ConsentMethod  `json:"method,omitempty"`
	RecordedAt *time.Time     `json:"recorded_at,omitempty"`
	RecordedBy *ID            `json:"recorded_by,omitempty"`
}

// Statuses renders every purpose, including those never recorded.
func (s ConsentState) Statuses() []ConsentStatus {
	out := make([]ConsentStatus, 0, len(ConsentPurposes))
	for _, p := range ConsentPurposes {
		st := ConsentStatus{Purpose: p}
		if c, ok := s[p]; ok {
			at := c.RecordedAt
			st.Granted, st.Recorded, st.Method = c.Granted, true, c.Method
			st.RecordedAt, st.RecordedBy = &at, c.RecordedBy
		}
		out = append(out, st)
	}
	return out
}

// PatientSummary is the consolidated view (BR-2.9).
type PatientSummary struct {
	Patient          Patient         `json:"patient"`
	AgeYears         int             `json:"age_years"`
	Allergies        []Allergy       `json:"allergies"`
	Conditions       []Condition     `json:"conditions"`
	Consents         []ConsentStatus `json:"consents"`
	Medications      []Regimen       `json:"medications"`
	RecentEncounters []Encounter     `json:"recent_encounters"`
	// OpenDraftNotes counts consultation notes started but not signed.
	OpenDraftNotes int `json:"open_draft_notes"`
	// UnacknowledgedAbnormalResults counts abnormal lab results no clinician
	// has acknowledged yet.
	UnacknowledgedAbnormalResults int `json:"unacknowledged_abnormal_results"`
	// Adherence is each current medicine's record over 7, 30 and 90 days
	// (BR-4.6).
	Adherence []RegimenAdherence `json:"adherence"`
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// FieldError names one problem with one input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationErrors collects every problem with an input so the client can
// show them all at once rather than one per round trip.
type ValidationErrors []FieldError

func (v ValidationErrors) Error() string {
	parts := make([]string, len(v))
	for i, e := range v {
		parts[i] = e.Field + " " + e.Message
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

func (v *ValidationErrors) Add(field, format string, args ...any) {
	*v = append(*v, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

// Err returns nil when there were no problems, so callers can write
// `return v.Err()` without a typed-nil trap.
func (v ValidationErrors) Err() error {
	if len(v) == 0 {
		return nil
	}
	return v
}

// ErrNotTheAuthor is returned when someone other than a draft's author tries
// to edit or sign it.
var ErrNotTheAuthor = errors.New("only the author may do this")
