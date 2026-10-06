package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/notify"
	"github.com/yourorg/medical-expert-system/internal/phone"
	"github.com/yourorg/medical-expert-system/internal/repository"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// NotificationService sends WhatsApp reminders and recalls by hand, and
// shows what was sent to a patient and what came back (M7).
type NotificationService struct{ *base }

const (
	// reminderLateness: a reminder that cannot go out within this of the
	// dose's time is not sent at all. "Time for your 08:00 medicine" at
	// 11:30 only confuses.
	reminderLateness = time.Hour
	// sendLease is longer than a send can take, so a second sender waits.
	sendLease   = 2 * time.Minute
	maxAttempts = 3
	// recallInterval is the least time between recalls to one patient.
	recallInterval = 7 * 24 * time.Hour
	// replyLookback is how far back a typed reply looks for the message it
	// answers.
	replyLookback = 24 * time.Hour
)

func (b *base) notifications() *repository.NotificationRepository {
	return repository.NewNotificationRepository(b.scope)
}

// termCache holds the catalogue's medicine names for the clinical-detail
// check, refreshed every ten minutes.
type termCache struct {
	mu      sync.Mutex
	terms   []string
	fetched time.Time
}

func (r *Registry) medicationTerms(ctx context.Context) ([]string, error) {
	r.terms.mu.Lock()
	defer r.terms.mu.Unlock()
	if r.terms.terms != nil && r.now().Sub(r.terms.fetched) < 10*time.Minute {
		return r.terms.terms, nil
	}
	names, err := r.platform.MedicationNames(ctx)
	if err != nil {
		return nil, err
	}
	terms := notify.MedicineTerms(names)
	r.terms.terms, r.terms.fetched = terms, r.now()
	return terms, nil
}

// checkParams refuses message parameters that would say something about
// the patient's health to whoever holds the phone. Every template's first
// parameter is the patient's first name: it is checked for health words, but
// not against the catalogue's brand names, some of which are also people's
// names, and a patient's own name reveals nothing about their treatment.
func (b *base) checkParams(ctx context.Context, params []string) error {
	terms, err := b.reg.medicationTerms(ctx)
	if err != nil {
		return err
	}
	if len(params) == 0 {
		return nil
	}
	if err := notify.CheckNoClinicalDetail(params[:1], nil); err != nil {
		return err
	}
	err = notify.CheckNoClinicalDetail(params[1:], terms)
	var ce *notify.ClinicalDetailError
	if errors.As(err, &ce) {
		ce.Param++ // numbered from the full list
	}
	return err
}

func (b *base) reminderConsent(ctx context.Context, patientID model.ID) (bool, error) {
	state, err := b.patients().ConsentState(ctx, patientID)
	if err != nil {
		return false, err
	}
	return state.Granted(model.ConsentReminderComms), nil
}

// newNotification builds a queued message, leased to its creator, who sends
// it straight away.
func (b *base) newNotification(purpose model.NotificationPurpose, tpl notify.Template, patientID model.ID, to string,
	params []string, doseIDs []model.ID, createdBy *model.ID) (*model.Notification, error) {
	preview, err := tpl.Preview(params)
	if err != nil {
		return nil, err
	}
	now := b.now()
	lease := now.Add(sendLease)
	n := &model.Notification{
		ID: model.MustNewID(), PatientID: patientID, Channel: model.ChannelWhatsApp, Purpose: purpose,
		Destination: to, TemplateName: tpl.Name, TemplateLanguage: b.reg.sender.Language(), Params: params,
		Preview: preview, Status: model.NotificationQueued, Attempts: 1, NextAttemptAt: &lease,
		CreatedBy: createdBy, CreatedAt: now, UpdatedAt: now, DoseIDs: doseIDs,
	}
	if len(tpl.Buttons) > 0 {
		if n.ReplyToken, err = notify.NewReplyToken(); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// refuse turns a message that failed the clinical-detail check into a
// failed record, so the refusal is visible and the doses are not tried
// again every minute.
func refuse(n *model.Notification, why error, now time.Time) {
	n.Status, n.Attempts, n.NextAttemptAt = model.NotificationFailed, 0, nil
	n.ErrorCode, n.ErrorDetail, n.FailedAt = "clinical_detail", why.Error(), &now
}

func backoff(attempts int) time.Duration {
	if attempts <= 1 {
		return time.Minute
	}
	return 5 * time.Minute
}

// deliver makes one attempt at a leased message and records what happened.
// Errors reaching the provider are recorded on the message, not returned;
// an error is returned only when the record itself cannot be written.
func (b *base) deliver(ctx context.Context, n *model.Notification) error {
	repo := b.notifications()
	tpl, ok := notify.ByName(n.TemplateName)
	if !ok {
		return repo.MarkFailed(ctx, n.ID, "unknown_template", "No template named "+n.TemplateName, b.now())
	}
	msg := notify.Message{To: n.Destination, Template: tpl, Params: n.Params}
	if len(tpl.Buttons) > 0 {
		msg.ButtonPayloads = notify.DosePayloads(n.ReplyToken)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	providerID, err := b.reg.sender.Send(sendCtx, msg)
	cancel()
	now := b.now()
	meta := map[string]any{"notification_id": n.ID.String(), "purpose": string(n.Purpose), "attempt": n.Attempts}
	if err == nil {
		if err := repo.MarkSent(ctx, n.ID, providerID, now); err != nil {
			return fmt.Errorf("recording a sent message: %w", err)
		}
		b.recordWrite(ctx, model.AuditEntry{Action: model.ActionNotificationSent, Outcome: model.OutcomeSuccess,
			SubjectType: "patient", SubjectID: idp(n.PatientID), Metadata: meta})
		return nil
	}
	code, detail := "", err.Error()
	var se *notify.SendError
	if errors.As(err, &se) {
		code, detail = se.Code, se.Detail
	}
	if notify.IsPermanent(err) || n.Attempts >= maxAttempts {
		if code == "" {
			code = "send_failed"
		}
		if err := repo.MarkFailed(ctx, n.ID, code, detail, now); err != nil && !errors.Is(err, repository.ErrConflict) {
			return err
		}
		meta["error_code"] = code
		b.recordWrite(ctx, model.AuditEntry{Action: model.ActionNotificationFailed, Outcome: model.OutcomeFailure,
			SubjectType: "patient", SubjectID: idp(n.PatientID), Reason: detail, Metadata: meta})
		return nil
	}
	if err := repo.RetryLater(ctx, n.ID, now.Add(backoff(n.Attempts)), code, detail, now); err != nil &&
		!errors.Is(err, repository.ErrConflict) {
		return err
	}
	return nil
}

// doseGroup is the doses one patient has at one moment: one message covers
// them all, and one reply answers them all.
type doseGroup struct {
	patientID model.ID
	firstName string
	mobile    string
	at        time.Time
	doseIDs   []model.ID
}

func groupDoses(due []repository.DueDose) []doseGroup {
	var out []doseGroup
	for _, d := range due {
		if n := len(out); n > 0 && out[n-1].patientID == d.PatientID && out[n-1].at.Equal(d.ScheduledAt) {
			out[n-1].doseIDs = append(out[n-1].doseIDs, d.DoseID)
			continue
		}
		out = append(out, doseGroup{patientID: d.PatientID, firstName: d.FirstName, mobile: d.Mobile,
			at: d.ScheduledAt, doseIDs: []model.ID{d.DoseID}})
	}
	return out
}

// sendDoseMessage writes and sends one reminder or follow-up. It returns
// nil, nil when another dispatcher got to the doses first.
func (b *base) sendDoseMessage(ctx context.Context, purpose model.NotificationPurpose, g doseGroup, loc *time.Location,
	createdBy *model.ID, mode repository.ClaimMode) (*model.Notification, error) {
	tpl := notify.DoseReminder
	if purpose == model.PurposeDoseFollowUp {
		tpl = notify.DoseFollowUp
	}
	params := []string{g.firstName, g.at.In(loc).Format("15:04")}
	n, err := b.newNotification(purpose, tpl, g.patientID, g.mobile, params, g.doseIDs, createdBy)
	if err != nil {
		return nil, err
	}
	refused := b.checkParams(ctx, params)
	var ce *notify.ClinicalDetailError
	if refused != nil && !errors.As(refused, &ce) {
		return nil, refused
	}
	if refused != nil {
		refuse(n, refused, b.now())
	}
	if err := b.notifications().Create(ctx, n, mode); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, nil
		}
		return nil, err
	}
	meta := map[string]any{"notification_id": n.ID.String(), "purpose": string(purpose), "doses": len(g.doseIDs)}
	if refused != nil {
		b.recordWrite(ctx, model.AuditEntry{Action: model.ActionNotificationRefused, Outcome: model.OutcomeDenied,
			SubjectType: "patient", SubjectID: idp(g.patientID), Reason: refused.Error(), Metadata: meta})
		return n, clinicalDetail(refused)
	}
	b.recordWrite(ctx, model.AuditEntry{Action: model.ActionNotificationQueued, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(g.patientID), Metadata: meta})
	return n, b.deliver(ctx, n)
}

// DispatchResult counts one dispatcher pass.
type DispatchResult struct {
	Reminders, FollowUps, Retried, Expired, Refused int
	SkippedNoConsent, SkippedLimit                  int
}

func (d *DispatchResult) add(o DispatchResult) {
	d.Reminders += o.Reminders
	d.FollowUps += o.FollowUps
	d.Retried += o.Retried
	d.Expired += o.Expired
	d.Refused += o.Refused
	d.SkippedNoConsent += o.SkippedNoConsent
	d.SkippedLimit += o.SkippedLimit
}

func (d DispatchResult) String() string {
	return fmt.Sprintf("%d reminders, %d follow-ups, %d retried, %d expired, %d refused, %d without consent, %d over the limit",
		d.Reminders, d.FollowUps, d.Retried, d.Expired, d.Refused, d.SkippedNoConsent, d.SkippedLimit)
}

// DispatchReminders is the per-minute pass (BR-4.3, BR-4.5, BR-7.4): retry
// what failed for a passing reason, remind every dose that has come due,
// and follow up doses still unanswered.
func (r *Registry) DispatchReminders(ctx context.Context) (DispatchResult, error) {
	var total DispatchResult
	if r.sender == nil {
		return total, nil
	}
	err := r.forEachPractice(ctx, func(b *base, t *model.Tenant) error {
		if !r.messagingOn(t) {
			return nil
		}
		res, err := b.dispatch(ctx, t)
		total.add(res)
		return err
	})
	return total, err
}

// followUpAfter is the practice's follow-up delay, or the platform's.
func (r *Registry) followUpAfter(t *model.Tenant) time.Duration {
	if m := t.Settings.FollowUpMinutes; m != nil {
		return time.Duration(*m) * time.Minute
	}
	return r.cfg.FollowUpAfter
}

func (b *base) dispatch(ctx context.Context, t *model.Tenant) (DispatchResult, error) {
	var res DispatchResult
	repo := b.notifications()
	loc := practiceLocation(t.Timezone)
	now := b.now()

	expired, err := repo.ExpireStale(ctx, now.Add(-reminderLateness), now)
	if err != nil {
		return res, err
	}
	res.Expired = int(expired)

	retries, err := repo.DueForRetry(ctx, now, 100)
	if err != nil {
		return res, err
	}
	for i := range retries {
		n := &retries[i]
		if err := repo.Lease(ctx, n.ID, now, now.Add(sendLease)); err != nil {
			if errors.Is(err, repository.ErrConflict) {
				continue // another sender has it
			}
			return res, err
		}
		n.Attempts++
		res.Retried++
		if err := b.deliver(ctx, n); err != nil {
			return res, err
		}
	}

	used, err := repo.CountSince(ctx, startOfDayIn(now, loc))
	if err != nil {
		return res, err
	}
	consent := map[model.ID]bool{}
	send := func(purpose model.NotificationPurpose, due []repository.DueDose) error {
		for _, g := range groupDoses(due) {
			if used >= b.reg.cfg.NotificationDailyLimit {
				res.SkippedLimit++
				continue
			}
			ok, seen := consent[g.patientID]
			if !seen {
				granted, err := b.reminderConsent(ctx, g.patientID)
				if err != nil {
					return err
				}
				ok, consent[g.patientID] = granted, granted
			}
			if !ok {
				res.SkippedNoConsent++
				continue
			}
			n, err := b.sendDoseMessage(ctx, purpose, g, loc, nil, repository.ClaimExclusive)
			var se *Error
			switch {
			case errors.As(err, &se) && se.Code == "clinical_detail":
				res.Refused++
			case err != nil:
				return err
			case n == nil:
				continue // claimed elsewhere
			case purpose == model.PurposeDoseReminder:
				res.Reminders++
			default:
				res.FollowUps++
			}
			used++
		}
		return nil
	}

	due, err := repo.DueForReminder(ctx, now, now.Add(-reminderLateness))
	if err != nil {
		return res, err
	}
	if err := send(model.PurposeDoseReminder, due); err != nil {
		return res, err
	}
	if after := b.reg.followUpAfter(t); after > 0 {
		due, err := repo.DueForFollowUp(ctx, now.Add(-after), now.Add(-after-reminderLateness))
		if err != nil {
			return res, err
		}
		if err := send(model.PurposeDoseFollowUp, due); err != nil {
			return res, err
		}
	}
	return res, nil
}

func startOfDayIn(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// messagingPractice loads the practice and checks it can send at all.
func (b *base) messagingPractice(ctx context.Context) (*model.Tenant, error) {
	if b.reg.sender == nil {
		return nil, ErrMessagingNotConfigured
	}
	t, err := repository.NewPracticeRepository(b.scope).Get(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case t.Settings.Demo && !b.reg.allowDemo:
		return nil, ErrDemoPractice
	case !t.Settings.WhatsAppEnabled:
		return nil, ErrMessagingOff
	}
	return t, nil
}

// messagePatient loads a patient and checks they can be messaged.
func (b *base) messagePatient(ctx context.Context, patientID model.ID) (*model.Patient, error) {
	p, err := b.patients().Get(ctx, patientID)
	if err != nil {
		return nil, fromRepo(err)
	}
	if p.Status != model.PatientActive {
		return nil, ErrPatientInactive
	}
	if p.PreferredChannel != model.ChannelWhatsApp || p.Mobile == "" {
		return nil, notOnWhatsApp(p)
	}
	ok, err := b.reminderConsent(ctx, patientID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrCommsConsentRequired
	}
	return p, nil
}

func (b *base) underDailyLimit(ctx context.Context, loc *time.Location) error {
	used, err := b.notifications().CountSince(ctx, startOfDayIn(b.now(), loc))
	if err != nil {
		return err
	}
	if used >= b.reg.cfg.NotificationDailyLimit {
		return ErrNotificationLimit
	}
	return nil
}

// SendReminder reminds a patient now about their latest unanswered doses
// that are due, when the automatic reminder was missed or needs repeating.
func (s *NotificationService) SendReminder(ctx context.Context, patientID model.ID) (*model.Notification, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	t, err := s.messagingPractice(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.messagePatient(ctx, patientID)
	if err != nil {
		return nil, err
	}
	loc := practiceLocation(t.Timezone)
	if err := s.underDailyLimit(ctx, loc); err != nil {
		return nil, err
	}
	now := s.now()
	due, err := s.notifications().PatientDueDoses(ctx, patientID, now, now.Add(-s.reg.cfg.DoseGrace))
	if err != nil {
		return nil, err
	}
	if len(due) == 0 {
		return nil, ErrNothingDue
	}
	// The latest moment with doses due; one message covers all its doses.
	g := doseGroup{patientID: p.ID, firstName: p.FirstName, mobile: p.Mobile, at: due[0].ScheduledAt}
	for _, d := range due {
		if d.ScheduledAt.Equal(g.at) {
			g.doseIDs = append(g.doseIDs, d.ID)
		}
	}
	n, err := s.sendDoseMessage(ctx, model.PurposeDoseReminder, g, loc, idp(s.actor.UserID), repository.ClaimMark)
	if err != nil {
		return nil, err
	}
	return s.notifications().Get(ctx, n.ID)
}

// SendRecall asks a patient to book a check-up, typically from a recall
// list. At most one a week per patient.
func (s *NotificationService) SendRecall(ctx context.Context, patientID model.ID) (*model.Notification, error) {
	if err := s.requireStaff(); err != nil {
		return nil, err
	}
	t, err := s.messagingPractice(ctx)
	if err != nil {
		return nil, err
	}
	if t.Phone == "" {
		return nil, ErrPracticePhoneMissing
	}
	p, err := s.messagePatient(ctx, patientID)
	if err != nil {
		return nil, err
	}
	loc := practiceLocation(t.Timezone)
	if err := s.underDailyLimit(ctx, loc); err != nil {
		return nil, err
	}
	repo := s.notifications()
	last, err := repo.LastRecall(ctx, patientID)
	if err != nil {
		return nil, err
	}
	if last != nil && s.now().Sub(*last) < recallInterval {
		return nil, &Error{Kind: KindConflict, Code: "recently_recalled",
			Message: "This patient was sent a recall in the last seven days.", Details: map[string]any{"last_recall": last.UTC()}}
	}
	params := []string{p.FirstName, t.Settings.SenderName(t.Name), phone.Display(t.Phone)}
	if err := s.checkParams(ctx, params); err != nil {
		var ce *notify.ClinicalDetailError
		if errors.As(err, &ce) {
			s.recordWrite(ctx, model.AuditEntry{Action: model.ActionNotificationRefused, Outcome: model.OutcomeDenied,
				SubjectType: "patient", SubjectID: idp(patientID), Reason: err.Error(), Metadata: map[string]any{"purpose": "recall"}})
			return nil, clinicalDetail(err)
		}
		return nil, err
	}
	n, err := s.newNotification(model.PurposeRecall, notify.Recall, patientID, p.Mobile, params, nil, idp(s.actor.UserID))
	if err != nil {
		return nil, err
	}
	if err := repo.Create(ctx, n, repository.ClaimNone); err != nil {
		return nil, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionNotificationQueued, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID), Metadata: map[string]any{"notification_id": n.ID.String(), "purpose": "recall"}})
	if err := s.deliver(ctx, n); err != nil {
		return nil, err
	}
	return repo.Get(ctx, n.ID)
}

// History lists what was sent to a patient and what they sent back.
func (s *NotificationService) History(ctx context.Context, patientID model.ID) (*model.PatientMessages, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	repo := s.notifications()
	sent, err := repo.ForPatient(ctx, patientID, 200)
	if err != nil {
		return nil, err
	}
	replies, err := repo.RepliesForPatient(ctx, patientID, 200)
	if err != nil {
		return nil, err
	}
	if err := s.readAudited(ctx, model.ActionNotificationsRead, "patient", patientID,
		map[string]any{"notifications": len(sent), "replies": len(replies)}); err != nil {
		return nil, err
	}
	return &model.PatientMessages{Notifications: sent, Replies: replies}, nil
}

// MessagingHealth is the practice's messaging at a glance: counts only, no
// patient identifiers.
type MessagingHealth struct {
	Configured      bool        `json:"configured"`
	Enabled         bool        `json:"enabled"`
	Active          bool        `json:"active"`
	DailyLimit      int         `json:"daily_limit"`
	SentToday       int         `json:"sent_today"`
	Last24Hours     HealthCount `json:"last_24_hours"`
	Last7Days       HealthCount `json:"last_7_days"`
	LastSentAt      *time.Time  `json:"last_sent_at,omitempty"`
	LastFailedAt    *time.Time  `json:"last_failed_at,omitempty"`
	LastErrorCode   string      `json:"last_error_code,omitempty"`
	LastError       string      `json:"last_error,omitempty"`
	FollowUpMinutes int         `json:"follow_up_minutes"`
}

// HealthCount is messages by status over a period.
type HealthCount struct {
	Total     int `json:"total"`
	Queued    int `json:"queued"`
	Sent      int `json:"sent"`
	Delivered int `json:"delivered"`
	Read      int `json:"read"`
	Failed    int `json:"failed"`
	Replies   int `json:"replies"`
}

func healthCount(h *repository.Health) HealthCount {
	return HealthCount{Total: h.Total, Queued: h.Queued, Sent: h.Sent, Delivered: h.Delivered, Read: h.Read,
		Failed: h.Failed, Replies: h.Replies}
}

// Health reports whether messaging works for this practice.
func (s *NotificationService) Health(ctx context.Context) (*MessagingHealth, error) {
	if err := s.requireStaff(); err != nil {
		return nil, err
	}
	t, err := repository.NewPracticeRepository(s.scope).Get(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	loc := practiceLocation(t.Timezone)
	repo := s.notifications()
	day, err := repo.Health(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	week, err := repo.Health(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		return nil, err
	}
	today, err := repo.CountSince(ctx, startOfDayIn(now, loc))
	if err != nil {
		return nil, err
	}
	return &MessagingHealth{
		Configured: s.reg.sender != nil, Enabled: t.Settings.WhatsAppEnabled, Active: s.reg.messagingOn(t),
		DailyLimit: s.reg.cfg.NotificationDailyLimit, SentToday: today,
		Last24Hours: healthCount(day), Last7Days: healthCount(week),
		LastSentAt: week.LastSentAt, LastFailedAt: week.LastFailedAt, LastErrorCode: week.LastErrorCode,
		LastError: week.LastErrorDetail, FollowUpMinutes: int(s.reg.followUpAfter(t) / time.Minute),
	}, nil
}

// --- webhooks ------------------------------------------------------------------

// WebhookResult counts what one webhook delivery changed.
type WebhookResult struct {
	Receipts, Replies, DosesAnswered, OptOuts, Ignored int
}

// HandleWhatsAppEvent applies a verified webhook: delivery receipts move
// messages forward, and replies answer doses or withdraw consent. Each is
// matched to the practice that sent the message it refers to, and handled
// in that practice's scope.
func (r *Registry) HandleWhatsAppEvent(ctx context.Context, ev *notify.Event) (WebhookResult, error) {
	var res WebhookResult
	ours := func(id string) bool { return r.phoneID == "" || id == "" || id == r.phoneID }
	for _, st := range ev.Statuses {
		if !ours(st.PhoneNumberID) {
			res.Ignored++
			continue
		}
		ref, err := r.platform.NotificationByProviderID(ctx, st.MessageID)
		if errors.Is(err, repository.ErrNotFound) {
			res.Ignored++
			continue
		}
		if err != nil {
			return res, err
		}
		b, err := r.systemBase(ref.TenantID)
		if err != nil {
			return res, err
		}
		changed, err := b.applyReceipt(ctx, ref.NotificationID, st)
		if err != nil {
			return res, err
		}
		if changed {
			res.Receipts++
		}
	}
	for _, m := range ev.Messages {
		if !ours(m.PhoneNumberID) {
			res.Ignored++
			continue
		}
		ref, answer, err := r.resolveReply(ctx, m)
		if err != nil {
			return res, err
		}
		if ref == nil {
			res.Ignored++
			continue
		}
		b, err := r.systemBase(ref.TenantID)
		if err != nil {
			return res, err
		}
		out, applied, err := b.applyReply(ctx, ref.NotificationID, m, answer)
		if err != nil {
			return res, err
		}
		if !applied {
			res.Ignored++
			continue
		}
		res.Replies++
		res.DosesAnswered += len(out.DosesUpdated)
		if out.OptedOut {
			res.OptOuts++
		}
	}
	return res, nil
}

func (r *Registry) systemBase(tenantID model.ID) (*base, error) {
	scope, err := tenant.NewScope(r.db, tenantID)
	if err != nil {
		return nil, err
	}
	return &base{reg: r, scope: scope, state: StateActive}, nil
}

// resolveReply finds the message a reply answers: the one whose button was
// pressed, the one swiped to reply to, or else the latest message that
// reached the sender's number. A button press is matched by its payload
// alone: one that names no message of ours is ignored rather than read as
// typed text, because it is not an answer to anything we asked.
func (r *Registry) resolveReply(ctx context.Context, m notify.InboundMessage) (*repository.NotificationRef, model.ReplyInterpretation, error) {
	if m.ButtonPayload != "" {
		token, answer, ok := notify.ParsePayload(m.ButtonPayload)
		if !ok {
			return nil, "", nil
		}
		ref, err := r.platform.NotificationByReplyToken(ctx, token)
		if errors.Is(err, repository.ErrNotFound) {
			return nil, "", nil
		}
		return ref, answer, err
	}
	answer := notify.InterpretReply(m.Text)
	if m.ContextID != "" {
		ref, err := r.platform.NotificationByProviderID(ctx, m.ContextID)
		if err == nil || !errors.Is(err, repository.ErrNotFound) {
			return ref, answer, err
		}
	}
	ref, err := r.platform.LatestNotificationTo(ctx, m.From, r.now().Add(-replyLookback))
	if errors.Is(err, repository.ErrNotFound) {
		return nil, answer, nil
	}
	return ref, answer, err
}

var receiptStatuses = map[string]model.NotificationStatus{
	"sent": model.NotificationSent, "delivered": model.NotificationDelivered,
	"read": model.NotificationRead, "failed": model.NotificationFailed,
}

func (b *base) applyReceipt(ctx context.Context, id model.ID, st notify.StatusUpdate) (bool, error) {
	status, ok := receiptStatuses[st.Status]
	if !ok {
		return false, nil
	}
	now := b.now()
	at := st.At
	if at.IsZero() || at.After(now) {
		at = now
	}
	changed, err := b.notifications().ApplyReceipt(ctx, id, status, at.UTC().Truncate(time.Millisecond), st.ErrorCode, st.ErrorDetail, now)
	if err != nil || !changed || status != model.NotificationFailed {
		return changed, err
	}
	n, err := b.notifications().Get(ctx, id)
	if err != nil {
		return true, err
	}
	b.recordWrite(ctx, model.AuditEntry{Action: model.ActionNotificationFailed, Outcome: model.OutcomeFailure,
		SubjectType: "patient", SubjectID: idp(n.PatientID), Reason: st.ErrorDetail,
		Metadata: map[string]any{"notification_id": id.String(), "error_code": st.ErrorCode, "reported_by": "provider"}})
	return true, nil
}

// applyReply records a reply and applies it. A reply from a number other
// than the one the message went to is ignored: whoever that is, they are
// not the patient the doses belong to.
func (b *base) applyReply(ctx context.Context, id model.ID, m notify.InboundMessage, answer model.ReplyInterpretation) (repository.ReplyOutcome, bool, error) {
	repo := b.notifications()
	n, err := repo.Get(ctx, id)
	if err != nil {
		return repository.ReplyOutcome{}, false, err
	}
	if n.Destination != m.From {
		return repository.ReplyOutcome{}, false, nil
	}
	now := b.now()
	at := m.At
	if at.IsZero() || at.After(now) {
		at = now
	}
	reply := &model.NotificationReply{
		ID: model.MustNewID(), PatientID: n.PatientID, NotificationID: &n.ID, ProviderID: m.ID,
		ReceivedAt: at.UTC().Truncate(time.Millisecond), Interpretation: answer, Body: m.Text, CreatedAt: now,
	}
	if reply.Body == "" && m.Type != "" && m.Type != "text" {
		reply.Body = "(" + m.Type + ")"
	}
	var doses []model.ID
	if n.Purpose.IsDose() {
		doses = n.DoseIDs
	}
	out, err := repo.RecordReply(ctx, reply, doses, "Replied STOP to a WhatsApp message")
	if err != nil || out.Duplicate {
		return out, false, err // a duplicate delivery was applied the first time
	}
	b.recordWrite(ctx, model.AuditEntry{Action: model.ActionReplyReceived, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(n.PatientID),
		Metadata: map[string]any{"notification_id": n.ID.String(), "interpretation": string(answer), "doses_answered": len(out.DosesUpdated)}})
	if len(out.DosesUpdated) > 0 {
		ids := make([]string, len(out.DosesUpdated))
		for i, d := range out.DosesUpdated {
			ids[i] = d.String()
		}
		b.recordWrite(ctx, model.AuditEntry{Action: model.ActionDoseResponded, Outcome: model.OutcomeSuccess,
			SubjectType: "patient", SubjectID: idp(n.PatientID),
			Metadata: map[string]any{"dose_ids": ids, "status": string(answer), "channel": "whatsapp"}})
	}
	if out.OptedOut {
		b.recordWrite(ctx, model.AuditEntry{Action: model.ActionConsentRecorded, Outcome: model.OutcomeSuccess,
			SubjectType: "patient", SubjectID: idp(n.PatientID),
			Metadata: map[string]any{"purpose": string(model.ConsentReminderComms), "granted": false, "method": "electronic", "via": "whatsapp_reply"}})
		b.refreshSchedule(ctx, n.PatientID)
	}
	return out, true, nil
}
