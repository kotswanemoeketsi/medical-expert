package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Message is one template message to one number.
type Message struct {
	To       string // E.164, e.g. +27821234567
	Template Template
	Params   []string
	// ButtonPayloads, one per template button, come back with the press.
	ButtonPayloads []string
}

// Sender sends template messages. *Client is the real one; tests and the
// demo seeder use stand-ins.
type Sender interface {
	Send(ctx context.Context, m Message) (providerMessageID string, err error)
	Language() string
}

// SendError is a refusal or failure reported by the provider, or a failure
// to reach it. Permanent errors will fail again however often they are
// retried: a number not on WhatsApp, a template not approved.
type SendError struct {
	Permanent bool
	Status    int    // HTTP status, 0 if the provider was not reached
	Code      string // the provider's error code
	Detail    string
}

func (e *SendError) Error() string {
	kind := "temporary"
	if e.Permanent {
		kind = "permanent"
	}
	if e.Code != "" {
		return fmt.Sprintf("whatsapp: %s error %s: %s", kind, e.Code, e.Detail)
	}
	return fmt.Sprintf("whatsapp: %s error: %s", kind, e.Detail)
}

// IsPermanent reports whether retrying err is pointless.
func IsPermanent(err error) bool {
	var se *SendError
	return errors.As(err, &se) && se.Permanent
}

// Config is the WhatsApp Cloud API account.
type Config struct {
	PhoneNumberID string
	AccessToken   string
	APIVersion    string // default v23.0
	BaseURL       string // default https://graph.facebook.com
	Language      string // template language code, default en
	Timeout       time.Duration
	HTTPClient    *http.Client
}

// Client talks to the WhatsApp Cloud API.
type Client struct {
	cfg  Config
	http *http.Client
}

// New returns a client, or an error if the account is incomplete.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.PhoneNumberID) == "" || strings.TrimSpace(cfg.AccessToken) == "" {
		return nil, errors.New("notify: a phone number ID and an access token are both required")
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = "v23.0"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://graph.facebook.com"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Language == "" {
		cfg.Language = "en"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{cfg: cfg, http: hc}, nil
}

func (c *Client) Language() string { return c.cfg.Language }

// PhoneNumberID is the sending number, which webhooks name as theirs.
func (c *Client) PhoneNumberID() string { return c.cfg.PhoneNumberID }

type param struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Payload string `json:"payload,omitempty"`
}

type component struct {
	Type       string  `json:"type"`
	SubType    string  `json:"sub_type,omitempty"`
	Index      string  `json:"index,omitempty"`
	Parameters []param `json:"parameters"`
}

// RequestBody is the JSON sent for m. Exported so tests can check exactly
// what leaves the building.
func (c *Client) RequestBody(m Message) ([]byte, error) {
	if len(m.Params) != len(m.Template.Params) {
		return nil, fmt.Errorf("notify: template %s takes %d parameters, got %d", m.Template.Name, len(m.Template.Params), len(m.Params))
	}
	if len(m.ButtonPayloads) != len(m.Template.Buttons) {
		return nil, fmt.Errorf("notify: template %s has %d buttons, got %d payloads", m.Template.Name, len(m.Template.Buttons), len(m.ButtonPayloads))
	}
	to := strings.TrimPrefix(strings.TrimSpace(m.To), "+")
	if to == "" {
		return nil, errors.New("notify: no destination number")
	}
	body := component{Type: "body", Parameters: []param{}}
	for _, p := range m.Params {
		body.Parameters = append(body.Parameters, param{Type: "text", Text: p})
	}
	components := []component{body}
	for i, payload := range m.ButtonPayloads {
		components = append(components, component{Type: "button", SubType: "quick_reply", Index: strconv.Itoa(i),
			Parameters: []param{{Type: "payload", Payload: payload}}})
	}
	return json.Marshal(map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                to,
		"type":              "template",
		"template": map[string]any{
			"name":       m.Template.Name,
			"language":   map[string]string{"code": c.cfg.Language},
			"components": components,
		},
	})
}

// Send makes one attempt. Retrying is the caller's decision, guided by
// IsPermanent.
func (c *Client) Send(ctx context.Context, m Message) (string, error) {
	payload, err := c.RequestBody(m)
	if err != nil {
		return "", &SendError{Permanent: true, Detail: err.Error()}
	}
	url := fmt.Sprintf("%s/%s/%s/messages", c.cfg.BaseURL, c.cfg.APIVersion, c.cfg.PhoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", &SendError{Permanent: true, Detail: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		// The error text names the URL, which holds no secret: the token
		// travels in a header and never reaches a log.
		return "", &SendError{Detail: "could not reach the WhatsApp API: " + trimErr(err)}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode/100 == 2 {
		var ok struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &ok); err != nil || len(ok.Messages) == 0 || ok.Messages[0].ID == "" {
			// Accepted but unreadable: the message may well be on its way, so
			// sending again risks a duplicate. Treat as permanent.
			return "", &SendError{Permanent: true, Status: res.StatusCode, Detail: "the API accepted the message but returned no message ID"}
		}
		return ok.Messages[0].ID, nil
	}
	return "", classify(res.StatusCode, raw)
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// transientCodes are provider errors worth retrying: rate limits and the
// provider's own trouble.
var transientCodes = map[int]bool{
	1: true, 2: true, 4: true, 17: true, 341: true, 80007: true, 130429: true, 131000: true, 131016: true,
	131048: true, 131056: true, 133004: true,
}

func classify(status int, raw []byte) *SendError {
	var e struct {
		Error struct {
			Message   string `json:"message"`
			Code      int    `json:"code"`
			ErrorData struct {
				Details string `json:"details"`
			} `json:"error_data"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	detail := e.Error.Message
	if e.Error.ErrorData.Details != "" {
		detail = strings.TrimSpace(detail + ": " + e.Error.ErrorData.Details)
	}
	if detail == "" {
		detail = http.StatusText(status)
	}
	if len(detail) > 400 {
		detail = detail[:400]
	}
	se := &SendError{Status: status, Detail: detail}
	if e.Error.Code != 0 {
		se.Code = strconv.Itoa(e.Error.Code)
	}
	switch {
	case status == http.StatusTooManyRequests || status >= 500:
		se.Permanent = false
	case transientCodes[e.Error.Code]:
		se.Permanent = false
	default:
		se.Permanent = true
	}
	return se
}
