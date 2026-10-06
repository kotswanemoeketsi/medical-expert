package service

import (
	"context"
	"fmt"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/phone"
	"github.com/yourorg/medical-expert-system/internal/repository"
)

func (b *base) reporting() *repository.ReportingRepository {
	return repository.NewReportingRepository(b.scope)
}

// ---------------------------------------------------------------------------
// Dashboard (BR-9.1)
// ---------------------------------------------------------------------------

// DashboardService shows practice figures. Counts only, no patient
// identifiers: it is the kind of screen left open at reception.
type DashboardService struct{ *base }

type DashboardPatients struct {
	Active       int `json:"active"`
	Chronic      int `json:"chronic"`
	SeenToday    int `json:"seen_today"`
	SeenInWindow int `json:"seen_in_window"`
}

type DashboardConsultations struct {
	SignedToday    int `json:"signed_today"`
	SignedInWindow int `json:"signed_in_window"`
	OpenDrafts     int `json:"open_drafts"`
}

type DashboardAdherence struct {
	model.DoseCounts
	Due             int                   `json:"due"`
	PendingExcluded int                   `json:"pending_excluded"`
	ResponseRate    *float64              `json:"response_rate"`
	AdherenceRate   *float64              `json:"adherence_rate"`
	ResponseBuckets model.ResponseBuckets `json:"patients_by_response"`
}

type DashboardResults struct {
	Unacknowledged int `json:"unacknowledged"`
	Abnormal       int `json:"abnormal"`
	Critical       int `json:"critical"`
}

type DashboardPrescriptions struct {
	ExpiringSoon     int `json:"expiring_within_14_days"`
	RepeatsExhausted int `json:"repeats_exhausted"`
}

// Dashboard is every figure, with notes on what each one measures.
type Dashboard struct {
	GeneratedAt       time.Time                   `json:"generated_at"`
	WindowDays        int                         `json:"window_days"`
	Patients          DashboardPatients           `json:"patients"`
	ChronicConditions []repository.ConditionCount `json:"chronic_conditions"`
	Consultations     DashboardConsultations      `json:"consultations"`
	Adherence         DashboardAdherence          `json:"adherence"`
	Results           DashboardResults            `json:"results"`
	Prescriptions     DashboardPrescriptions      `json:"prescriptions"`
	Messages          HealthCount                 `json:"messages_7_days"`
	Notes             []string                    `json:"notes"`
}

// Get returns the dashboard over the last `days` days (7 to 365, default 30).
func (s *DashboardService) Get(ctx context.Context, days int) (*Dashboard, error) {
	if err := s.requireStaff(); err != nil {
		return nil, err
	}
	if days == 0 {
		days = 30
	}
	if days < 7 || days > 365 {
		return nil, invalidField("days", "must be between 7 and 365")
	}
	t, err := repository.NewPracticeRepository(s.scope).Get(ctx)
	if err != nil {
		return nil, err
	}
	loc := practiceLocation(t.Timezone)
	now := s.now()
	todayStart := startOfDayIn(now, loc)
	today := model.DateOf(now.In(loc))
	c, err := s.reporting().Dashboard(ctx, now.AddDate(0, 0, -days), todayStart, now, today,
		model.DateOf(todayStart.AddDate(0, 0, 14)))
	if err != nil {
		return nil, err
	}
	health, err := s.notifications().Health(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		return nil, err
	}
	d := &Dashboard{
		GeneratedAt: now, WindowDays: days,
		Patients:          DashboardPatients{Active: c.ActivePatients, Chronic: c.ChronicPatients, SeenToday: c.SeenToday, SeenInWindow: c.SeenInWindow},
		ChronicConditions: c.ByCondition,
		Consultations:     DashboardConsultations{SignedToday: c.SignedToday, SignedInWindow: c.SignedInWindow, OpenDrafts: c.OpenDrafts},
		Adherence: DashboardAdherence{DoseCounts: c.Doses, Due: c.Doses.Due(), PendingExcluded: c.PendingDoses,
			ResponseRate: c.Doses.ResponseRate(), AdherenceRate: c.Doses.AdherenceRate(), ResponseBuckets: c.Buckets},
		Results:       DashboardResults{Unacknowledged: c.UnackResults, Abnormal: c.UnackAbnormal, Critical: c.UnackCrit},
		Prescriptions: DashboardPrescriptions{ExpiringSoon: c.ScriptsExpiring, RepeatsExhausted: c.ScriptsExhausted},
		Messages:      healthCount(health),
		Notes: []string{
			"Response rate: of the doses whose answer window has closed, the share the patient answered at all, taken or skipped.",
			"Adherence rate: of the same doses, the share the patient reported taking. Answers are self-reported, not observed.",
			fmt.Sprintf("Pending doses (not yet due, or due in the last %d hours and still answerable) are left out of both rates.", int(s.reg.cfg.DoseGrace.Hours())),
			"patients_by_response places each patient with closed doses in the window by the share they answered.",
			"Chronic counts each patient once. A patient with two chronic conditions appears in both condition rows, so the rows add up to more than the total.",
			"Doses are scheduled only for patients who can answer: those with their own login, or reachable on WhatsApp with their consent.",
			"Counts only: nothing here identifies a patient.",
		},
	}
	if err := s.record(ctx, model.AuditEntry{Action: model.ActionDashboardRead, Outcome: model.OutcomeSuccess,
		Metadata: map[string]any{"window_days": days}}); err != nil {
		return nil, err
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Recall lists
// ---------------------------------------------------------------------------

// RecallService builds chronic-care worklists. Unlike the dashboard these
// carry names and phone numbers, deliberately — the point is that somebody
// picks up the phone — so they are staff-only and every pull is audited as
// a read of patient data.
type RecallService struct{ *base }

// RecallReason names a worklist.
type RecallReason string

const (
	RecallReviewOverdue        RecallReason = "review_overdue"
	RecallScriptExpiring       RecallReason = "script_expiring"
	RecallRepeatsExhausted     RecallReason = "repeats_exhausted"
	RecallNotResponding        RecallReason = "not_responding"
	RecallUnacknowledgedResult RecallReason = "unacknowledged_result"
)

// RecallOptions tune a list; zero values take the defaults.
type RecallOptions struct {
	Days      int // script_expiring: look ahead (14); not_responding: look back (30)
	Threshold int // not_responding: response rate below this percentage (50)
	MinDoses  int // not_responding: closed doses needed to judge (4)
	Limit     int // at most 200 (100)
}

// RecallEntry is one patient to contact and why.
type RecallEntry struct {
	PatientID        model.ID      `json:"patient_id"`
	FirstName        string        `json:"first_name"`
	LastName         string        `json:"last_name"`
	Mobile           string        `json:"mobile,omitempty"`
	MobileDisplay    string        `json:"mobile_display,omitempty"`
	PreferredChannel model.Channel `json:"preferred_channel"`
	// CanMessage: on WhatsApp, with a number and consent to reminders. A
	// recall message can be sent if the practice's messaging is on.
	CanMessage   bool       `json:"can_message"`
	LastRecallAt *time.Time `json:"last_recall_at,omitempty"`

	LastReview           *time.Time  `json:"last_review,omitempty"`
	ReviewIntervalMonths int         `json:"review_interval_months,omitempty"`
	ReviewDueSince       *model.Date `json:"review_due_since,omitempty"`
	PrescriptionID       *model.ID   `json:"prescription_id,omitempty"`
	ValidUntil           *model.Date `json:"valid_until,omitempty"`
	RepeatsLeft          *int        `json:"repeats_left,omitempty"`
	DosesDue             int         `json:"doses_due,omitempty"`
	DosesAnswered        *int        `json:"doses_answered,omitempty"`
	ResponseRate         *float64    `json:"response_rate,omitempty"`
	LabResultID          *model.ID   `json:"lab_result_id,omitempty"`
	Panel                string      `json:"panel,omitempty"`
	ResultRecordedAt     *time.Time  `json:"result_recorded_at,omitempty"`
	Critical             bool        `json:"critical,omitempty"`
}

// RecallList is one worklist and the criteria that built it.
type RecallList struct {
	Reason          RecallReason   `json:"reason"`
	Criteria        map[string]any `json:"criteria"`
	MessagingActive bool           `json:"messaging_active"`
	Count           int            `json:"count"`
	Items           []RecallEntry  `json:"items"`
}

// List builds a worklist. Unacknowledged results are for clinicians only:
// reception has no business with them, and a clinician must review a result
// before anyone phones about it.
func (s *RecallService) List(ctx context.Context, reason RecallReason, o RecallOptions) (*RecallList, error) {
	if err := s.requireStaff(); err != nil {
		return nil, err
	}
	if o.Limit <= 0 || o.Limit > 200 {
		o.Limit = 100
	}
	t, err := repository.NewPracticeRepository(s.scope).Get(ctx)
	if err != nil {
		return nil, err
	}
	loc := practiceLocation(t.Timezone)
	now := s.now()
	today := model.DateOf(now.In(loc))
	repo := s.reporting()
	list := &RecallList{Reason: reason, Criteria: map[string]any{}, MessagingActive: s.reg.messagingOn(t)}
	var rows []repository.RecallRow
	switch reason {
	case RecallReviewOverdue:
		list.Criteria["default_review_months"] = s.reg.cfg.DefaultReviewMonths
		rows, err = repo.ReviewOverdue(ctx, now, s.reg.cfg.DefaultReviewMonths, o.Limit)
	case RecallScriptExpiring:
		if o.Days == 0 {
			o.Days = 14
		}
		if o.Days < 1 || o.Days > 90 {
			return nil, invalidField("days", "must be between 1 and 90")
		}
		list.Criteria["within_days"] = o.Days
		rows, err = repo.ScriptExpiring(ctx, today, model.DateOf(now.In(loc).AddDate(0, 0, o.Days)), o.Limit)
	case RecallRepeatsExhausted:
		rows, err = repo.RepeatsExhausted(ctx, today, o.Limit)
	case RecallNotResponding:
		if o.Days == 0 {
			o.Days = 30
		}
		if o.Threshold == 0 {
			o.Threshold = 50
		}
		if o.MinDoses == 0 {
			o.MinDoses = 4
		}
		var v model.ValidationErrors
		if o.Days < 3 || o.Days > 365 {
			v.Add("days", "must be between 3 and 365")
		}
		if o.Threshold < 1 || o.Threshold > 100 {
			v.Add("threshold", "must be a percentage between 1 and 100")
		}
		if o.MinDoses < 1 || o.MinDoses > 1000 {
			v.Add("min_doses", "must be between 1 and 1000")
		}
		if len(v) > 0 {
			return nil, invalid(v)
		}
		list.Criteria["days"], list.Criteria["below_percent"], list.Criteria["min_doses"] = o.Days, o.Threshold, o.MinDoses
		rows, err = repo.NotResponding(ctx, now.AddDate(0, 0, -o.Days), now, o.MinDoses, o.Threshold, o.Limit)
	case RecallUnacknowledgedResult:
		if !s.actor.Role.IsClinical() {
			return nil, ErrForbidden
		}
		list.Criteria["unreviewed_for_hours"] = 24
		rows, err = repo.UnacknowledgedResult(ctx, now.Add(-24*time.Hour), o.Limit)
	default:
		return nil, invalidField("reason", "must be review_overdue, script_expiring, repeats_exhausted, not_responding or unacknowledged_result")
	}
	if err != nil {
		return nil, err
	}
	list.Items = make([]RecallEntry, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		list.Items = append(list.Items, recallEntry(r, reason, loc))
		ids = append(ids, r.PatientID.String())
	}
	list.Count = len(list.Items)
	if err := s.record(ctx, model.AuditEntry{Action: model.ActionRecallListRead, Outcome: model.OutcomeSuccess,
		Metadata: map[string]any{"reason": string(reason), "count": len(ids), "patient_ids": ids}}); err != nil {
		return nil, err
	}
	return list, nil
}

func recallEntry(r repository.RecallRow, reason RecallReason, loc *time.Location) RecallEntry {
	e := RecallEntry{
		PatientID: r.PatientID, FirstName: r.FirstName, LastName: r.LastName, Mobile: r.Mobile,
		PreferredChannel: r.Channel, LastRecallAt: r.LastRecallAt,
		CanMessage: r.Channel == model.ChannelWhatsApp && r.Mobile != "" && r.ReminderConsent,
	}
	if r.Mobile != "" {
		e.MobileDisplay = phone.Display(r.Mobile)
	}
	switch reason {
	case RecallReviewOverdue:
		e.LastReview, e.ReviewIntervalMonths = r.LastReview, r.ReviewMonths
		if r.LastReview != nil {
			due := model.DateOf(r.LastReview.In(loc).AddDate(0, r.ReviewMonths, 0))
			e.ReviewDueSince = &due
		}
	case RecallScriptExpiring, RecallRepeatsExhausted:
		e.PrescriptionID, e.ValidUntil, e.RepeatsLeft = r.PrescriptionID, r.ValidUntil, r.RepeatsLeft
	case RecallNotResponding:
		answered := r.Answered
		e.DosesDue, e.DosesAnswered = r.Due, &answered
		e.ResponseRate = model.DoseCounts{Taken: r.Answered, Missed: r.Due - r.Answered}.ResponseRate()
	case RecallUnacknowledgedResult:
		e.LabResultID, e.Panel, e.ResultRecordedAt, e.Critical = r.LabResultID, r.Panel, r.ResultRecorded, r.Critical
	}
	return e
}

// ---------------------------------------------------------------------------
// Maintenance
// ---------------------------------------------------------------------------

// MaintenanceService runs a practice's own maintenance on request.
type MaintenanceService struct{ *base }

// CloseOverdueDoses closes this practice's expired answer windows now,
// rather than at the next hourly run. Administrators only.
func (s *MaintenanceService) CloseOverdueDoses(ctx context.Context) (int64, error) {
	if err := s.requireAdmin(); err != nil {
		return 0, err
	}
	return s.closeOverdue(ctx)
}
