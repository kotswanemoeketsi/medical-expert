package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yourorg/medical-expert-system/internal/phone"
)

// ErrNoAppSecret means webhook signatures cannot be checked, so every
// webhook is refused rather than trusted unsigned.
var ErrNoAppSecret = errors.New("notify: no app secret is configured, so webhooks cannot be verified and are refused")

// ErrBadSignature means the body was not signed with the app secret.
var ErrBadSignature = errors.New("notify: webhook signature does not match")

// VerifySignature checks Meta's X-Hub-Signature-256 header against the raw
// body. The webhook is public by necessity, so this is the only thing
// between the internet and the dose record: it runs before the body is
// parsed at all.
func VerifySignature(appSecret string, body []byte, header string) error {
	if appSecret == "" {
		return ErrNoAppSecret
	}
	hexSig, ok := strings.CutPrefix(strings.TrimSpace(header), "sha256=")
	if !ok {
		return ErrBadSignature
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrBadSignature
	}
	return nil
}

// Sign returns the header value Meta would send for body. For tests and
// local tools.
func Sign(appSecret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySubscription answers Meta's one-off check when the webhook is set up:
// it returns the challenge to echo if the verify token matches.
func VerifySubscription(q url.Values, verifyToken string) (string, bool) {
	if verifyToken == "" || q.Get("hub.mode") != "subscribe" {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("hub.verify_token")), []byte(verifyToken)) != 1 {
		return "", false
	}
	challenge := q.Get("hub.challenge")
	return challenge, challenge != ""
}

// StatusUpdate is a delivery receipt for a message we sent.
type StatusUpdate struct {
	PhoneNumberID string // our number, as the webhook names it
	MessageID     string
	Status        string // sent, delivered, read or failed
	At            time.Time
	ErrorCode     string
	ErrorDetail   string
}

// InboundMessage is something a patient sent us.
type InboundMessage struct {
	PhoneNumberID string
	ID            string
	From          string // E.164
	At            time.Time
	Type          string
	Text          string // what they typed, or the label of the button they pressed
	ButtonPayload string // set for a button press
	ContextID     string // the message they replied to, when they used reply
}

// Event is everything one webhook delivery carries.
type Event struct {
	Statuses []StatusUpdate
	Messages []InboundMessage
}

type webhookBody struct {
	Object string `json:"object"`
	Entry  []struct {
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Messages []struct {
					From      string `json:"from"`
					ID        string `json:"id"`
					Timestamp string `json:"timestamp"`
					Type      string `json:"type"`
					Text      struct {
						Body string `json:"body"`
					} `json:"text"`
					Button struct {
						Payload string `json:"payload"`
						Text    string `json:"text"`
					} `json:"button"`
					Interactive struct {
						ButtonReply struct {
							ID    string `json:"id"`
							Title string `json:"title"`
						} `json:"button_reply"`
					} `json:"interactive"`
					Context struct {
						ID string `json:"id"`
					} `json:"context"`
				} `json:"messages"`
				Statuses []struct {
					ID        string `json:"id"`
					Status    string `json:"status"`
					Timestamp string `json:"timestamp"`
					Errors    []struct {
						Code      int    `json:"code"`
						Title     string `json:"title"`
						Message   string `json:"message"`
						ErrorData struct {
							Details string `json:"details"`
						} `json:"error_data"`
					} `json:"errors"`
				} `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

func unixTime(s string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// Do not cut a multi-byte character in half.
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// ParseWebhook reads a verified webhook body. Anything it does not
// understand is skipped rather than rejected: Meta adds fields, and a
// webhook answered with an error is retried for days.
func ParseWebhook(body []byte) (*Event, error) {
	var w webhookBody
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, errors.New("notify: webhook body is not JSON")
	}
	ev := &Event{}
	if w.Object != "whatsapp_business_account" {
		return ev, nil
	}
	for _, entry := range w.Entry {
		for _, ch := range entry.Changes {
			if ch.Field != "messages" {
				continue
			}
			pnid := ch.Value.Metadata.PhoneNumberID
			for _, s := range ch.Value.Statuses {
				if s.ID == "" {
					continue
				}
				u := StatusUpdate{PhoneNumberID: pnid, MessageID: s.ID, Status: s.Status, At: unixTime(s.Timestamp)}
				if len(s.Errors) > 0 {
					e := s.Errors[0]
					u.ErrorCode = strconv.Itoa(e.Code)
					u.ErrorDetail = e.Title
					if e.ErrorData.Details != "" {
						u.ErrorDetail = strings.TrimSpace(u.ErrorDetail + ": " + e.ErrorData.Details)
					} else if e.Message != "" && e.Message != e.Title {
						u.ErrorDetail = strings.TrimSpace(u.ErrorDetail + ": " + e.Message)
					}
					u.ErrorDetail = clip(u.ErrorDetail, 500)
				}
				ev.Statuses = append(ev.Statuses, u)
			}
			for _, m := range ch.Value.Messages {
				from, err := phone.NormalizeE164("+" + strings.TrimPrefix(m.From, "+"))
				if m.ID == "" || err != nil {
					continue
				}
				in := InboundMessage{PhoneNumberID: pnid, ID: m.ID, From: from, At: unixTime(m.Timestamp),
					Type: m.Type, ContextID: m.Context.ID}
				switch m.Type {
				case "text":
					in.Text = m.Text.Body
				case "button":
					in.Text, in.ButtonPayload = m.Button.Text, m.Button.Payload
				case "interactive":
					in.Text, in.ButtonPayload = m.Interactive.ButtonReply.Title, m.Interactive.ButtonReply.ID
				}
				in.Text = clip(in.Text, 1000)
				ev.Messages = append(ev.Messages, in)
			}
		}
	}
	return ev, nil
}
