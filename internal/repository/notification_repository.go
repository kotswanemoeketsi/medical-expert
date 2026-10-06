package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// NotificationRepository stores outgoing messages, the doses they were
// about, and patients' replies.
type NotificationRepository struct {
	scope *tenant.Scope
}

func NewNotificationRepository(s *tenant.Scope) *NotificationRepository {
	return &NotificationRepository{scope: s}
}

const insertNotification = `
INSERT INTO notifications (tenant_id, id, patient_id, channel, purpose, destination, template_name, template_language,
                           params_json, preview, reply_token, status, attempts, next_attempt_at, error_code,
                           error_detail, created_by, created_at, failed_at, updated_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const insertNotificationDose = `
INSERT INTO notification_doses (tenant_id, notification_id, dose_event_id) VALUES (:tenant, ?, ?)`

// Claims. The condition is in the WHERE clause, so of two dispatchers
// racing for the same dose exactly one matches.
const claimForReminder = `
UPDATE dose_events SET reminded_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'pending' AND reminded_at IS NULL`

const claimForFollowUp = `
UPDATE dose_events SET followed_up_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'pending' AND reminded_at IS NOT NULL AND followed_up_at IS NULL`

var doseClaims = map[model.NotificationPurpose]string{
	model.PurposeDoseReminder: claimForReminder,
	model.PurposeDoseFollowUp: claimForFollowUp,
}

// A reminder sent by hand marks its doses reminded, so the dispatcher does
// not send a second one, but does not need them unclaimed.
const markReminded = `
UPDATE dose_events SET reminded_at = COALESCE(reminded_at, ?)
 WHERE tenant_id = :tenant AND id = ? AND status = 'pending'`

// ClaimMode says how a notification takes hold of its doses.
type ClaimMode int

const (
	// ClaimNone: the message is not about doses.
	ClaimNone ClaimMode = iota
	// ClaimExclusive: every dose must still be unclaimed for this purpose,
	// or nothing is written. The dispatcher uses it.
	ClaimExclusive
	// ClaimMark: doses are marked reminded if they were not. A reminder sent
	// by hand uses it.
	ClaimMark
)

func (r *NotificationRepository) insert(ctx context.Context, tx *tenant.Scope, n *model.Notification) error {
	params, err := json.Marshal(n.Params)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, insertNotification, n.ID.Bytes(), n.PatientID.Bytes(), string(n.Channel), string(n.Purpose),
		n.Destination, n.TemplateName, n.TemplateLanguage, string(params), n.Preview, nullString(n.ReplyToken),
		string(n.Status), n.Attempts, nullTime(n.NextAttemptAt), nullString(n.ErrorCode), nullString(n.ErrorDetail),
		nullID(n.CreatedBy), n.CreatedAt, nullTime(n.FailedAt), n.UpdatedAt)
	return translate(err)
}

// Create writes a notification and links its doses, claiming them as mode
// says, in one transaction. With ClaimExclusive, if any dose has already
// been claimed — by another dispatcher, or answered meanwhile — nothing is
// written and ErrConflict is returned.
func (r *NotificationRepository) Create(ctx context.Context, n *model.Notification, mode ClaimMode) error {
	return r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		switch mode {
		case ClaimExclusive:
			if _, ok := doseClaims[n.Purpose]; !ok {
				return errors.New("repository: only dose reminders claim doses")
			}
			for _, id := range n.DoseIDs {
				if err := expectOne(tx.Exec(ctx, doseClaims[n.Purpose], n.CreatedAt, id.Bytes())); err != nil {
					return err
				}
			}
		case ClaimMark:
			for _, id := range n.DoseIDs {
				if _, err := tx.Exec(ctx, markReminded, n.CreatedAt, id.Bytes()); err != nil {
					return err
				}
			}
		}
		if err := r.insert(ctx, tx, n); err != nil {
			return err
		}
		for _, id := range n.DoseIDs {
			if _, err := tx.Exec(ctx, insertNotificationDose, n.ID.Bytes(), id.Bytes()); err != nil {
				return translate(err)
			}
		}
		return nil
	})
}

const notificationFrom = ` id, patient_id, channel, purpose, destination, template_name, template_language, params_json,
       preview, reply_token, status, attempts, next_attempt_at, provider_message_id, error_code, error_detail,
       created_by, created_at, sent_at, delivered_at, read_at, failed_at, updated_at
  FROM notifications`

func scanNotification(sc scanner) (*model.Notification, error) {
	var n model.Notification
	var params []byte
	var token, providerID, code, detail sql.Null[string]
	var next, sent, delivered, read, failed sql.Null[time.Time]
	var by sql.Null[model.ID]
	if err := sc.Scan(&n.ID, &n.PatientID, &n.Channel, &n.Purpose, &n.Destination, &n.TemplateName, &n.TemplateLanguage,
		&params, &n.Preview, &token, &n.Status, &n.Attempts, &next, &providerID, &code, &detail,
		&by, &n.CreatedAt, &sent, &delivered, &read, &failed, &n.UpdatedAt); err != nil {
		return nil, translate(err)
	}
	if err := json.Unmarshal(params, &n.Params); err != nil {
		return nil, err
	}
	n.ReplyToken, n.ProviderMessageID, n.ErrorCode, n.ErrorDetail = token.V, providerID.V, code.V, detail.V
	n.NextAttemptAt, n.SentAt, n.DeliveredAt, n.ReadAt, n.FailedAt = timePtr(next), timePtr(sent), timePtr(delivered),
		timePtr(read), timePtr(failed)
	n.CreatedBy = idPtr(by)
	return &n, nil
}

func collectNotifications(rows *sql.Rows, err error) ([]model.Notification, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

const selectNotification = `SELECT` + notificationFrom + ` WHERE tenant_id = :tenant AND id = ?`

const selectNotificationDoses = `
SELECT dose_event_id FROM notification_doses WHERE tenant_id = :tenant AND notification_id = ? ORDER BY dose_event_id`

// Get returns a notification with the doses it was about.
func (r *NotificationRepository) Get(ctx context.Context, id model.ID) (*model.Notification, error) {
	n, err := scanNotification(r.scope.QueryRow(ctx, selectNotification, id.Bytes()))
	if err != nil {
		return nil, err
	}
	rows, err := r.scope.Query(ctx, selectNotificationDoses, id.Bytes())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d model.ID
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		n.DoseIDs = append(n.DoseIDs, d)
	}
	return n, rows.Err()
}

const selectPatientNotifications = `SELECT` + notificationFrom + `
 WHERE tenant_id = :tenant AND patient_id = ?
 ORDER BY created_at DESC, id
 LIMIT ?`

const selectPatientNotificationDoses = `
SELECT nd.notification_id, nd.dose_event_id
  FROM notification_doses nd
  JOIN notifications n ON n.tenant_id = nd.tenant_id AND n.id = nd.notification_id
 WHERE nd.tenant_id = :tenant AND n.patient_id = ?
 ORDER BY nd.dose_event_id`

// ForPatient lists a patient's messages, newest first, each with the doses
// it was about.
func (r *NotificationRepository) ForPatient(ctx context.Context, patientID model.ID, limit int) ([]model.Notification, error) {
	list, err := collectNotifications(r.scope.Query(ctx, selectPatientNotifications, patientID.Bytes(), limit))
	if err != nil {
		return nil, err
	}
	at := make(map[model.ID]int, len(list))
	for i := range list {
		at[list[i].ID] = i
	}
	rows, err := r.scope.Query(ctx, selectPatientNotificationDoses, patientID.Bytes())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var nid, did model.ID
		if err := rows.Scan(&nid, &did); err != nil {
			return nil, err
		}
		if i, ok := at[nid]; ok {
			list[i].DoseIDs = append(list[i].DoseIDs, did)
		}
	}
	return list, rows.Err()
}

// Lease takes the right to send: it counts the attempt and moves the next
// attempt past the time a send can take, so a second sender — another
// server, or a retry pass — leaves the message alone meanwhile.
const leaseNotification = `
UPDATE notifications SET attempts = attempts + 1, next_attempt_at = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'queued' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)`

func (r *NotificationRepository) Lease(ctx context.Context, id model.ID, now, until time.Time) error {
	return expectOne(r.scope.Exec(ctx, leaseNotification, until, now, id.Bytes(), now))
}

const markNotificationSent = `
UPDATE notifications
   SET status = 'sent', provider_message_id = ?, sent_at = ?, next_attempt_at = NULL, error_code = NULL,
       error_detail = NULL, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'queued'`

func (r *NotificationRepository) MarkSent(ctx context.Context, id model.ID, providerID string, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, markNotificationSent, providerID, now, now, id.Bytes()))
}

const markNotificationFailed = `
UPDATE notifications
   SET status = 'failed', failed_at = ?, error_code = ?, error_detail = ?, next_attempt_at = NULL, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'queued'`

func (r *NotificationRepository) MarkFailed(ctx context.Context, id model.ID, code, detail string, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, markNotificationFailed, now, nullString(code), nullString(clipText(detail, 500)), now, id.Bytes()))
}

const retryNotificationLater = `
UPDATE notifications SET next_attempt_at = ?, error_code = ?, error_detail = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'queued'`

func (r *NotificationRepository) RetryLater(ctx context.Context, id model.ID, at time.Time, code, detail string, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, retryNotificationLater, at, nullString(code), nullString(clipText(detail, 500)), now, id.Bytes()))
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

const selectDueRetries = `SELECT` + notificationFrom + `
 WHERE tenant_id = :tenant AND status = 'queued' AND next_attempt_at <= ?
 ORDER BY next_attempt_at
 LIMIT ?`

// DueForRetry lists queued messages whose next attempt is due, including
// any a crashed sender left leased.
func (r *NotificationRepository) DueForRetry(ctx context.Context, now time.Time, limit int) ([]model.Notification, error) {
	return collectNotifications(r.scope.Query(ctx, selectDueRetries, now, limit))
}

// Receipts only move a message forward: a "delivered" that arrives after
// "read" changes nothing, and a failure cannot overwrite a delivery.
const (
	applySent = `
UPDATE notifications SET status = 'sent', sent_at = COALESCE(sent_at, ?), updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status = 'queued'`
	applyDelivered = `
UPDATE notifications SET status = 'delivered', sent_at = COALESCE(sent_at, ?), delivered_at = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status IN ('queued', 'sent')`
	applyRead = `
UPDATE notifications
   SET status = 'read', sent_at = COALESCE(sent_at, ?), delivered_at = COALESCE(delivered_at, ?), read_at = ?,
       updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status IN ('queued', 'sent', 'delivered')`
)

const applyFailed = `
UPDATE notifications SET status = 'failed', failed_at = ?, error_code = ?, error_detail = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND status IN ('queued', 'sent')`

// ApplyReceipt records a delivery receipt. It reports whether anything
// changed.
func (r *NotificationRepository) ApplyReceipt(ctx context.Context, id model.ID, status model.NotificationStatus, at time.Time, code, detail string, now time.Time) (bool, error) {
	var res sql.Result
	var err error
	switch status {
	case model.NotificationSent:
		res, err = r.scope.Exec(ctx, applySent, at, now, id.Bytes())
	case model.NotificationDelivered:
		res, err = r.scope.Exec(ctx, applyDelivered, at, at, now, id.Bytes())
	case model.NotificationRead:
		res, err = r.scope.Exec(ctx, applyRead, at, at, at, now, id.Bytes())
	case model.NotificationFailed:
		res, err = r.scope.Exec(ctx, applyFailed, at, nullString(code), nullString(clipText(detail, 500)), now, id.Bytes())
	default:
		return false, nil
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// A message leased by a sender is left alone: it may be on its way.
const expireStaleNotifications = `
UPDATE notifications
   SET status = 'failed', failed_at = ?, error_code = 'expired',
       error_detail = 'Not sent in time; a late reminder would only confuse.', next_attempt_at = NULL, updated_at = ?
 WHERE tenant_id = :tenant AND status = 'queued' AND purpose IN ('dose_reminder', 'dose_follow_up') AND created_at < ?
   AND (next_attempt_at IS NULL OR next_attempt_at <= ?)`

// ExpireStale gives up on dose reminders still queued long after they were
// due.
func (r *NotificationRepository) ExpireStale(ctx context.Context, createdBefore, now time.Time) (int64, error) {
	res, err := r.scope.Exec(ctx, expireStaleNotifications, now, now, createdBefore, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const countNotificationsSince = `SELECT COUNT(*) FROM notifications WHERE tenant_id = :tenant AND created_at >= ?`

// CountSince counts messages created since a moment, for the daily limit.
func (r *NotificationRepository) CountSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := r.scope.QueryRow(ctx, countNotificationsSince, since).Scan(&n)
	return n, translate(err)
}

const selectLastRecall = `
SELECT MAX(created_at) FROM notifications
 WHERE tenant_id = :tenant AND patient_id = ? AND purpose = 'recall' AND status <> 'failed'`

// LastRecall is when the patient was last sent a recall that did not fail.
func (r *NotificationRepository) LastRecall(ctx context.Context, patientID model.ID) (*time.Time, error) {
	var at sql.Null[time.Time]
	if err := r.scope.QueryRow(ctx, selectLastRecall, patientID.Bytes()).Scan(&at); err != nil {
		return nil, translate(err)
	}
	return timePtr(at), nil
}

// --- what the dispatcher sends -------------------------------------------------

// DueDose is a dose that may need a reminder, with what the message needs.
type DueDose struct {
	DoseID      model.ID
	PatientID   model.ID
	ScheduledAt time.Time
	FirstName   string
	Mobile      string
}

// dueFrom is a fragment; each query prefixes SELECT and adds its own tenant
// predicate. Only active regimens and active patients who asked to be
// reached on WhatsApp and have a number; consent is checked per patient.
const dueFrom = ` d.id, d.patient_id, d.scheduled_at, p.first_name, p.mobile
  FROM dose_events d
  JOIN regimens r ON r.tenant_id = d.tenant_id AND r.id = d.regimen_id
  JOIN patients p ON p.tenant_id = d.tenant_id AND p.id = d.patient_id`

const selectDueForReminder = `SELECT` + dueFrom + `
 WHERE d.tenant_id = :tenant AND d.status = 'pending' AND d.reminded_at IS NULL
   AND d.scheduled_at <= ? AND d.scheduled_at > ?
   AND r.status = 'active' AND p.status = 'active' AND p.preferred_channel = 'whatsapp' AND p.mobile IS NOT NULL
 ORDER BY d.patient_id, d.scheduled_at, d.id
 LIMIT 1000`

// A follow-up goes only where the first reminder was actually sent: if it
// failed, a second message to the same number fails the same way.
const selectDueForFollowUp = `SELECT` + dueFrom + `
 WHERE d.tenant_id = :tenant AND d.status = 'pending' AND d.reminded_at IS NOT NULL AND d.followed_up_at IS NULL
   AND d.scheduled_at <= ? AND d.scheduled_at > ?
   AND r.status = 'active' AND p.status = 'active' AND p.preferred_channel = 'whatsapp' AND p.mobile IS NOT NULL
   AND EXISTS (SELECT 1 FROM notification_doses nd
                 JOIN notifications n ON n.tenant_id = nd.tenant_id AND n.id = nd.notification_id
                WHERE nd.tenant_id = d.tenant_id AND nd.dose_event_id = d.id
                  AND n.purpose = 'dose_reminder' AND n.status IN ('sent', 'delivered', 'read'))
 ORDER BY d.patient_id, d.scheduled_at, d.id
 LIMIT 1000`

func collectDue(rows *sql.Rows, err error) ([]DueDose, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueDose
	for rows.Next() {
		var d DueDose
		if err := rows.Scan(&d.DoseID, &d.PatientID, &d.ScheduledAt, &d.FirstName, &d.Mobile); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DueForReminder lists unreminded pending doses due in (after, upTo].
func (r *NotificationRepository) DueForReminder(ctx context.Context, upTo, after time.Time) ([]DueDose, error) {
	return collectDue(r.scope.Query(ctx, selectDueForReminder, upTo, after))
}

// DueForFollowUp lists reminded, still-pending doses due in (after, upTo].
func (r *NotificationRepository) DueForFollowUp(ctx context.Context, upTo, after time.Time) ([]DueDose, error) {
	return collectDue(r.scope.Query(ctx, selectDueForFollowUp, upTo, after))
}

const selectPatientDueDoses = `
SELECT d.id, d.scheduled_at FROM dose_events d
  JOIN regimens r ON r.tenant_id = d.tenant_id AND r.id = d.regimen_id
 WHERE d.tenant_id = :tenant AND d.patient_id = ? AND d.status = 'pending' AND r.status = 'active'
   AND d.scheduled_at <= ? AND d.scheduled_at > ?
 ORDER BY d.scheduled_at DESC, d.id`

// PatientDueDoses lists a patient's unanswered doses due in (after, upTo],
// latest first, for a reminder sent by hand.
func (r *NotificationRepository) PatientDueDoses(ctx context.Context, patientID model.ID, upTo, after time.Time) ([]ScheduledDose, error) {
	rows, err := r.scope.Query(ctx, selectPatientDueDoses, patientID.Bytes(), upTo, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledDose
	for rows.Next() {
		var d ScheduledDose
		if err := rows.Scan(&d.ID, &d.ScheduledAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- replies -------------------------------------------------------------------

const insertReply = `
INSERT INTO notification_replies (tenant_id, id, patient_id, notification_id, provider_message_id, received_at,
                                  interpretation, body, doses_updated, created_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// A reply answers a dose only while it is pending: a dose already answered
// in the portal, or closed as missed, keeps what it says.
const answerDoseByReply = `
UPDATE dose_events
   SET status = ?, responded_at = ?, response_channel = 'whatsapp', responded_by = NULL
 WHERE tenant_id = :tenant AND id = ? AND status = 'pending'`

const insertConsentByReply = `
INSERT INTO patient_consents (tenant_id, id, patient_id, purpose, granted, method, note, recorded_by, recorded_at)
VALUES (:tenant, ?, ?, 'reminder_communications', 0, 'electronic', ?, NULL, ?)`

// ReplyOutcome is what processing a reply changed.
type ReplyOutcome struct {
	Duplicate    bool
	DosesUpdated []model.ID
	OptedOut     bool
}

// RecordReply stores a reply and applies it, in one transaction: a taken or
// skipped answer updates the pending doses listed, a STOP withdraws consent
// to reminders. A reply already stored (the provider delivered the webhook
// twice) changes nothing and reports Duplicate.
func (r *NotificationRepository) RecordReply(ctx context.Context, reply *model.NotificationReply, doseIDs []model.ID, consentNote string) (ReplyOutcome, error) {
	var out ReplyOutcome
	err := r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		out = ReplyOutcome{}
		_, err := tx.Exec(ctx, insertReply, reply.ID.Bytes(), reply.PatientID.Bytes(), nullID(reply.NotificationID),
			reply.ProviderID, reply.ReceivedAt, string(reply.Interpretation), nullString(reply.Body), 0, reply.CreatedAt)
		if err != nil {
			if errors.Is(translate(err), ErrDuplicate) {
				out.Duplicate = true
				return nil
			}
			return translate(err)
		}
		switch reply.Interpretation {
		case model.ReplyTaken, model.ReplySkipped:
			for _, id := range doseIDs {
				res, err := tx.Exec(ctx, answerDoseByReply, string(reply.Interpretation), reply.ReceivedAt, id.Bytes())
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					out.DosesUpdated = append(out.DosesUpdated, id)
				}
			}
		case model.ReplyStop:
			id, err := model.NewID()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, insertConsentByReply, id.Bytes(), reply.PatientID.Bytes(), consentNote, reply.ReceivedAt); err != nil {
				return translate(err)
			}
			out.OptedOut = true
		}
		if len(out.DosesUpdated) > 0 {
			return expectOne(tx.Exec(ctx, updateReplyCount, len(out.DosesUpdated), reply.ID.Bytes()))
		}
		return nil
	})
	return out, err
}

const updateReplyCount = `UPDATE notification_replies SET doses_updated = ? WHERE tenant_id = :tenant AND id = ?`

const selectPatientReplies = `
SELECT id, patient_id, notification_id, provider_message_id, received_at, interpretation, body, doses_updated, created_at
  FROM notification_replies
 WHERE tenant_id = :tenant AND patient_id = ?
 ORDER BY received_at DESC, id
 LIMIT ?`

// RepliesForPatient lists what a patient sent back, newest first.
func (r *NotificationRepository) RepliesForPatient(ctx context.Context, patientID model.ID, limit int) ([]model.NotificationReply, error) {
	rows, err := r.scope.Query(ctx, selectPatientReplies, patientID.Bytes(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.NotificationReply{}
	for rows.Next() {
		var x model.NotificationReply
		var nid sql.Null[model.ID]
		var body sql.Null[string]
		if err := rows.Scan(&x.ID, &x.PatientID, &nid, &x.ProviderID, &x.ReceivedAt, &x.Interpretation, &body,
			&x.DosesUpdated, &x.CreatedAt); err != nil {
			return nil, err
		}
		x.NotificationID, x.Body = idPtr(nid), body.V
		out = append(out, x)
	}
	return out, rows.Err()
}

// --- health --------------------------------------------------------------------

// Health counts messages by status since a moment. COALESCE because SUM over
// no rows is NULL, not zero: without it the health check failed on a new
// practice's first day and worked from its first message onwards.
type Health struct {
	Total, Queued, Sent, Delivered, Read, Failed int
	Replies                                      int
	LastSentAt, LastFailedAt                     *time.Time
	LastErrorCode, LastErrorDetail               string
}

const selectHealthCounts = `
SELECT COUNT(*),
       COALESCE(SUM(status = 'queued'), 0), COALESCE(SUM(status = 'sent'), 0),
       COALESCE(SUM(status = 'delivered'), 0), COALESCE(SUM(status = 'read'), 0),
       COALESCE(SUM(status = 'failed'), 0), MAX(sent_at), MAX(failed_at)
  FROM notifications
 WHERE tenant_id = :tenant AND created_at >= ?`

const selectReplyCount = `SELECT COUNT(*) FROM notification_replies WHERE tenant_id = :tenant AND received_at >= ?`

const selectLastFailure = `
SELECT error_code, error_detail FROM notifications
 WHERE tenant_id = :tenant AND status = 'failed' AND created_at >= ?
 ORDER BY failed_at DESC
 LIMIT 1`

func (r *NotificationRepository) Health(ctx context.Context, since time.Time) (*Health, error) {
	var h Health
	var lastSent, lastFailed sql.Null[time.Time]
	if err := r.scope.QueryRow(ctx, selectHealthCounts, since).Scan(&h.Total, &h.Queued, &h.Sent, &h.Delivered,
		&h.Read, &h.Failed, &lastSent, &lastFailed); err != nil {
		return nil, translate(err)
	}
	h.LastSentAt, h.LastFailedAt = timePtr(lastSent), timePtr(lastFailed)
	if err := r.scope.QueryRow(ctx, selectReplyCount, since).Scan(&h.Replies); err != nil {
		return nil, translate(err)
	}
	var code, detail sql.Null[string]
	err := r.scope.QueryRow(ctx, selectLastFailure, since).Scan(&code, &detail)
	if err != nil && !errors.Is(translate(err), ErrNotFound) {
		return nil, err
	}
	h.LastErrorCode, h.LastErrorDetail = code.V, detail.V
	return &h, nil
}
