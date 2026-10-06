//go:build integration

package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/medical-expert-system/internal/notify"
)

// ---------------------------------------------------------------------------
// A stand-in for the WhatsApp Cloud API. The real client talks to it over
// HTTP, so these tests see exactly the JSON that would leave the building.
// ---------------------------------------------------------------------------

const (
	testPhoneID   = "1098765"
	testAppSecret = "test-app-secret"
)

type fakeFailure struct {
	status int
	body   string
}

type fakeGraph struct {
	mu       sync.Mutex
	srv      *httptest.Server
	bodies   []string
	failures map[string][]fakeFailure // by destination, as the API writes it
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newFakeGraph(t *testing.T) *fakeGraph {
	g := &fakeGraph{failures: map[string][]fakeFailure{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var to struct {
			To string `json:"to"`
		}
		_ = json.Unmarshal(raw, &to)
		g.mu.Lock()
		defer g.mu.Unlock()
		if queue := g.failures[to.To]; len(queue) > 0 {
			f := queue[0]
			g.failures[to.To] = queue[1:]
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, f.body)
			return
		}
		g.bodies = append(g.bodies, string(raw))
		// Provider IDs are unique across test runs: the column is unique.
		fmt.Fprintf(w, `{"messages":[{"id":"wamid.%s"}]}`, randomHex(12))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGraph) client(t *testing.T) notify.Sender {
	c, err := notify.New(notify.Config{PhoneNumberID: testPhoneID, AccessToken: "test-token", BaseURL: g.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// failNext makes the next send to a number fail as given.
func (g *fakeGraph) failNext(e164 string, status int, body string) {
	g.mu.Lock()
	to := strings.TrimPrefix(e164, "+")
	g.failures[to] = append(g.failures[to], fakeFailure{status, body})
	g.mu.Unlock()
}

type sentMessage struct {
	To, Template string
	Params       []string
	Payloads     []string
	Raw          string
}

// sentTo returns what was accepted for one number, oldest first. Other
// practices in the shared test database may be sending too.
func (g *fakeGraph) sentTo(e164 string) []sentMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []sentMessage
	for _, raw := range g.bodies {
		var body struct {
			To       string `json:"to"`
			Template struct {
				Name       string `json:"name"`
				Components []struct {
					Type       string `json:"type"`
					Parameters []struct {
						Text    string `json:"text"`
						Payload string `json:"payload"`
					} `json:"parameters"`
				} `json:"components"`
			} `json:"template"`
		}
		_ = json.Unmarshal([]byte(raw), &body)
		if "+"+body.To != e164 {
			continue
		}
		m := sentMessage{To: body.To, Template: body.Template.Name, Raw: raw}
		for _, c := range body.Template.Components {
			for _, p := range c.Parameters {
				if c.Type == "body" {
					m.Params = append(m.Params, p.Text)
				} else {
					m.Payloads = append(m.Payloads, p.Payload)
				}
			}
		}
		out = append(out, m)
	}
	return out
}

func newMessagingHarness(t *testing.T, o harnessOpts) (*harness, *fakeGraph) {
	g := newFakeGraph(t)
	o.sender, o.phoneID, o.appSecret, o.verifyToken = g.client(t), testPhoneID, testAppSecret, "verify-me"
	o.sessionIdle, o.sessionMax = 60*24*time.Hour, 60*24*time.Hour
	return newHarness(t, o), g
}

// enableMessaging switches WhatsApp on for a clinic, and off again when the
// test ends, so leftover practices in the shared database stay quiet.
func (h *harness) enableMessaging(c clinic) {
	h.t.Helper()
	h.expect(h.do("PATCH", "/api/v1/practice", c.adminToken,
		map[string]any{"whatsapp_enabled": true, "phone": "011 555 0123"}), 200, "enable messaging")
	h.t.Cleanup(func() {
		h.do("PATCH", "/api/v1/practice", c.adminToken, map[string]any{"whatsapp_enabled": false})
	})
}

// randomMobile is unique enough that a reply matched by number finds this
// test's message, not one from an earlier run.
func randomMobile() (typed, e164 string) {
	n, _ := rand.Int(rand.Reader, big.NewInt(9_000_000))
	digits := fmt.Sprintf("%07d", n.Int64()+1_000_000)
	return "082 " + digits[:3] + " " + digits[3:], "+2782" + digits
}

// whatsappPatient registers a chronic patient on WhatsApp who has consented
// to reminders.
func (h *harness) whatsappPatient(c clinic, first, last string) (id, e164 string) {
	h.t.Helper()
	typed, e164 := randomMobile()
	body := patientBody(first, last, "1970-03-15")
	body["mobile"], body["preferred_channel"] = typed, "whatsapp"
	id = h.expect(h.createPatient(c.adminToken, body), 201, "patient").str("id")
	h.expect(h.do("POST", "/api/v1/patients/"+id+"/conditions", c.doctor,
		map[string]any{"description": "Essential hypertension", "icd10_code": "I10", "is_chronic": true}), 201, "condition")
	h.setReminderConsent(c, id, true)
	return id, e164
}

func (h *harness) setReminderConsent(c clinic, patientID string, granted bool) {
	h.t.Helper()
	h.expect(h.do("POST", "/api/v1/patients/"+patientID+"/consents", c.nurse,
		map[string]any{"purpose": "reminder_communications", "granted": granted, "method": "verbal"}), 201, "consent")
	h.clock.Advance(time.Millisecond)
}

type doseView struct {
	ID          string
	ScheduledAt time.Time
	Status      string
	Source      string
	Channel     string
	RegimenID   string
}

// doses lists a patient's doses in [from, to), oldest first.
func (h *harness) doses(token, patientID string, from, to time.Time) []doseView {
	h.t.Helper()
	r := h.expect(h.do("GET", fmt.Sprintf("/api/v1/patients/%s/doses?from=%s&to=%s", patientID,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)), token, nil), 200, "doses")
	var out []doseView
	for _, x := range r.body["items"].([]any) {
		m := x.(map[string]any)
		at, _ := time.Parse(time.RFC3339Nano, m["scheduled_at"].(string))
		ch, _ := m["response_channel"].(string)
		out = append(out, doseView{ID: m["id"].(string), ScheduledAt: at, Status: m["status"].(string),
			Source: m["source"].(string), Channel: ch, RegimenID: m["regimen_id"].(string)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScheduledAt.Before(out[j].ScheduledAt) })
	return out
}

func (h *harness) futureDoses(token, patientID string) []doseView {
	now := h.clock.Now()
	return h.doses(token, patientID, now, now.Add(8*24*time.Hour))
}

// regimenWith starts a regimen of a given frequency.
func (h *harness) regimenWith(c clinic, patientID, medication, frequency string) string {
	h.t.Helper()
	return h.expect(h.do("POST", "/api/v1/patients/"+patientID+"/regimens", c.doctor, map[string]any{
		"medication_id": h.medication(c.doctor, medication), "dose": "1 tablet", "frequency": frequency,
	}), 201, "regimen").str("id")
}

func (h *harness) dispatch() {
	h.t.Helper()
	if _, err := h.reg.DispatchReminders(context.Background()); err != nil {
		h.t.Fatalf("dispatch: %v", err)
	}
}

// --- webhooks ---------------------------------------------------------------

func (h *harness) webhook(body, secret string) resp {
	h.t.Helper()
	req, err := http.NewRequest("POST", h.srv.URL+"/api/v1/webhooks/whatsapp", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Hub-Signature-256", notify.Sign(secret, []byte(body)))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, header: res.Header, body: map[string]any{}}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func envelope(value string) string {
	return `{"object":"whatsapp_business_account","entry":[{"id":"WABA","changes":[{"field":"messages","value":{` +
		`"messaging_product":"whatsapp","metadata":{"phone_number_id":"` + testPhoneID + `"},` + value + `}}]}]}`
}

func unix(t time.Time) string { return fmt.Sprint(t.Unix()) }

func buttonReply(from, payload string, at time.Time) (body, id string) {
	id = "wamid.in" + randomHex(10)
	return envelope(`"messages":[{"from":"` + strings.TrimPrefix(from, "+") + `","id":"` + id + `","timestamp":"` + unix(at) +
		`","type":"button","button":{"payload":"` + payload + `","text":"Taken"}}]`), id
}

func textReply(from, text, contextID string, at time.Time) string {
	ctx := ""
	if contextID != "" {
		ctx = `,"context":{"id":"` + contextID + `"}`
	}
	b, _ := json.Marshal(text)
	return envelope(`"messages":[{"from":"` + strings.TrimPrefix(from, "+") + `","id":"wamid.in` + randomHex(10) +
		`","timestamp":"` + unix(at) + `","type":"text","text":{"body":` + string(b) + `}` + ctx + `}]`)
}

func receipt(providerID, status string, at time.Time) string {
	extra := ""
	if status == "failed" {
		extra = `,"errors":[{"code":131026,"title":"Message undeliverable"}]`
	}
	return envelope(`"statuses":[{"id":"` + providerID + `","status":"` + status + `","timestamp":"` + unix(at) +
		`","recipient_id":"27820000000"` + extra + `}]`)
}

type messageView struct {
	ID, Purpose, Status, Preview, ProviderID, ErrorCode string
	DoseIDs                                             []string
}

type replyView struct {
	Interpretation, Body string
	DosesUpdated         int
}

func (h *harness) messages(token, patientID string) ([]messageView, []replyView) {
	h.t.Helper()
	r := h.expect(h.do("GET", "/api/v1/patients/"+patientID+"/notifications", token, nil), 200, "messages")
	var ms []messageView
	for _, x := range r.body["notifications"].([]any) {
		m := x.(map[string]any)
		v := messageView{ID: m["id"].(string), Purpose: m["purpose"].(string), Status: m["status"].(string),
			Preview: m["preview"].(string)}
		v.ProviderID, _ = m["provider_message_id"].(string)
		v.ErrorCode, _ = m["error_code"].(string)
		if ids, ok := m["dose_ids"].([]any); ok {
			for _, d := range ids {
				v.DoseIDs = append(v.DoseIDs, d.(string))
			}
		}
		ms = append(ms, v)
	}
	var rs []replyView
	for _, x := range r.body["replies"].([]any) {
		m := x.(map[string]any)
		v := replyView{Interpretation: m["interpretation"].(string), DosesUpdated: int(m["doses_updated"].(float64))}
		v.Body, _ = m["body"].(string)
		rs = append(rs, v)
	}
	return ms, rs
}

// ---------------------------------------------------------------------------

func TestSchedulerOnlySchedulesPatientsWhoCanAnswer(t *testing.T) {
	h, _ := newMessagingHarness(t, harnessOpts{})
	c := h.newClinic()
	h.enableMessaging(c)

	// On WhatsApp with consent: a week of doses, at 08:00 and 20:00.
	a, _ := h.whatsappPatient(c, "Anele", "Zulu")
	reg := h.regimenWith(c, a, "Metformin", "twice_daily")
	doses := h.futureDoses(c.nurse, a)
	if len(doses) < 13 || len(doses) > 15 {
		t.Fatalf("expected about 14 doses in a week, got %d", len(doses))
	}
	sast := time.FixedZone("SAST", 2*3600)
	for _, d := range doses {
		if hm := d.ScheduledAt.In(sast).Format("15:04"); (hm != "08:00" && hm != "20:00") || d.Source != "schedule" || d.Status != "pending" {
			t.Fatalf("unexpected dose %+v at %s", d, hm)
		}
	}

	// Without consent to reminders there is nobody to ask, so nothing is
	// scheduled: doses nobody can answer would all end up "missed".
	b, _ := h.whatsappPatient(c, "Bongani", "Dube")
	h.setReminderConsent(c, b, false)
	regB := h.regimenWith(c, b, "Amlodipine", "once_daily")
	if n := len(h.futureDoses(c.nurse, b)); n != 0 {
		t.Fatalf("scheduled %d doses for a patient who cannot be asked", n)
	}
	h.setReminderConsent(c, b, true)
	if n := len(h.futureDoses(c.nurse, b)); n < 6 || n > 8 {
		t.Fatalf("consent should start the schedule at once, got %d doses", n)
	}

	// Waking hours move the doses.
	h.expect(h.do("PUT", "/api/v1/patients/"+b, c.adminToken, map[string]any{
		"first_name": "Bongani", "last_name": "Dube", "date_of_birth": "1970-03-15", "sex": "female",
		"mobile": h.patientMobile(c, b), "preferred_channel": "whatsapp", "waking_start": "10:00", "waking_end": "23:00",
	}), 200, "waking hours")
	for _, d := range h.futureDoses(c.nurse, b) {
		if hm := d.ScheduledAt.In(sast).Format("15:04"); hm != "11:00" {
			t.Fatalf("dose not moved to an hour after waking: %s", hm)
		}
	}

	// A patient with their own login can answer in the portal, WhatsApp or not.
	p := h.expect(h.createPatient(c.adminToken, patientBody("Carla", "Botha", "1958-11-02")), 201, "portal patient").str("id")
	h.regimenWith(c, p, "Atorvastatin", "once_daily")
	if n := len(h.futureDoses(c.nurse, p)); n != 0 {
		t.Fatalf("scheduled %d doses before the patient had any way to answer", n)
	}
	h.expect(h.do("POST", "/api/v1/users", c.adminToken, map[string]any{"email": "carla-" + randomHex(3) + "@" + c.slug + ".test",
		"full_name": "Carla Botha", "role": "patient", "patient_id": p, "password": adminPassword}), 201, "portal login")
	if n := len(h.futureDoses(c.nurse, p)); n < 6 {
		t.Fatalf("a portal login should start the schedule, got %d doses", n)
	}

	// Pausing withdraws future doses, including one added by hand; resuming
	// brings the scheduled ones back.
	extra := h.clock.Now().Add(30 * time.Hour).Truncate(time.Minute)
	h.expect(h.do("POST", "/api/v1/regimens/"+reg+"/doses", c.nurse, map[string]any{"times": []string{extra.Format(time.RFC3339)}}), 200, "manual dose")
	h.expect(h.do("POST", "/api/v1/regimens/"+reg+"/status", c.doctor, map[string]any{"status": "paused", "reason": "Gastro upset"}), 200, "pause")
	if n := len(h.futureDoses(c.nurse, a)); n != 0 {
		t.Fatalf("%d doses left after pausing", n)
	}
	h.expect(h.do("POST", "/api/v1/regimens/"+reg+"/status", c.doctor, map[string]any{"status": "active", "reason": "Settled"}), 200, "resume")
	if n := len(h.futureDoses(c.nurse, a)); n < 13 {
		t.Fatalf("resuming should reschedule, got %d doses", n)
	}

	// Switching the practice's messaging off leaves WhatsApp-only patients
	// with nobody to ask; the portal patient keeps their schedule.
	h.expect(h.do("PATCH", "/api/v1/practice", c.adminToken, map[string]any{"whatsapp_enabled": false}), 200, "messaging off")
	if n := len(h.futureDoses(c.nurse, a)) + len(h.futureDoses(c.nurse, b)); n != 0 {
		t.Fatalf("%d doses left for patients nobody can ask", n)
	}
	if n := len(h.futureDoses(c.nurse, p)); n < 6 {
		t.Fatalf("the portal patient lost their schedule: %d", n)
	}

	// The hourly run is idempotent.
	before := len(h.futureDoses(c.nurse, p))
	if _, err := h.reg.GenerateDoses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := len(h.futureDoses(c.nurse, p)); after != before {
		t.Fatalf("a second run changed the schedule: %d -> %d", before, after)
	}
	_ = regB
}

func (h *harness) patientMobile(c clinic, id string) string {
	return h.expect(h.do("GET", "/api/v1/patients/"+id, c.adminToken, nil), 200, "patient").str("mobile")
}

// dueNow moves the clock to a patient's first pending dose and returns it.
func (h *harness) dueNow(token, patientID string) doseView {
	h.t.Helper()
	doses := h.futureDoses(token, patientID)
	if len(doses) == 0 {
		h.t.Fatal("no doses scheduled")
	}
	h.clock.Advance(doses[0].ScheduledAt.Sub(h.clock.Now()) + time.Second)
	return doses[0]
}

func TestRemindersRepliesAndReceipts(t *testing.T) {
	h, g := newMessagingHarness(t, harnessOpts{followUp: time.Hour})
	c := h.newClinic()
	h.enableMessaging(c)
	pid, mobile := h.whatsappPatient(c, "Thandeka", "Mokoena")
	h.regimenWith(c, pid, "Metformin", "twice_daily")
	h.regimenWith(c, pid, "Amlodipine", "twice_daily")

	first := h.dueNow(c.nurse, pid)
	h.dispatch()
	h.dispatch() // a second pass must not remind again
	sent := g.sentTo(mobile)
	if len(sent) != 1 {
		t.Fatalf("expected one reminder for both medicines, got %d", len(sent))
	}
	m := sent[0]
	sast := time.FixedZone("SAST", 2*3600)
	if m.Template != "mes_dose_reminder" || len(m.Params) != 2 || m.Params[0] != "Thandeka" ||
		m.Params[1] != first.ScheduledAt.In(sast).Format("15:04") || len(m.Payloads) != 2 {
		t.Fatalf("reminder: %+v", m)
	}
	// No clinical detail leaves the building.
	for _, word := range []string{"Metformin", "Amlodipine", "metformin", "hypertension", "tablet", "Mokoena"} {
		if strings.Contains(m.Raw, word) {
			t.Fatalf("the message sent %q: %s", word, m.Raw)
		}
	}

	msgs, _ := h.messages(c.nurse, pid)
	if len(msgs) != 1 || msgs[0].Status != "sent" || len(msgs[0].DoseIDs) != 2 || msgs[0].ProviderID == "" {
		t.Fatalf("notification record: %+v", msgs)
	}

	// Pressing "Taken" answers both doses, through WhatsApp.
	body, _ := buttonReply(mobile, m.Payloads[0], h.clock.Now())
	h.expect(h.webhook(body, testAppSecret), 200, "button reply")
	h.expect(h.webhook(body, testAppSecret), 200, "the same webhook again")
	now := h.clock.Now()
	var taken int
	for _, d := range h.doses(c.nurse, pid, first.ScheduledAt.Add(-time.Minute), first.ScheduledAt.Add(time.Minute)) {
		if d.Status == "taken" && d.Channel == "whatsapp" {
			taken++
		}
	}
	if taken != 2 {
		t.Fatalf("expected both doses taken by WhatsApp, got %d", taken)
	}
	_, replies := h.messages(c.nurse, pid)
	if len(replies) != 1 || replies[0].Interpretation != "taken" || replies[0].DosesUpdated != 2 {
		t.Fatalf("a webhook delivered twice must be applied once: %+v", replies)
	}

	// Receipts only move forward.
	pv := msgs[0].ProviderID
	for _, st := range []string{"read", "delivered", "failed"} {
		h.expect(h.webhook(receipt(pv, st, now), testAppSecret), 200, "receipt "+st)
	}
	msgs, _ = h.messages(c.nurse, pid)
	if msgs[0].Status != "read" || msgs[0].ErrorCode != "" {
		t.Fatalf("a late receipt moved the message backwards: %+v", msgs[0])
	}

	// The next reminder; an ambiguous reply changes nothing and is kept.
	second := h.dueNow(c.nurse, pid)
	h.dispatch()
	sent = g.sentTo(mobile)
	if len(sent) != 2 {
		t.Fatalf("expected a second reminder, got %d messages", len(sent))
	}
	h.expect(h.webhook(textReply(mobile, "I'm not sure, can I take it later?", "", h.clock.Now()), testAppSecret), 200, "unsure")
	for _, d := range h.doses(c.nurse, pid, second.ScheduledAt.Add(-time.Minute), second.ScheduledAt.Add(time.Minute)) {
		if d.Status != "pending" {
			t.Fatalf("an ambiguous reply answered a dose: %+v", d)
		}
	}
	_, replies = h.messages(c.nurse, pid)
	if replies[0].Interpretation != "unrecognised" || replies[0].Body != "I'm not sure, can I take it later?" {
		t.Fatalf("the unreadable reply should be kept for staff: %+v", replies[0])
	}

	// An hour later, still unanswered: one follow-up, then no more.
	h.clock.Advance(time.Hour)
	h.dispatch()
	h.dispatch()
	sent = g.sentTo(mobile)
	if len(sent) != 3 || sent[2].Template != "mes_dose_follow_up" {
		t.Fatalf("expected one follow-up, got %d messages", len(sent))
	}

	// Typed "SKIP" with no button and no reply context answers the latest
	// message: the follow-up, which covers the same two doses.
	h.expect(h.webhook(textReply(mobile, "SKIP", "", h.clock.Now()), testAppSecret), 200, "typed skip")
	for _, d := range h.doses(c.nurse, pid, second.ScheduledAt.Add(-time.Minute), second.ScheduledAt.Add(time.Minute)) {
		if d.Status != "skipped" || d.Channel != "whatsapp" {
			t.Fatalf("typed SKIP not applied: %+v", d)
		}
	}

	// STOP withdraws consent, by the patient, and future doses go.
	h.expect(h.webhook(textReply(mobile, "STOP", "", h.clock.Now()), testAppSecret), 200, "stop")
	r := h.expect(h.do("GET", "/api/v1/patients/"+pid+"/consents", c.nurse, nil), 200, "consents")
	for _, x := range r.body["current"].([]any) {
		st := x.(map[string]any)
		if st["purpose"] == "reminder_communications" && (st["granted"] != false || st["recorded_by"] != nil) {
			t.Fatalf("STOP not recorded as the patient's own withdrawal: %v", st)
		}
	}
	if n := len(h.futureDoses(c.nurse, pid)); n != 0 {
		t.Fatalf("%d doses still scheduled after STOP", n)
	}
}

func TestSendingRetriesTemporaryFailuresOnly(t *testing.T) {
	h, g := newMessagingHarness(t, harnessOpts{followUp: time.Hour})
	c := h.newClinic()
	h.enableMessaging(c)
	pid, mobile := h.whatsappPatient(c, "Sipho", "Ndlovu")
	h.regimenWith(c, pid, "Metformin", "once_daily")

	// The provider is down: the reminder is kept and retried a minute later.
	h.dueNow(c.nurse, pid)
	g.failNext(mobile, 503, `{"error":{"message":"Service temporarily unavailable","code":131016}}`)
	h.dispatch()
	msgs, _ := h.messages(c.nurse, pid)
	if len(msgs) != 1 || msgs[0].Status != "queued" {
		t.Fatalf("a temporary failure should leave the message queued: %+v", msgs)
	}
	h.dispatch() // too soon: leased until the retry is due
	if len(g.sentTo(mobile)) != 0 {
		t.Fatal("retried before the back-off")
	}
	h.clock.Advance(2*time.Minute + time.Second)
	h.dispatch()
	msgs, _ = h.messages(c.nurse, pid)
	if len(g.sentTo(mobile)) != 1 || msgs[0].Status != "sent" {
		t.Fatalf("retry did not send: %+v", msgs)
	}

	// A permanent failure is recorded and not retried, and no follow-up is
	// sent to a number that cannot receive messages.
	q, qMobile := h.whatsappPatient(c, "Lerato", "Molefe")
	h.regimenWith(c, q, "Amlodipine", "once_daily")
	h.dueNow(c.nurse, q)
	g.failNext(qMobile, 400, `{"error":{"message":"(#131026) Message undeliverable","code":131026}}`)
	h.dispatch()
	h.clock.Advance(90 * time.Minute)
	h.dispatch()
	msgs, _ = h.messages(c.nurse, q)
	if len(msgs) != 1 || msgs[0].Status != "failed" || msgs[0].ErrorCode != "131026" || len(g.sentTo(qMobile)) != 0 {
		t.Fatalf("permanent failure handling: %+v, %d sent", msgs, len(g.sentTo(qMobile)))
	}

	health := h.expect(h.do("GET", "/api/v1/notifications/health", c.adminToken, nil), 200, "health")
	day := health.body["last_24_hours"].(map[string]any)
	if day["failed"].(float64) < 1 || health.body["last_error_code"] != "131026" || health.body["active"] != true {
		t.Fatalf("health: %v", health.body)
	}
	if strings.Contains(fmt.Sprint(health.body), "Molefe") || strings.Contains(fmt.Sprint(health.body), qMobile) {
		t.Fatal("the health report identifies a patient")
	}
}

func TestLateDosesAreNotRemindedAndTheDailyLimitHolds(t *testing.T) {
	h, g := newMessagingHarness(t, harnessOpts{dailyLimit: 1, followUp: -1})
	c := h.newClinic()
	h.enableMessaging(c)
	a, aMobile := h.whatsappPatient(c, "Naledi", "Khumalo")
	h.regimenWith(c, a, "Metformin", "twice_daily")

	// Two hours after the dose, a reminder would only confuse.
	h.dueNow(c.nurse, a)
	h.clock.Advance(2 * time.Hour)
	h.dispatch()
	if len(g.sentTo(aMobile)) != 0 {
		t.Fatal("a reminder went out two hours late")
	}

	// Three patients due at the same moment, and a daily limit of one
	// message: exactly one goes out.
	b, bMobile := h.whatsappPatient(c, "Karabo", "Sithole")
	h.regimenWith(c, b, "Atorvastatin", "once_daily")
	d, dMobile := h.whatsappPatient(c, "Palesa", "Nkosi")
	h.regimenWith(c, d, "Amlodipine", "once_daily")
	h.dueNow(c.nurse, b)
	h.dispatch()
	total := len(g.sentTo(aMobile)) + len(g.sentTo(bMobile)) + len(g.sentTo(dMobile))
	if total != 1 {
		t.Fatalf("daily limit of one, but %d messages went out", total)
	}
}

func TestWebhookIsRefusedWithoutAValidSignature(t *testing.T) {
	h, _ := newMessagingHarness(t, harnessOpts{})
	body := receipt("wamid.nothing", "delivered", time.Now())
	if r := h.webhook(body, ""); r.status != 401 || r.errCode() != "bad_signature" {
		t.Fatalf("unsigned webhook: %d %v", r.status, r.body)
	}
	if r := h.webhook(body, "some-other-secret"); r.status != 401 {
		t.Fatalf("wrongly signed webhook: %d", r.status)
	}
	h.expect(h.webhook(body, testAppSecret), 200, "signed webhook")

	// The subscription check echoes the challenge only for the right token.
	r := h.do("GET", "/api/v1/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=12345", "", nil)
	if r.status != 403 {
		t.Fatalf("wrong verify token: %d", r.status)
	}
	res, err := http.Get(h.srv.URL + "/api/v1/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=verify-me&hub.challenge=12345")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(raw) != "12345" {
		t.Fatalf("challenge: %d %q", res.StatusCode, raw)
	}

	// With no app secret configured, every webhook is refused.
	bare := newHarness(t, harnessOpts{})
	if r := bare.webhook(body, testAppSecret); r.status != 503 || r.errCode() != "webhook_not_configured" {
		t.Fatalf("webhook without an app secret: %d %v", r.status, r.body)
	}
}

func TestManualReminderAndRecallRules(t *testing.T) {
	h, g := newMessagingHarness(t, harnessOpts{})
	c := h.newClinic()
	pid, mobile := h.whatsappPatient(c, "Zanele", "Mthembu")
	h.regimenWith(c, pid, "Metformin", "once_daily")

	if r := h.do("POST", "/api/v1/patients/"+pid+"/notifications/recall", c.adminToken, nil); r.errCode() != "messaging_off" {
		t.Fatalf("messaging off: %v", r.body)
	}
	// Enabling messaging on a practice whose name reveals what it treats is
	// refused until a neutral messaging name is set.
	r := h.do("PATCH", "/api/v1/practice", c.adminToken, map[string]any{"name": "Greenside HIV Clinic", "whatsapp_enabled": true})
	if r.errCode() != "clinical_detail" || !strings.Contains(errField(r), "messaging_name") {
		t.Fatalf("revealing name accepted: %v", r.body)
	}
	// So is one naming a medicine in the catalogue, including one of the
	// medicines in a combination product.
	for _, name := range []string{"Metformin Rooms", "Efavirenz House"} {
		r := h.do("PATCH", "/api/v1/practice", c.adminToken, map[string]any{"messaging_name": name, "whatsapp_enabled": true})
		if r.errCode() != "clinical_detail" {
			t.Fatalf("messaging name %q accepted: %v", name, r.body)
		}
	}
	h.expect(h.do("PATCH", "/api/v1/practice", c.adminToken, map[string]any{"name": "Greenside HIV Clinic",
		"messaging_name": "Greenside Rooms", "whatsapp_enabled": true}), 200, "neutral messaging name")
	h.t.Cleanup(func() { h.do("PATCH", "/api/v1/practice", c.adminToken, map[string]any{"whatsapp_enabled": false}) })

	if r := h.do("POST", "/api/v1/patients/"+pid+"/notifications/recall", c.adminToken, nil); r.errCode() != "practice_phone_missing" {
		t.Fatalf("recall without a practice phone: %v", r.body)
	}
	h.expect(h.do("PATCH", "/api/v1/practice", c.adminToken, map[string]any{"phone": "011 555 0123"}), 200, "phone")
	rec := h.expect(h.do("POST", "/api/v1/patients/"+pid+"/notifications/recall", c.adminToken, nil), 201, "recall")
	preview := rec.str("preview")
	if !strings.Contains(preview, "Greenside Rooms") || strings.Contains(preview, "HIV") || !strings.Contains(preview, "011 555 0123") {
		t.Fatalf("recall preview: %q", preview)
	}
	if r := h.do("POST", "/api/v1/patients/"+pid+"/notifications/recall", c.adminToken, nil); r.errCode() != "recently_recalled" {
		t.Fatalf("second recall in a week: %v", r.body)
	}
	if len(g.sentTo(mobile)) != 1 || g.sentTo(mobile)[0].Template != "mes_recall" {
		t.Fatal("recall not sent")
	}

	// Nothing due yet.
	if r := h.do("POST", "/api/v1/patients/"+pid+"/notifications/reminder", c.nurse, nil); r.errCode() != "nothing_due" {
		t.Fatalf("reminder with nothing due: %v", r.body)
	}
	// Due: a reminder by hand, and then the dispatcher does not send another.
	h.dueNow(c.nurse, pid)
	h.expect(h.do("POST", "/api/v1/patients/"+pid+"/notifications/reminder", c.nurse, nil), 201, "manual reminder")
	h.dispatch()
	if n := len(g.sentTo(mobile)); n != 2 {
		t.Fatalf("expected the recall and one reminder, got %d messages", n)
	}
	if r := h.do("POST", "/api/v1/patients/"+pid+"/notifications/reminder", c.adminToken, nil); r.status != 403 {
		t.Fatalf("an administrator sent a dose reminder: %d", r.status)
	}

	// Consent and channel are checked.
	h.setReminderConsent(c, pid, false)
	if r := h.do("POST", "/api/v1/patients/"+pid+"/notifications/reminder", c.nurse, nil); r.errCode() != "comms_consent_required" {
		t.Fatalf("no consent: %v", r.body)
	}
	sms := patientBody("Ruth", "Pillay", "1961-07-07")
	typed, _ := randomMobile()
	sms["mobile"], sms["preferred_channel"] = typed, "sms"
	smsID := h.expect(h.createPatient(c.adminToken, sms), 201, "sms patient").str("id")
	h.setReminderConsent(c, smsID, true)
	r = h.do("POST", "/api/v1/patients/"+smsID+"/notifications/recall", c.adminToken, nil)
	if r.errCode() != "no_whatsapp" || !strings.Contains(r.body["error"].(map[string]any)["message"].(string), "sms") {
		t.Fatalf("SMS patient: %v", r.body)
	}

	// A server with no WhatsApp account says so.
	bare := newHarness(t, harnessOpts{})
	bc := bare.newClinic()
	bp := bare.expect(bare.createPatient(bc.adminToken, patientBody("Mpho", "Radebe", "1980-02-02")), 201, "patient").str("id")
	if r := bare.do("POST", "/api/v1/patients/"+bp+"/notifications/recall", bc.adminToken, nil); r.status != 503 ||
		r.errCode() != "messaging_not_configured" {
		t.Fatalf("unconfigured server: %d %v", r.status, r.body)
	}
}

func TestDemoPracticesNeverSend(t *testing.T) {
	h, _ := newMessagingHarness(t, harnessOpts{})
	c := h.newClinic()
	if _, err := h.db.Exec(`UPDATE tenants SET settings_json = '{"demo": true}' WHERE slug = ?`, c.slug); err != nil {
		t.Fatal(err)
	}
	if r := h.do("PATCH", "/api/v1/practice", c.adminToken, map[string]any{"whatsapp_enabled": true}); r.errCode() != "demo_practice" {
		t.Fatalf("a demo practice switched messaging on: %v", r.body)
	}
	v := h.expect(h.do("GET", "/api/v1/practice", c.nurse, nil), 200, "practice")
	if v.body["messaging"].(map[string]any)["active"] != false {
		t.Fatal("a demo practice reports messaging as active")
	}
}

// ---------------------------------------------------------------------------
// Dashboard and recall lists, over a history built with a moving clock.
// ---------------------------------------------------------------------------

func (h *harness) setClock(t time.Time) { h.clock.Advance(t.Sub(h.clock.Now())) }

type history struct {
	c                clinic
	x, y, z, w       string // patients: see buildHistory
	xScript, yScript string
	xResult          string
	names            []string
	sast             *time.Location
	now              time.Time
}

// buildHistory sets up four patients:
//   - x: diabetes and hypertension; takes every dose; only a draft note, so
//     never reviewed; a script expiring in ten days; an abnormal, critical
//     result recorded two days ago and not yet acknowledged.
//   - y: hypertension; skips every dose, but answers; seen today in a
//     signed note; a script with no repeats.
//   - z: diabetes; never answers; last signed note 220 days ago.
//   - w: an acute condition only, never seen.
func buildHistory(t *testing.T, h *harness) *history {
	hs := &history{sast: time.FixedZone("SAST", 2*3600), now: h.clock.Now()}
	h.setClock(hs.now.Add(-220 * 24 * time.Hour))
	c := h.newClinic()
	hs.c = c
	mk := func(first, last string, conditions ...[2]string) string {
		id := h.expect(h.createPatient(c.adminToken, patientBody(first, last, "1965-05-05")), 201, "patient").str("id")
		for _, cond := range conditions {
			h.expect(h.do("POST", "/api/v1/patients/"+id+"/conditions", c.doctor,
				map[string]any{"description": cond[1], "icd10_code": cond[0], "is_chronic": cond[0] != "J06.9"}), 201, "condition")
		}
		hs.names = append(hs.names, first, last)
		return id
	}
	diabetes, hypertension := [2]string{"E11.9", "Type 2 diabetes mellitus"}, [2]string{"I10", "Essential hypertension"}
	hs.x = mk("Xolani", "Mahlangu", diabetes, hypertension)
	hs.y = mk("Yusuf", "Patel", hypertension)
	hs.z = mk("Zinhle", "Ngcobo", diabetes)
	hs.w = mk("Wynand", "Venter", [2]string{"J06.9", "Acute upper respiratory infection"})
	h.signedEncounter(c, hs.z) // 220 days ago

	// Twenty days ago: medicines and ten daily doses, added by hand.
	h.setClock(hs.now.Add(-20 * 24 * time.Hour))
	dayOne := time.Date(h.clock.Now().In(hs.sast).Year(), h.clock.Now().In(hs.sast).Month(), h.clock.Now().In(hs.sast).Day()+1, 8, 0, 0, 0, hs.sast)
	var times []string
	for i := 0; i < 10; i++ {
		times = append(times, dayOne.AddDate(0, 0, i).Format(time.RFC3339))
	}
	for _, p := range []string{hs.x, hs.y, hs.z} {
		reg := h.regimenWith(c, p, "Metformin", "once_daily")
		h.expect(h.do("POST", "/api/v1/regimens/"+reg+"/doses", c.nurse, map[string]any{"times": times}), 200, "doses")
	}

	// Two days ago: x's result.
	h.setClock(hs.now.Add(-48 * time.Hour))
	hs.xResult = h.expect(h.do("POST", "/api/v1/patients/"+hs.x+"/lab-results", c.nurse, map[string]any{
		"lab_name": "City Pathology", "panel": "Urea and electrolytes",
		"items": []map[string]any{{"analyte": "Potassium", "value": "6.4", "unit": "mmol/L", "abnormal_flag": "critical_high"}},
	}), 201, "result").str("id")

	// Now: answers, a draft for x, a signed note for y, scripts.
	h.setClock(hs.now)
	for _, p := range []struct {
		id, status string
	}{{hs.x, "taken"}, {hs.y, "skipped"}} {
		for _, d := range h.doses(c.nurse, p.id, hs.now.AddDate(0, 0, -30), hs.now) {
			h.expect(h.do("POST", "/api/v1/doses/"+d.ID+"/response", c.nurse, map[string]any{"status": p.status}), 200, "answer")
		}
	}
	h.expect(h.do("POST", "/api/v1/maintenance/close-overdue-doses", c.adminToken, nil), 200, "close overdue")
	h.expect(h.do("POST", "/api/v1/patients/"+hs.x+"/encounters", c.doctor, map[string]any{"presenting_complaint": "Draft only"}), 201, "draft")
	h.signedEncounter(c, hs.y)
	xUntil := h.clock.Now().In(hs.sast).AddDate(0, 0, 10).Format("2006-01-02")
	regs := h.expect(h.do("GET", "/api/v1/patients/"+hs.x+"/regimens", c.doctor, nil), 200, "regimens").body["items"].([]any)
	hs.xScript = h.expect(h.do("POST", "/api/v1/patients/"+hs.x+"/prescriptions", c.doctor, map[string]any{
		"items":              []map[string]any{{"regimen_id": regs[0].(map[string]any)["id"], "quantity": "30 tablets"}},
		"repeats_authorised": 2, "valid_until": xUntil, "override_reason": "Seen at the clinic's outreach day",
	}), 201, "x script").body["prescription"].(map[string]any)["id"].(string)
	yRegs := h.expect(h.do("GET", "/api/v1/patients/"+hs.y+"/regimens", c.doctor, nil), 200, "regimens").body["items"].([]any)
	hs.yScript = h.expect(h.do("POST", "/api/v1/patients/"+hs.y+"/prescriptions", c.doctor, map[string]any{
		"items":              []map[string]any{{"regimen_id": yRegs[0].(map[string]any)["id"], "quantity": "30 tablets"}},
		"repeats_authorised": 0,
	}), 201, "y script").body["prescription"].(map[string]any)["id"].(string)
	return hs
}

func TestDashboardCountsWithoutIdentifiers(t *testing.T) {
	h := newHarness(t, harnessOpts{sessionIdle: 400 * 24 * time.Hour, sessionMax: 400 * 24 * time.Hour})
	hs := buildHistory(t, h)
	c := hs.c

	// A dose two hours ago nobody has answered yet is pending, not missed.
	regs := h.expect(h.do("GET", "/api/v1/patients/"+hs.x+"/regimens", c.doctor, nil), 200, "regimens").body["items"].([]any)
	twoHoursAgo := h.clock.Now().Add(-2 * time.Hour).Truncate(time.Minute)
	h.expect(h.do("POST", "/api/v1/regimens/"+regs[0].(map[string]any)["id"].(string)+"/doses", c.nurse,
		map[string]any{"times": []string{twoHoursAgo.Format(time.RFC3339)}}), 200, "recent dose")

	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/dashboard?days=30", nil)
	req.Header.Set("Authorization", "Bearer "+c.adminToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("dashboard: %d %s", res.StatusCode, raw)
	}
	for _, s := range append(hs.names, hs.x, hs.y, hs.z, hs.w) {
		if strings.Contains(string(raw), s) {
			t.Fatalf("the dashboard identifies a patient (%q)", s)
		}
	}
	var d struct {
		Patients          map[string]float64 `json:"patients"`
		ChronicConditions []struct {
			ICD10Code string  `json:"icd10_code"`
			Patients  float64 `json:"patients"`
		} `json:"chronic_conditions"`
		Adherence struct {
			Taken, Skipped, Missed, Due float64
			PendingExcluded             float64            `json:"pending_excluded"`
			ResponseRate                *float64           `json:"response_rate"`
			AdherenceRate               *float64           `json:"adherence_rate"`
			Buckets                     map[string]float64 `json:"patients_by_response"`
		} `json:"adherence"`
		Results       map[string]float64 `json:"results"`
		Consultations map[string]float64 `json:"consultations"`
		Prescriptions map[string]float64 `json:"prescriptions"`
		Notes         []string           `json:"notes"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.Patients["active"] != 4 || d.Patients["chronic"] != 3 || d.Patients["seen_today"] != 1 {
		t.Fatalf("patients: %v", d.Patients)
	}
	rows := map[string]float64{}
	var sum float64
	for _, r := range d.ChronicConditions {
		rows[r.ICD10Code] = r.Patients
		sum += r.Patients
	}
	if rows["E11.9"] != 2 || rows["I10"] != 2 || sum == d.Patients["chronic"] {
		t.Fatalf("condition rows %v should add up to more than the %v chronic patients", rows, d.Patients["chronic"])
	}
	a := d.Adherence
	if a.Taken != 10 || a.Skipped != 10 || a.Missed != 10 || a.Due != 30 || a.PendingExcluded != 1 {
		t.Fatalf("dose counts: %+v", a)
	}
	// Answering and taking are different facts: y answers every time and
	// takes nothing.
	if a.ResponseRate == nil || *a.ResponseRate != 0.667 || a.AdherenceRate == nil || *a.AdherenceRate != 0.333 {
		t.Fatalf("rates: %v %v", *a.ResponseRate, *a.AdherenceRate)
	}
	if a.Buckets["every_dose"] != 2 || a.Buckets["none"] != 1 {
		t.Fatalf("buckets: %v", a.Buckets)
	}
	if d.Results["unacknowledged"] != 1 || d.Results["critical"] != 1 || d.Consultations["open_drafts"] != 1 {
		t.Fatalf("results %v, consultations %v", d.Results, d.Consultations)
	}
	if d.Prescriptions["expiring_within_14_days"] != 1 || d.Prescriptions["repeats_exhausted"] != 1 {
		t.Fatalf("prescriptions: %v", d.Prescriptions)
	}
	if len(d.Notes) < 5 {
		t.Fatal("the dashboard must say what its figures measure")
	}

	// The summary carries adherence per medicine over 7, 30 and 90 days.
	sum30 := h.expect(h.do("GET", "/api/v1/patients/"+hs.y+"/summary", c.doctor, nil), 200, "summary")
	adh := sum30.body["adherence"].([]any)[0].(map[string]any)["periods"].([]any)
	p30 := adh[1].(map[string]any)
	if p30["days"].(float64) != 30 || p30["skipped"].(float64) != 10 || p30["response_rate"].(float64) != 1 ||
		p30["adherence_rate"].(float64) != 0 {
		t.Fatalf("summary adherence: %v", adh)
	}
}

func TestRecallLists(t *testing.T) {
	h := newHarness(t, harnessOpts{sessionIdle: 400 * 24 * time.Hour, sessionMax: 400 * 24 * time.Hour})
	hs := buildHistory(t, h)
	c := hs.c
	ids := func(r resp) []string {
		var out []string
		for _, x := range r.body["items"].([]any) {
			out = append(out, x.(map[string]any)["patient_id"].(string))
		}
		return out
	}
	list := func(token, query string) resp {
		return h.expect(h.do("GET", "/api/v1/recalls?"+query, token, nil), 200, query)
	}

	// Never reviewed first; a draft note is not a review; the acute patient
	// is not on any chronic list; the patient seen today is not overdue.
	got := ids(list(c.adminToken, "reason=review_overdue"))
	if len(got) != 2 || got[0] != hs.x || got[1] != hs.z {
		t.Fatalf("review_overdue: %v, want [x z]", got)
	}
	if body := fmt.Sprint(list(c.adminToken, "reason=review_overdue").body); strings.Contains(body, "diabetes") || strings.Contains(body, "hypertension") {
		t.Fatal("a recall list named a condition")
	}

	if got := ids(list(c.doctor, "reason=script_expiring&days=14")); len(got) != 1 || got[0] != hs.x {
		t.Fatalf("script_expiring: %v", got)
	}
	if got := ids(list(c.doctor, "reason=script_expiring&days=5")); len(got) != 0 {
		t.Fatalf("script_expiring within 5 days: %v", got)
	}
	if got := ids(list(c.doctor, "reason=repeats_exhausted")); len(got) != 1 || got[0] != hs.y {
		t.Fatalf("repeats_exhausted: %v", got)
	}
	// A new script replaces the exhausted one on the list.
	regs := h.expect(h.do("GET", "/api/v1/patients/"+hs.y+"/regimens", c.doctor, nil), 200, "regimens").body["items"].([]any)
	h.clock.Advance(time.Second)
	h.expect(h.do("POST", "/api/v1/patients/"+hs.y+"/prescriptions", c.doctor, map[string]any{
		"items": []map[string]any{{"regimen_id": regs[0].(map[string]any)["id"], "quantity": "30 tablets"}}, "repeats_authorised": 5,
	}), 201, "new script")
	if got := ids(list(c.doctor, "reason=repeats_exhausted")); len(got) != 0 {
		t.Fatalf("an exhausted script already replaced is still listed: %v", got)
	}

	// Skipping is answering: only z, who never answers, is not responding.
	nr := list(c.nurse, "reason=not_responding")
	if got := ids(nr); len(got) != 1 || got[0] != hs.z {
		t.Fatalf("not_responding: %v", got)
	}
	entry := nr.body["items"].([]any)[0].(map[string]any)
	if entry["doses_due"].(float64) != 10 || entry["doses_answered"].(float64) != 0 || entry["response_rate"].(float64) != 0 {
		t.Fatalf("not_responding entry: %v", entry)
	}

	// Results are for clinicians only.
	if r := h.do("GET", "/api/v1/recalls?reason=unacknowledged_result", c.adminToken, nil); r.status != 403 {
		t.Fatalf("an administrator pulled the results list: %d", r.status)
	}
	res := list(c.doctor, "reason=unacknowledged_result")
	if got := ids(res); len(got) != 1 || got[0] != hs.x || res.body["items"].([]any)[0].(map[string]any)["critical"] != true {
		t.Fatalf("unacknowledged_result: %v", res.body)
	}
	h.expect(h.do("POST", "/api/v1/lab-results/"+hs.xResult+"/acknowledge", c.doctor, nil), 200, "acknowledge")
	if got := ids(list(c.doctor, "reason=unacknowledged_result")); len(got) != 0 {
		t.Fatalf("an acknowledged result is still listed: %v", got)
	}

	if r := h.do("GET", "/api/v1/recalls?reason=everyone", c.doctor, nil); r.status != 422 {
		t.Fatalf("unknown reason: %d", r.status)
	}

	// Every pull is audited as a read of patient data, naming who was listed.
	audit := h.expect(h.do("GET", "/api/v1/audit?action=recall.read&limit=50", c.adminToken, nil), 200, "audit")
	found := false
	for _, x := range audit.body["items"].([]any) {
		e := x.(map[string]any)
		if e["action"] == "recall.read" && strings.Contains(fmt.Sprint(e["metadata"]), hs.z) {
			found = true
		}
	}
	if !found {
		t.Fatal("recall pulls are not audited")
	}

	// Another practice's lists never include these patients.
	other := h.newClinic()
	for _, reason := range []string{"review_overdue", "script_expiring", "repeats_exhausted", "not_responding"} {
		for _, id := range ids(list(other.doctor, "reason="+reason)) {
			if id == hs.x || id == hs.y || id == hs.z || id == hs.w {
				t.Fatalf("%s leaked a patient from another practice", reason)
			}
		}
	}
}

func TestStage3CrossTenantAccessIsNotFound(t *testing.T) {
	h, g := newMessagingHarness(t, harnessOpts{})
	a := h.newClinic()
	h.enableMessaging(a)
	b := h.newClinic()
	h.enableMessaging(b)
	pa, mobile := h.whatsappPatient(a, "Ayanda", "Cele")
	h.regimenWith(a, pa, "Metformin", "once_daily")

	for _, call := range []struct{ method, path, token string }{
		{"POST", "/api/v1/patients/" + pa + "/notifications/reminder", b.nurse},
		{"POST", "/api/v1/patients/" + pa + "/notifications/recall", b.adminToken},
		{"GET", "/api/v1/patients/" + pa + "/notifications", b.doctor},
	} {
		if r := h.do(call.method, call.path, call.token, nil); r.status != 404 {
			t.Errorf("%s %s from another practice: %d %v", call.method, call.path, r.status, r.body)
		}
	}

	// A reply from a different number does not answer this patient's doses,
	// even with a valid button payload.
	h.dueNow(a.nurse, pa)
	h.dispatch()
	sent := g.sentTo(mobile)
	if len(sent) != 1 {
		t.Fatalf("reminder: %d", len(sent))
	}
	_, stranger := randomMobile()
	body, _ := buttonReply(stranger, sent[0].Payloads[0], h.clock.Now())
	h.expect(h.webhook(body, testAppSecret), 200, "stranger")
	now := h.clock.Now()
	for _, d := range h.doses(a.nurse, pa, now.Add(-time.Hour), now) {
		if d.Status != "pending" {
			t.Fatalf("a stranger's reply answered a dose: %+v", d)
		}
	}
	// A made-up payload is ignored.
	body, _ = buttonReply(mobile, "AAAAAAAAAAAAAAAAAAAAAA:taken", h.clock.Now())
	h.expect(h.webhook(body, testAppSecret), 200, "made-up payload")
	for _, d := range h.doses(a.nurse, pa, now.Add(-time.Hour), now) {
		if d.Status != "pending" {
			t.Fatalf("a made-up payload answered a dose: %+v", d)
		}
	}
}

func TestParallelDispatchersRemindOnce(t *testing.T) {
	h, g := newMessagingHarness(t, harnessOpts{})
	c := h.newClinic()
	h.enableMessaging(c)
	var mobiles []string
	var first string
	for i := 0; i < 5; i++ {
		id, m := h.whatsappPatient(c, fmt.Sprintf("Patient%d", i), "Parallel")
		h.regimenWith(c, id, "Metformin", "once_daily")
		mobiles = append(mobiles, m)
		if i == 0 {
			first = id
		}
	}
	h.dueNow(c.nurse, first)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.reg.DispatchReminders(context.Background())
		}()
	}
	wg.Wait()
	for _, m := range mobiles {
		if n := len(g.sentTo(m)); n != 1 {
			t.Fatalf("%s got %d reminders from four dispatchers", m, n)
		}
	}
}
