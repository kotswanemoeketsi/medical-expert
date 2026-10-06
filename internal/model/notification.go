package model

import "time"

// NotificationPurpose is why a message was sent.
type NotificationPurpose string

const (
	PurposeDoseReminder NotificationPurpose = "dose_reminder"
	PurposeDoseFollowUp NotificationPurpose = "dose_follow_up"
	PurposeRecall       NotificationPurpose = "recall"
)

// IsDose reports whether the message asks about doses, so a reply to it can
// answer them.
func (p NotificationPurpose) IsDose() bool {
	return p == PurposeDoseReminder || p == PurposeDoseFollowUp
}

// NotificationStatus follows a message through the provider:
// queued → sent → delivered → read, or failed.
type NotificationStatus string

const (
	NotificationQueued    NotificationStatus = "queued"
	NotificationSent      NotificationStatus = "sent"
	NotificationDelivered NotificationStatus = "delivered"
	NotificationRead      NotificationStatus = "read"
	NotificationFailed    NotificationStatus = "failed"
)

// Notification is one outgoing message. Params and the reply token stay
// server-side; staff see the preview, which is the message as the patient
// reads it. WhatsApp is the only channel built: a patient who prefers SMS
// gets nothing rather than being switched silently, because they may have
// chosen SMS precisely because they have no smartphone.
type Notification struct {
	ID                ID                  `json:"id"`
	PatientID         ID                  `json:"patient_id"`
	Channel           Channel             `json:"channel"`
	Purpose           NotificationPurpose `json:"purpose"`
	Destination       string              `json:"destination"`
	TemplateName      string              `json:"template_name"`
	TemplateLanguage  string              `json:"template_language"`
	Params            []string            `json:"-"`
	Preview           string              `json:"preview"`
	ReplyToken        string              `json:"-"`
	Status            NotificationStatus  `json:"status"`
	Attempts          int                 `json:"attempts"`
	NextAttemptAt     *time.Time          `json:"-"`
	ProviderMessageID string              `json:"provider_message_id,omitempty"`
	ErrorCode         string              `json:"error_code,omitempty"`
	ErrorDetail       string              `json:"error_detail,omitempty"`
	CreatedBy         *ID                 `json:"created_by,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
	SentAt            *time.Time          `json:"sent_at,omitempty"`
	DeliveredAt       *time.Time          `json:"delivered_at,omitempty"`
	ReadAt            *time.Time          `json:"read_at,omitempty"`
	FailedAt          *time.Time          `json:"failed_at,omitempty"`
	UpdatedAt         time.Time           `json:"updated_at"`
	DoseIDs           []ID                `json:"dose_ids,omitempty"`
}

// ReplyInterpretation is what the system made of a patient's reply. Anything
// it cannot read with certainty is "unrecognised" and changes nothing:
// guessing would put a confident wrong entry in a clinical record.
type ReplyInterpretation string

const (
	ReplyTaken        ReplyInterpretation = "taken"
	ReplySkipped      ReplyInterpretation = "skipped"
	ReplyStop         ReplyInterpretation = "stop"
	ReplyUnrecognised ReplyInterpretation = "unrecognised"
)

// NotificationReply is a message a patient sent back.
type NotificationReply struct {
	ID             ID                  `json:"id"`
	PatientID      ID                  `json:"patient_id"`
	NotificationID *ID                 `json:"notification_id,omitempty"`
	ProviderID     string              `json:"-"`
	ReceivedAt     time.Time           `json:"received_at"`
	Interpretation ReplyInterpretation `json:"interpretation"`
	Body           string              `json:"body,omitempty"`
	DosesUpdated   int                 `json:"doses_updated"`
	CreatedAt      time.Time           `json:"created_at"`
}

// PatientMessages is a patient's message history, both directions.
type PatientMessages struct {
	Notifications []Notification      `json:"notifications"`
	Replies       []NotificationReply `json:"replies"`
}
