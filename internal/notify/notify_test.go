package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
)

func TestPreviewFillsEveryPlaceholder(t *testing.T) {
	got, err := DoseReminder.Preview([]string{"Thabo", "08:00"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "Hi Thabo, it's time for your 08:00 medicine.") || strings.Contains(got, "{{") {
		t.Fatalf("preview: %q", got)
	}
	if _, err := Recall.Preview([]string{"Thabo", "Greenside Clinic"}); err == nil {
		t.Error("a missing parameter was accepted")
	}
	if _, err := DoseReminder.Preview([]string{"Thabo", " "}); err == nil {
		t.Error("an empty parameter was accepted")
	}
	for _, tpl := range Templates {
		if !strings.Contains(tpl.Body, "STOP") {
			t.Errorf("%s has no way to opt out (BR-7.5)", tpl.Name)
		}
		if got, ok := ByName(tpl.Name); !ok || got.Name != tpl.Name {
			t.Errorf("ByName(%s) failed", tpl.Name)
		}
	}
}

func TestClinicalDetailIsRefused(t *testing.T) {
	refused := [][]string{
		{"Thabo", "Greenside HIV Clinic"},
		{"Thabo", "Diabetes Care Centre", "011 555 0123"},
		{"Thabo", "your blood pressure pills"},
		{"Your results are ready", "08:00"},
		{"Thabo", "Metformin"},
		{"Thabo", "GLUCOPHAGE"},
	}
	extra := []string{"Metformin", "Glucophage", "Amlodipine"}
	for _, params := range refused {
		var ce *ClinicalDetailError
		if err := CheckNoClinicalDetail(params, extra); !errors.As(err, &ce) {
			t.Errorf("%q passed the check", params)
		}
	}
	allowed := [][]string{
		{"Thabo", "08:00"},
		{"Martin", "Greenside Clinic", "011 555 0123"}, // "art" inside a name is not a word
		{"Oliver", "Dr Naidoo's rooms"},                // nor is "liver"
		{"Zoë", "20:30"},
	}
	for _, params := range allowed {
		if err := CheckNoClinicalDetail(params, extra); err != nil {
			t.Errorf("%q refused: %v", params, err)
		}
	}
}

func TestEachMedicineInACombinationIsATerm(t *testing.T) {
	terms := MedicineTerms([]string{"Tenofovir/emtricitabine/efavirenz", "Amoxicillin + clavulanic acid",
		"Insulin, biphasic human (30/70)", "Metformin", "metformin"})
	for _, name := range []string{"Efavirenz Rooms", "The Tenofovir Practice", "Clavulanic Acid Clinic", "Metformin House"} {
		if err := CheckNoClinicalDetail([]string{name}, terms); err == nil {
			t.Errorf("%q passed the check", name)
		}
	}
	// The strengths in "(30/70)" are not names: a time or a phone number
	// with a 70 in it goes through.
	for _, p := range []string{"21:30", "011 555 70 70", "Greenside Rooms"} {
		if err := CheckNoClinicalDetail([]string{p}, terms); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
	if len(terms) != 9 { // four distinct names, and five parts
		t.Errorf("terms: %q", terms)
	}
}

func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{PhoneNumberID: "1098765", AccessToken: "EAAG-secret", BaseURL: srv.URL, APIVersion: "v23.0"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSendBuildsATemplateMessageWithButtonPayloads(t *testing.T) {
	var got map[string]any
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = io.WriteString(w, `{"messaging_product":"whatsapp","contacts":[{"input":"27821234567","wa_id":"27821234567"}],"messages":[{"id":"wamid.HBgL"}]}`)
	}))
	defer srv.Close()
	c := testClient(t, srv)
	token, _ := NewReplyToken()
	id, err := c.Send(context.Background(), Message{To: "+27821234567", Template: DoseReminder,
		Params: []string{"Thabo", "08:00"}, ButtonPayloads: DosePayloads(token)})
	if err != nil || id != "wamid.HBgL" {
		t.Fatalf("send: %q, %v", id, err)
	}
	if path != "/v23.0/1098765/messages" || auth != "Bearer EAAG-secret" {
		t.Fatalf("request went to %s with %q", path, auth)
	}
	tpl := got["template"].(map[string]any)
	comps := tpl["components"].([]any)
	if got["to"] != "27821234567" || tpl["name"] != "mes_dose_reminder" || len(comps) != 3 {
		t.Fatalf("body: %v", got)
	}
	second := comps[2].(map[string]any)
	payload := second["parameters"].([]any)[0].(map[string]any)["payload"]
	if second["sub_type"] != "quick_reply" || second["index"] != "1" || payload != token+":skip" {
		t.Fatalf("skip button: %v", second)
	}

	if _, err := c.Send(context.Background(), Message{To: "+27821234567", Template: DoseReminder, Params: []string{"Thabo"}}); !IsPermanent(err) {
		t.Errorf("wrong parameter count should fail permanently: %v", err)
	}
}

func TestSendClassifiesFailures(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		permanent bool
		code      string
	}{
		{400, `{"error":{"message":"(#131026) Message undeliverable","code":131026,"error_data":{"details":"not a WhatsApp user"}}}`, true, "131026"},
		{404, `{"error":{"message":"(#132001) Template name does not exist in the translation","code":132001}}`, true, "132001"},
		{400, `{"error":{"message":"(#130429) Rate limit hit","code":130429}}`, false, "130429"},
		{429, `{}`, false, ""},
		{503, `upstream down`, false, ""},
		{200, `{"messages":[]}`, true, ""},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		_, err := testClient(t, srv).Send(context.Background(), Message{To: "+27821234567", Template: Recall,
			Params: []string{"Thabo", "Greenside Clinic", "011 555 0123"}})
		srv.Close()
		var se *SendError
		if !errors.As(err, &se) || se.Permanent != tc.permanent || se.Code != tc.code {
			t.Errorf("%d %s: %#v", tc.status, tc.body, err)
		}
	}

	// The provider unreachable is temporary.
	srv := httptest.NewServer(http.NotFoundHandler())
	c := testClient(t, srv)
	srv.Close()
	if _, err := c.Send(context.Background(), Message{To: "+27821234567", Template: Recall,
		Params: []string{"Thabo", "Greenside Clinic", "011 555 0123"}}); err == nil || IsPermanent(err) {
		t.Errorf("unreachable provider: %v", err)
	}
	if _, err := New(Config{PhoneNumberID: "1"}); err == nil {
		t.Error("a client without a token was built")
	}
}

func TestWebhookSignature(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account"}`)
	good := Sign("app-secret", body)
	if err := VerifySignature("app-secret", body, good); err != nil {
		t.Fatalf("valid signature refused: %v", err)
	}
	if err := VerifySignature("", body, good); !errors.Is(err, ErrNoAppSecret) {
		t.Errorf("an unset secret must refuse everything: %v", err)
	}
	for _, header := range []string{"", good[7:], "sha256=zz", Sign("other-secret", body), "sha1=" + good[7:]} {
		if err := VerifySignature("app-secret", body, header); !errors.Is(err, ErrBadSignature) {
			t.Errorf("header %q accepted: %v", header, err)
		}
	}
	if err := VerifySignature("app-secret", append(body, ' '), good); err == nil {
		t.Error("a modified body passed")
	}
}

func TestSubscriptionChallenge(t *testing.T) {
	q := url.Values{"hub.mode": {"subscribe"}, "hub.verify_token": {"tok-123"}, "hub.challenge": {"987654"}}
	if c, ok := VerifySubscription(q, "tok-123"); !ok || c != "987654" {
		t.Fatalf("valid challenge refused: %q %v", c, ok)
	}
	if _, ok := VerifySubscription(q, "other"); ok {
		t.Error("wrong verify token accepted")
	}
	if _, ok := VerifySubscription(q, ""); ok {
		t.Error("an unset verify token must refuse")
	}
}

const sampleWebhook = `{
 "object": "whatsapp_business_account",
 "entry": [{"id": "WABA", "changes": [
  {"field": "messages", "value": {
    "messaging_product": "whatsapp",
    "metadata": {"display_phone_number": "27105550000", "phone_number_id": "1098765"},
    "statuses": [
      {"id": "wamid.A", "status": "delivered", "timestamp": "1790000000", "recipient_id": "27821234567"},
      {"id": "wamid.B", "status": "failed", "timestamp": "1790000060", "recipient_id": "27821234567",
       "errors": [{"code": 131026, "title": "Message undeliverable", "error_data": {"details": "Receiver is not on WhatsApp"}}]}
    ],
    "messages": [
      {"from": "27821234567", "id": "wamid.C", "timestamp": "1790000100", "type": "text", "text": {"body": "Taken ✅"}},
      {"from": "27821234567", "id": "wamid.D", "timestamp": "1790000200", "type": "button",
       "button": {"payload": "abcdefghijklmnopqrstuv:skip", "text": "Skip"}, "context": {"id": "wamid.A"}},
      {"from": "27821234567", "id": "wamid.E", "timestamp": "1790000300", "type": "image", "image": {"id": "x"}},
      {"from": "not-a-number", "id": "wamid.F", "timestamp": "1790000300", "type": "text", "text": {"body": "hi"}}
    ]}},
  {"field": "account_update", "value": {}}
 ]}]
}`

func TestParseWebhook(t *testing.T) {
	ev, err := ParseWebhook([]byte(sampleWebhook))
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Statuses) != 2 || len(ev.Messages) != 3 {
		t.Fatalf("parsed %d statuses and %d messages", len(ev.Statuses), len(ev.Messages))
	}
	failed := ev.Statuses[1]
	if failed.Status != "failed" || failed.ErrorCode != "131026" || !strings.Contains(failed.ErrorDetail, "not on WhatsApp") ||
		failed.PhoneNumberID != "1098765" || !failed.At.Equal(time.Unix(1790000060, 0)) {
		t.Errorf("failed status: %+v", failed)
	}
	text, button := ev.Messages[0], ev.Messages[1]
	if text.From != "+27821234567" || text.Text != "Taken ✅" || text.ButtonPayload != "" {
		t.Errorf("text message: %+v", text)
	}
	if button.ButtonPayload != "abcdefghijklmnopqrstuv:skip" || button.ContextID != "wamid.A" {
		t.Errorf("button message: %+v", button)
	}
	if ev.Messages[2].Type != "image" || ev.Messages[2].Text != "" {
		t.Errorf("image message: %+v", ev.Messages[2])
	}
	if _, err := ParseWebhook([]byte("not json")); err == nil {
		t.Error("garbage parsed")
	}
	if ev, _ := ParseWebhook([]byte(`{"object":"page","entry":[]}`)); len(ev.Messages)+len(ev.Statuses) != 0 {
		t.Error("another product's webhook was read")
	}
}

func TestInterpretReplyOnlyReadsWholeAnswers(t *testing.T) {
	cases := map[string]model.ReplyInterpretation{
		"TAKEN": model.ReplyTaken, " taken ": model.ReplyTaken, "Taken ✅": model.ReplyTaken, "Taken!!": model.ReplyTaken,
		"I’ve taken it": model.ReplyTaken, "took it": model.ReplyTaken, "Done": model.ReplyTaken,
		"SKIP": model.ReplySkipped, "skipped": model.ReplySkipped, "Not taken": model.ReplySkipped,
		"STOP": model.ReplyStop, "Unsubscribe": model.ReplyStop,
		// Everything below is kept for staff and changes nothing.
		"I'm not sure": model.ReplyUnrecognised, "not sure": model.ReplyUnrecognised,
		"can I take it later?": model.ReplyUnrecognised, "yes": model.ReplyUnrecognised,
		"no": model.ReplyUnrecognised, "I ran out of pills": model.ReplyUnrecognised,
		"taken yesterday but not today": model.ReplyUnrecognised, "stop taking it?": model.ReplyUnrecognised,
		"": model.ReplyUnrecognised,
	}
	for in, want := range cases {
		if got := InterpretReply(in); got != want {
			t.Errorf("InterpretReply(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestReplyTokensAndPayloads(t *testing.T) {
	a, _ := NewReplyToken()
	b, _ := NewReplyToken()
	if len(a) != 22 || a == b {
		t.Fatalf("tokens %q %q", a, b)
	}
	p := DosePayloads(a)
	if tok, ans, ok := ParsePayload(p[0]); !ok || tok != a || ans != model.ReplyTaken {
		t.Errorf("taken payload: %q %s %v", tok, ans, ok)
	}
	if tok, ans, ok := ParsePayload(p[1]); !ok || tok != a || ans != model.ReplySkipped {
		t.Errorf("skip payload: %q %s %v", tok, ans, ok)
	}
	for _, bad := range []string{"", a, a + ":later", "short:taken", "x:" + a} {
		if _, _, ok := ParsePayload(bad); ok {
			t.Errorf("payload %q accepted", bad)
		}
	}
}
