package model

import "time"

// AuditOutcome records whether the audited action happened.
type AuditOutcome string

const (
	OutcomeSuccess AuditOutcome = "success"
	OutcomeFailure AuditOutcome = "failure"
	// OutcomeDenied means the caller was not allowed to do this. Recorded
	// separately from failure because a pattern of denials is what an
	// investigation of record snooping looks for.
	OutcomeDenied AuditOutcome = "denied"
)

// Audit actions. The names are stable identifiers: reports and alerts key on
// them, so rename one only with a migration of existing rows.
const (
	ActionLoginSucceeded        = "auth.login.succeeded"
	ActionLoginFailed           = "auth.login.failed"
	ActionLoginMFAChallenged    = "auth.login.mfa_challenged"
	ActionLogout                = "auth.logout"
	ActionAccountLocked         = "auth.account.locked"
	ActionPasswordChanged       = "auth.password.changed"
	ActionMFAEnrolmentStarted   = "auth.mfa.enrolment_started"
	ActionMFAEnabled            = "auth.mfa.enabled"
	ActionMFAVerified           = "auth.mfa.verified"
	ActionMFAFailed             = "auth.mfa.failed"
	ActionMFARecoveryCodeUsed   = "auth.mfa.recovery_code_used"
	ActionMFARecoveryCodesIssue = "auth.mfa.recovery_codes_issued"
	ActionMFAReset              = "auth.mfa.reset"

	ActionTenantCreated     = "tenant.created"
	ActionUserCreated       = "user.created"
	ActionUserStatusChanged = "user.status_changed"

	ActionPatientCreated      = "patient.created"
	ActionPatientUpdated      = "patient.updated"
	ActionPatientRead         = "patient.read"
	ActionPatientSearched     = "patient.searched"
	ActionPatientSummaryRead  = "patient.summary_read"
	ActionPatientAccessDenied = "patient.access_denied"
	ActionAllergyRecorded     = "patient.allergy_recorded"
	ActionAllergyUpdated      = "patient.allergy_updated"
	ActionConditionRecorded   = "patient.condition_recorded"
	ActionConditionUpdated    = "patient.condition_updated"
	ActionConsentRecorded     = "patient.consent_recorded"
	ActionConsentRead         = "patient.consent_read"
	ActionReviewIntervalSet   = "patient.review_interval_set"

	ActionAuditRead = "audit.read"
)

// AuditEntry is one row of the audit log (BR-8.1, BR-8.2).
type AuditEntry struct {
	TenantID    *ID
	ActorID     *ID
	ActorRole   string
	Action      string
	SubjectType string
	SubjectID   *ID
	Outcome     AuditOutcome
	Reason      string
	RequestID   string
	IP          string
	Metadata    map[string]any
	OccurredAt  time.Time
}

// AuditRecord is an audit row as read back for a practice administrator.
type AuditRecord struct {
	Seq         uint64         `json:"seq"`
	ActorID     *ID            `json:"actor_id,omitempty"`
	ActorRole   string         `json:"actor_role,omitempty"`
	Action      string         `json:"action"`
	SubjectType string         `json:"subject_type,omitempty"`
	SubjectID   *ID            `json:"subject_id,omitempty"`
	Outcome     AuditOutcome   `json:"outcome"`
	Reason      string         `json:"reason,omitempty"`
	RequestID   string         `json:"request_id,omitempty"`
	IP          string         `json:"ip,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	OccurredAt  time.Time      `json:"occurred_at"`
}
