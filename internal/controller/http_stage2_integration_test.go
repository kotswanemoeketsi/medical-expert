//go:build integration

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/medical-expert-system/internal/ai"
	"github.com/yourorg/medical-expert-system/internal/model"
)

// fakeDrafter stands in for the model provider. It records every call, so a
// test can prove the model was never called — not merely that a request
// failed.
type fakeDrafter struct {
	mu      sync.Mutex
	calls   int
	lastReq ai.Request
	result  *ai.Result
	err     error
}

func (f *fakeDrafter) DraftNote(_ context.Context, req ai.Request) (*ai.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeDrafter) Model() string { return "fake-model" }

func (f *fakeDrafter) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// clinic is a practice with a doctor and a nurse signed in.
type clinic struct {
	practice
	doctor, nurse     string // tokens
	doctorID, nurseID string
}

func (h *harness) newClinic() clinic {
	h.t.Helper()
	p := h.newPractice()
	c := clinic{practice: p}
	var email string
	c.doctorID, email = h.createStaff(p, model.RoleDoctor)
	c.doctor = h.mustLogin(p.slug, email, adminPassword)
	c.nurseID, email = h.createStaff(p, model.RoleNurse)
	c.nurse = h.mustLogin(p.slug, email, adminPassword)
	return c
}

// chronicPatient registers a patient with an active chronic condition.
func (h *harness) chronicPatient(c clinic, first, last string) string {
	h.t.Helper()
	id := h.expect(h.createPatient(c.adminToken, patientBody(first, last, "1966-06-16")), 201, "patient").str("id")
	h.expect(h.do("POST", "/api/v1/patients/"+id+"/conditions", c.doctor,
		map[string]any{"description": "Type 2 diabetes mellitus", "icd10_code": "E11.9", "is_chronic": true}), 201, "condition")
	return id
}

func (h *harness) medication(token, name string) string {
	h.t.Helper()
	r := h.expect(h.do("GET", "/api/v1/medications?q="+name, token, nil), 200, "catalogue")
	items := r.body["items"].([]any)
	if len(items) == 0 {
		h.t.Fatalf("catalogue has no %s", name)
	}
	return items[0].(map[string]any)["id"].(string)
}

func (h *harness) regimen(c clinic, patientID, medication string) string {
	h.t.Helper()
	return h.expect(h.do("POST", "/api/v1/patients/"+patientID+"/regimens", c.doctor, map[string]any{
		"medication_id": h.medication(c.doctor, medication), "dose": "1 tablet", "frequency": "twice_daily",
		"instructions": "with meals",
	}), 201, "regimen").str("id")
}

func (h *harness) grantAIConsent(c clinic, patientID string, granted bool) {
	h.t.Helper()
	h.expect(h.do("POST", "/api/v1/patients/"+patientID+"/consents", c.nurse,
		map[string]any{"purpose": "ai_assisted_review", "granted": granted, "method": "written"}), 201, "consent")
	h.clock.Advance(time.Millisecond)
}

func (h *harness) signedEncounter(c clinic, patientID string) string {
	h.t.Helper()
	id := h.expect(h.do("POST", "/api/v1/patients/"+patientID+"/encounters", c.doctor, map[string]any{
		"presenting_complaint": "Routine review", "assessment": "Stable", "plan": "Continue",
	}), 201, "encounter").str("id")
	h.expect(h.do("POST", "/api/v1/encounters/"+id+"/sign", c.doctor, nil), 200, "sign")
	return id
}

func errField(r resp) string { return fmt.Sprint(r.body["error"].(map[string]any)["fields"]) }

func (h *harness) year() int { return h.clock.Now().In(time.FixedZone("SAST", 7200)).Year() }

// ---------------------------------------------------------------------------

func TestEncounterLifecycle(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.newClinic()
	pid := h.chronicPatient(c, "Themba", "Mabuza")

	seen := h.clock.Now().Add(-20 * time.Minute).UTC().Truncate(time.Second)
	r := h.expect(h.do("POST", "/api/v1/patients/"+pid+"/encounters", c.nurse, map[string]any{
		"encounter_type": "follow_up", "encounter_at": seen.Format(time.RFC3339),
		"presenting_complaint": "Headache for two days"}), 201, "create")
	id := r.str("id")
	if r.str("status") != "draft" || r.str("clinician_id") != c.nurseID {
		t.Fatalf("new note: %v", r.body)
	}
	if got := h.do("PATCH", "/api/v1/encounters/"+id, c.doctor, map[string]any{"assessment": "x"}); got.errCode() != "not_author" {
		t.Fatalf("someone else edited the draft: %v", got.body)
	}
	if got := h.do("POST", "/api/v1/encounters/"+id+"/sign", c.nurse, nil); got.status != 422 {
		t.Fatalf("signed without an assessment or plan: %d", got.status)
	}
	bad := h.do("PATCH", "/api/v1/encounters/"+id, c.nurse, map[string]any{
		"presenting_complaint": "Headache", "vitals": map[string]any{"systolic_bp": 1200, "diastolic_bp": 80}})
	if bad.status != 422 || !strings.Contains(errField(bad), "vitals.systolic_bp") {
		t.Fatalf("implausible vitals accepted: %v", bad.body)
	}
	upd := h.expect(h.do("PATCH", "/api/v1/encounters/"+id, c.nurse, map[string]any{
		"presenting_complaint": "Headache for two days", "examination": "BP 128/82, afebrile",
		"assessment": "Tension-type headache", "plan": "Analgesia, review if worse",
		"vitals": map[string]any{"systolic_bp": 128, "diastolic_bp": 82, "pulse_rate": 76}}), 200, "update")
	if upd.str("encounter_type") != "follow_up" || upd.str("encounter_at") != r.str("encounter_at") {
		t.Fatalf("an update that left out the type and time reset them: %v", upd.body)
	}
	signed := h.expect(h.do("POST", "/api/v1/encounters/"+id+"/sign", c.nurse, nil), 200, "sign")
	if signed.str("status") != "signed" || signed.str("signed_at") == "" {
		t.Fatalf("not signed: %v", signed.body)
	}
	if got := h.do("PATCH", "/api/v1/encounters/"+id, c.nurse, map[string]any{"assessment": "changed"}); got.errCode() != "note_state" {
		t.Fatalf("a signed note was edited in place: %v", got.body)
	}

	// Corrections are amendments that keep the original.
	if got := h.do("POST", "/api/v1/encounters/"+id+"/amendments", c.doctor,
		map[string]any{"field": "assessment", "new_value": "Migraine without aura"}); got.status != 422 {
		t.Fatalf("amendment without a reason: %d", got.status)
	}
	if got := h.do("POST", "/api/v1/encounters/"+id+"/amendments", c.doctor,
		map[string]any{"field": "encounter_at", "new_value": "x", "reason": "r"}); got.status != 422 {
		t.Fatalf("amended a field that is not amendable: %d", got.status)
	}
	am := h.expect(h.do("POST", "/api/v1/encounters/"+id+"/amendments", c.doctor, map[string]any{
		"field": "assessment", "new_value": "Migraine without aura", "reason": "Photophobia reported after signing"}), 200, "amend")
	enc := am.body["encounter"].(map[string]any)
	amends := am.body["amendments"].([]any)
	if enc["assessment"] != "Migraine without aura" || enc["signed_at"] != signed.str("signed_at") || enc["amended_at"] == nil {
		t.Fatalf("amendment not applied, or the signature changed: %v", enc)
	}
	first := amends[0].(map[string]any)
	if len(amends) != 1 || first["previous_value"] != "Tension-type headache" || first["amended_by"] != c.doctorID {
		t.Fatalf("original not preserved: %v", amends)
	}

	// A patient sees their signed notes only; an administrator sees none.
	h.expect(h.do("POST", "/api/v1/patients/"+pid+"/encounters", c.doctor, map[string]any{"presenting_complaint": "Draft"}), 201, "draft")
	email := "patient-" + strings.ToLower(pid[:6]) + "@" + c.slug + ".test"
	h.expect(h.do("POST", "/api/v1/users", c.adminToken, map[string]any{"email": email, "full_name": "Themba Mabuza",
		"role": "patient", "patient_id": pid, "password": adminPassword}), 201, "patient login")
	pt := h.mustLogin(c.slug, email, adminPassword)
	mine := h.expect(h.do("GET", "/api/v1/patients/"+pid+"/encounters", pt, nil), 200, "patient list")
	if n := len(mine.body["items"].([]any)); n != 1 {
		t.Fatalf("patient should see only the signed note, saw %d", n)
	}
	all := h.expect(h.do("GET", "/api/v1/patients/"+pid+"/encounters", c.doctor, nil), 200, "clinician list")
	if n := len(all.body["items"].([]any)); n != 2 {
		t.Fatalf("clinician should see both notes, saw %d", n)
	}
	h.expect(h.do("GET", "/api/v1/patients/"+pid+"/encounters", c.adminToken, nil), 403, "admin list")

	sum := h.expect(h.do("GET", "/api/v1/patients/"+pid+"/summary", c.doctor, nil), 200, "summary")
	if n := len(sum.body["recent_encounters"].([]any)); n != 1 || sum.body["open_draft_notes"].(float64) != 1 {
		t.Fatalf("summary: %d recent, %v open drafts", n, sum.body["open_draft_notes"])
	}
}

func TestAIDraftingNeedsConsentAndNeverTouchesTheNote(t *testing.T) {
	fake := &fakeDrafter{result: &ai.Result{Model: "fake-model-2026", InputTokens: 900, OutputTokens: 200,
		Draft: ai.Draft{PresentingComplaint: "Cough for three days", History: "Dry cough, no fever",
			Examination: "Chest clear", Assessment: "Viral URTI", Plan: "Fluids and rest",
			Flags: []string{"Dose of amoxicillin unclear"}},
		Discarded: []string{"diagnosis"}}}
	h := newHarness(t, harnessOpts{drafter: fake})
	c := h.newClinic()
	pid := h.chronicPatient(c, "Nosipho", "Khanyile")
	h.regimen(c, pid, "Metformin")
	h.expect(h.do("POST", "/api/v1/patients/"+pid+"/allergies", c.doctor,
		map[string]any{"substance": "Sulphonamides", "severity": "severe"}), 201, "allergy")
	id := h.expect(h.do("POST", "/api/v1/patients/"+pid+"/encounters", c.doctor,
		map[string]any{"presenting_complaint": "Typed by the doctor"}), 201, "encounter").str("id")
	dictation := map[string]any{"dictation": "Nosipho Khanyile presents with a dry cough for three days. Chest clear."}

	r := h.do("POST", "/api/v1/encounters/"+id+"/draft-note", c.doctor, dictation)
	if r.errCode() != "ai_consent_required" || fake.callCount() != 0 {
		t.Fatalf("drafted without consent (status %d, %d model calls)", r.status, fake.callCount())
	}
	h.grantAIConsent(c, pid, true)

	if got := h.do("POST", "/api/v1/encounters/"+id+"/draft-note", c.nurse, dictation); got.errCode() != "not_author" {
		t.Fatalf("drafted on someone else's note: %v", got.body)
	}
	r = h.expect(h.do("POST", "/api/v1/encounters/"+id+"/draft-note", c.doctor, dictation), 201, "draft")
	draft := r.body["draft"].(map[string]any)
	content := draft["draft"].(map[string]any)
	if content["assessment"] != "Viral URTI" || r.str("notice") == "" || draft["model"] != "fake-model-2026" {
		t.Fatalf("draft: %v", r.body)
	}
	flags := fmt.Sprint(content["flags"])
	if !strings.Contains(flags, "discarded unread") || !strings.Contains(flags, "replaced with a placeholder") {
		t.Fatalf("the clinician was not told about discarded output or redaction: %s", flags)
	}

	// What left the building: no name, labelled background with the
	// medication list.
	sent := fake.lastReq
	if strings.Contains(sent.Dictation, "Nosipho") || strings.Contains(sent.Dictation, "Khanyile") ||
		!strings.Contains(sent.Dictation, "[patient]") {
		t.Fatalf("the patient's name was sent to the model: %q", sent.Dictation)
	}
	if sent.Context.AgeYears != 60 || len(sent.Context.Medication) != 1 || len(sent.Context.Allergies) != 1 {
		t.Fatalf("background: %+v", sent.Context)
	}

	// The note itself is untouched, but its provenance is recorded.
	enc := h.expect(h.do("GET", "/api/v1/encounters/"+id, c.doctor, nil), 200, "get").body["encounter"].(map[string]any)
	if enc["presenting_complaint"] != "Typed by the doctor" || enc["assessment"] != "" || enc["ai_assisted"] != true {
		t.Fatalf("the draft touched the note, or provenance is missing: %v", enc)
	}
	drafts := h.expect(h.do("GET", "/api/v1/encounters/"+id+"/drafts", c.doctor, nil), 200, "drafts").body["items"].([]any)
	d0 := drafts[0].(map[string]any)
	if len(drafts) != 1 || !strings.Contains(d0["dictation"].(string), "Nosipho") {
		t.Fatalf("the practice's own copy should keep the dictation as spoken: %v", drafts)
	}
	draftID := d0["id"].(string)
	h.expect(h.do("POST", "/api/v1/encounters/"+id+"/drafts/"+draftID+"/flag", c.doctor,
		map[string]any{"reason": "Added a finding I did not dictate"}), 204, "flag")
	if got := h.do("POST", "/api/v1/encounters/"+id+"/drafts/"+draftID+"/flag", c.doctor,
		map[string]any{"reason": "again"}); got.errCode() != "already_flagged" {
		t.Fatalf("flagged twice: %v", got.body)
	}

	// Withdrawing consent stops drafting at once.
	h.grantAIConsent(c, pid, false)
	calls := fake.callCount()
	if got := h.do("POST", "/api/v1/encounters/"+id+"/draft-note", c.doctor, dictation); got.errCode() != "ai_consent_required" ||
		fake.callCount() != calls {
		t.Fatalf("drafted after consent was withdrawn: %v", got.body)
	}
}

func TestAIDraftingOffLimitedAndFailing(t *testing.T) {
	off := newHarness(t, harnessOpts{})
	c := off.newClinic()
	pid := off.chronicPatient(c, "Sibusiso", "Radebe")
	off.grantAIConsent(c, pid, true)
	id := off.expect(off.do("POST", "/api/v1/patients/"+pid+"/encounters", c.doctor, map[string]any{}), 201, "encounter").str("id")
	if got := off.do("POST", "/api/v1/encounters/"+id+"/draft-note", c.doctor, map[string]any{"dictation": "x"}); got.status != 503 ||
		got.errCode() != "ai_unavailable" {
		t.Fatalf("drafting without a configured model: %d %v", got.status, got.body)
	}

	fake := &fakeDrafter{result: &ai.Result{Draft: ai.Draft{Plan: "p", Flags: []string{}}}}
	h := newHarness(t, harnessOpts{drafter: fake, aiDailyLimit: 2})
	c = h.newClinic()
	pid = h.chronicPatient(c, "Lindani", "Shabalala")
	h.grantAIConsent(c, pid, true)
	id = h.expect(h.do("POST", "/api/v1/patients/"+pid+"/encounters", c.doctor, map[string]any{}), 201, "encounter").str("id")
	h.expect(h.do("POST", "/api/v1/encounters/"+id+"/draft-note", c.doctor, map[string]any{"dictation": "first"}), 201, "first")

	fake.mu.Lock()
	fake.err = errors.New("ai: provider returned 529 overloaded_error")
	fake.mu.Unlock()
	h.clock.Advance(time.Second)
	if got := h.do("POST", "/api/v1/encounters/"+id+"/draft-note", c.doctor, map[string]any{"dictation": "second"}); got.status != 503 ||
		got.errCode() != "ai_failed" {
		t.Fatalf("provider failure: %d %v", got.status, got.body)
	}
	drafts := h.expect(h.do("GET", "/api/v1/encounters/"+id+"/drafts", c.doctor, nil), 200, "drafts").body["items"].([]any)
	if len(drafts) != 2 || drafts[0].(map[string]any)["status"] != "failed" {
		t.Fatalf("the failed attempt should be kept with the dictation: %v", drafts)
	}
	// Two attempts today, failures included: the third is refused before
	// the model is called.
	calls := fake.callCount()
	if got := h.do("POST", "/api/v1/encounters/"+id+"/draft-note", c.doctor, map[string]any{"dictation": "third"}); got.status != 429 ||
		got.errCode() != "ai_limit_reached" || fake.callCount() != calls {
		t.Fatalf("daily limit: %d %v", got.status, got.body)
	}
}

func TestPrescriptionRules(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.newClinic()

	plain := h.expect(h.createPatient(c.adminToken, patientBody("Not", "Chronic", "1990-01-01")), 201, "patient").str("id")
	pr := h.regimen(c, plain, "Paracetamol")
	if got := h.do("POST", "/api/v1/patients/"+plain+"/prescriptions", c.doctor, map[string]any{
		"items": []any{map[string]any{"regimen_id": pr, "quantity": "20 tablets"}}}); got.errCode() != "not_chronic" {
		t.Fatalf("script for a patient not on the chronic register: %v", got.body)
	}

	pid := h.chronicPatient(c, "Refilwe", "Molefe")
	h.expect(h.do("POST", "/api/v1/patients/"+pid+"/allergies", c.doctor,
		map[string]any{"substance": "Penicillin", "severity": "life_threatening"}), 201, "allergy")
	reg := h.regimen(c, pid, "Metformin")
	script := map[string]any{"items": []any{map[string]any{"regimen_id": reg, "quantity": "60 tablets"}}, "repeats_authorised": 1}

	h.expect(h.do("POST", "/api/v1/patients/"+pid+"/prescriptions", c.nurse, script), 403, "nurse")
	r := h.do("POST", "/api/v1/patients/"+pid+"/prescriptions", c.doctor, script)
	if r.errCode() != "review_overdue" || !strings.Contains(errField(r), "override_reason") {
		t.Fatalf("a never-reviewed patient needs an override: %v", r.body)
	}
	script["override_reason"] = "Seen at hospital last week; review booked"
	r = h.expect(h.do("POST", "/api/v1/patients/"+pid+"/prescriptions", c.doctor, script), 201, "issue with override")
	orig := r.body["prescription"].(map[string]any)
	if r.str("notice") == "" || !strings.Contains(r.str("notice"), "No interaction, allergy or appropriateness checks") {
		t.Fatalf("the response must say that nothing was checked: %v", r.body)
	}
	if n := len(r.body["recorded_allergies"].([]any)); n != 1 {
		t.Fatalf("recorded allergies should be shown for information, got %d", n)
	}
	item := orig["items"].([]any)[0].(map[string]any)
	if item["medication_name"] != "Metformin 500 mg tablet" || item["frequency"] != "twice daily" || orig["override_reason"] == "" {
		t.Fatalf("item not copied from the regimen: %v", orig)
	}

	// Items are copied: stopping the regimen does not change the script.
	h.expect(h.do("POST", "/api/v1/regimens/"+reg+"/status", c.doctor,
		map[string]any{"status": "stopped", "reason": "Changed to gliclazide"}), 200, "stop regimen")
	again := h.expect(h.do("GET", "/api/v1/prescriptions/"+orig["id"].(string), c.nurse, nil), 200, "get")
	if again.body["items"].([]any)[0].(map[string]any)["medication_name"] != "Metformin 500 mg tablet" {
		t.Fatal("the script changed when its regimen did")
	}

	// Once reviewed in a signed note, no override is needed.
	h.signedEncounter(c, pid)
	reg2 := h.regimen(c, pid, "Gliclazide")
	h.expect(h.do("POST", "/api/v1/patients/"+pid+"/prescriptions", c.doctor, map[string]any{
		"items": []any{map[string]any{"regimen_id": reg2, "quantity": "30 tablets"}}}), 201, "issue after review")

	// One repeat authorised: the first succeeds, the second is refused.
	rep := h.expect(h.do("POST", "/api/v1/prescriptions/"+orig["id"].(string)+"/repeat", c.doctor, nil), 201, "repeat")
	rp := rep.body["prescription"].(map[string]any)
	if rp["repeat_of_id"] != orig["id"] || rp["valid_until"] != orig["valid_until"] {
		t.Fatalf("repeat must point at the original and keep its expiry: %v", rp)
	}
	if got := h.do("POST", "/api/v1/prescriptions/"+rp["id"].(string)+"/repeat", c.doctor, nil); got.errCode() != "repeats_exhausted" {
		t.Fatalf("repeat beyond the authorisation: %v", got.body)
	}

	// Cancelled scripts cannot be repeated.
	many := h.expect(h.do("POST", "/api/v1/patients/"+pid+"/prescriptions", c.doctor, map[string]any{
		"items": []any{map[string]any{"regimen_id": reg2, "quantity": "30 tablets"}}, "repeats_authorised": 5}), 201, "five repeats")
	manyID := many.body["prescription"].(map[string]any)["id"].(string)
	h.expect(h.do("POST", "/api/v1/prescriptions/"+manyID+"/cancel", c.doctor, map[string]any{}), 422, "cancel without reason")
	h.expect(h.do("POST", "/api/v1/prescriptions/"+manyID+"/cancel", c.doctor, map[string]any{"reason": "Duplicate"}), 200, "cancel")
	if got := h.do("POST", "/api/v1/prescriptions/"+manyID+"/repeat", c.doctor, nil); got.errCode() != "prescription_cancelled" {
		t.Fatalf("repeat of a cancelled script: %v", got.body)
	}

	// Validity is capped; another patient's regimen cannot be put on a script.
	far := model.DateOf(h.clock.Now().AddDate(1, 0, 0)).String()
	if got := h.do("POST", "/api/v1/patients/"+pid+"/prescriptions", c.doctor, map[string]any{
		"items": []any{map[string]any{"regimen_id": reg2, "quantity": "30"}}, "valid_until": far}); got.status != 422 {
		t.Fatalf("a year-long authorisation was accepted: %d", got.status)
	}
	if got := h.do("POST", "/api/v1/patients/"+pid+"/prescriptions", c.doctor, map[string]any{
		"items": []any{map[string]any{"regimen_id": pr, "quantity": "30"}}}); got.status != 422 || !strings.Contains(errField(got), "regimen_id") {
		t.Fatalf("another patient's regimen on a script: %d %v", got.status, got.body)
	}
	list := h.expect(h.do("GET", "/api/v1/patients/"+pid+"/prescriptions", c.nurse, nil), 200, "list").body["items"].([]any)
	if len(list) != 4 {
		t.Fatalf("expected 4 scripts (original, repeat, two more), got %d", len(list))
	}
	for _, p := range list {
		if len(p.(map[string]any)["items"].([]any)) != 1 {
			t.Fatal("a listed script has no items")
		}
	}
}

func TestLabResultsAndInbox(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.newClinic()
	pid := h.chronicPatient(c, "Karabo", "Sithole")
	post := func(panel string, flag string) resp {
		return h.do("POST", "/api/v1/patients/"+pid+"/lab-results", c.nurse, map[string]any{
			"lab_name": "Lancet", "panel": panel, "items": []any{
				map[string]any{"analyte": panel, "value": "8.1", "unit": "%", "reference_range": "4.0-6.0", "abnormal_flag": flag},
				map[string]any{"analyte": "Comment", "value": "See note"},
			}})
	}
	if got := post("HbA1c", "H"); got.status != 422 || !strings.Contains(errField(got), "abnormal_flag") {
		t.Fatalf("an unrecognised flag was accepted: %v", got.body)
	}
	high := h.expect(post("HbA1c", "high"), 201, "high")
	if high.body["has_abnormal"] != true || high.body["has_critical"] != false ||
		high.body["items"].([]any)[0].(map[string]any)["value"] != "8.1" {
		t.Fatalf("stored differently from the report: %v", high.body)
	}
	h.clock.Advance(time.Minute)
	critical := h.expect(post("Potassium", "critical_high"), 201, "critical")
	h.clock.Advance(time.Minute)
	h.expect(post("FBC", "none"), 201, "normal")

	inbox := h.expect(h.do("GET", "/api/v1/lab-results", c.doctor, nil), 200, "inbox").body["items"].([]any)
	if len(inbox) != 3 || inbox[0].(map[string]any)["id"] != critical.str("id") {
		t.Fatalf("critical results must come first: %v", inbox)
	}
	for _, it := range inbox {
		m := it.(map[string]any)
		if len(m["items"].([]any)) != 2 || m["patient_name"] != "Karabo Sithole" {
			t.Fatalf("inbox entry without analytes or patient: %v", m)
		}
	}
	abn := h.expect(h.do("GET", "/api/v1/lab-results?abnormal_only=true", c.doctor, nil), 200, "abnormal").body["items"].([]any)
	if len(abn) != 2 {
		t.Fatalf("abnormal-only inbox: %d", len(abn))
	}
	h.expect(h.do("GET", "/api/v1/lab-results", c.adminToken, nil), 403, "admin inbox")

	sum := h.expect(h.do("GET", "/api/v1/patients/"+pid+"/summary", c.doctor, nil), 200, "summary")
	if sum.body["unacknowledged_abnormal_results"].(float64) != 2 {
		t.Fatalf("summary count: %v", sum.body["unacknowledged_abnormal_results"])
	}
	ack := h.expect(h.do("POST", "/api/v1/lab-results/"+critical.str("id")+"/acknowledge", c.doctor,
		map[string]any{"note": "Phoned patient; repeat today"}), 200, "acknowledge")
	if ack.body["acknowledged_by"] != c.doctorID || ack.str("acknowledgement_note") == "" {
		t.Fatalf("acknowledgement not recorded: %v", ack.body)
	}
	if got := h.do("POST", "/api/v1/lab-results/"+critical.str("id")+"/acknowledge", c.nurse, nil); got.errCode() != "already_acknowledged" {
		t.Fatalf("acknowledged twice: %v", got.body)
	}
	inbox = h.expect(h.do("GET", "/api/v1/lab-results", c.doctor, nil), 200, "inbox").body["items"].([]any)
	if len(inbox) != 2 {
		t.Fatalf("acknowledged result still in the inbox: %d", len(inbox))
	}
}

func TestSickNoteRules(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.newClinic()
	pid := h.chronicPatient(c, "Ayanda", "Ngubane")
	today := model.DateOf(h.clock.Now().In(time.FixedZone("SAST", 7200)))
	d := func(days int) string { return model.DateOf(today.Time().AddDate(0, 0, days)).String() }

	r := h.do("POST", "/api/v1/patients/"+pid+"/sick-notes", c.nurse, map[string]any{
		"examined_on": d(0), "valid_from": d(0), "valid_to": d(2)})
	if r.status != 422 || !strings.Contains(errField(r), "basis") {
		t.Fatalf("a certificate without a stated basis was accepted: %v", r.body)
	}
	r = h.do("POST", "/api/v1/patients/"+pid+"/sick-notes", c.nurse, map[string]any{
		"basis": "examination", "examined_on": d(0), "valid_from": d(-2), "valid_to": d(1)})
	if r.errCode() != "backdate_reason_required" {
		t.Fatalf("backdated without a reason: %v", r.body)
	}
	r = h.do("POST", "/api/v1/patients/"+pid+"/sick-notes", c.nurse, map[string]any{
		"basis": "examination", "examined_on": d(0), "valid_from": d(0), "valid_to": d(1), "disclose_diagnosis": true})
	if r.status != 422 || !strings.Contains(errField(r), "diagnosis") {
		t.Fatalf("disclosure without a diagnosis: %v", r.body)
	}
	draft := h.expect(h.do("POST", "/api/v1/patients/"+pid+"/sick-notes", c.nurse, map[string]any{
		"basis": "patient_report", "examined_on": d(0), "valid_from": d(-1), "valid_to": d(1),
		"backdate_reason": "Too ill to attend yesterday", "diagnosis": "Gastroenteritis", "employee_number": "E-2211"}), 201, "draft")
	if draft.str("status") != "draft" || draft.str("document_number") != "" || draft.str("rendered_text") != "" {
		t.Fatalf("a draft must have no number or text yet: %v", draft.body)
	}
	docID := draft.str("id")
	h.expect(h.do("POST", "/api/v1/documents/"+docID+"/issue", c.nurse, nil), 403, "nurse issue")
	issued := h.expect(h.do("POST", "/api/v1/documents/"+docID+"/issue", c.doctor, nil), 200, "issue")
	want := fmt.Sprintf("MC-%d-0001", h.year())
	text := strings.Join(strings.Fields(issued.str("rendered_text")), " ")
	if issued.str("document_number") != want || issued.str("status") != "issued" {
		t.Fatalf("expected %s: %v", want, issued.body)
	}
	for _, phrase := range []string{want, "based on what the patient reported to me, not on my own examination",
		"not disclosed, in accordance with patient confidentiality", "BACKDATED", "Too ill to attend yesterday",
		"employee number E-2211"} {
		if !strings.Contains(text, phrase) {
			t.Errorf("certificate missing %q:\n%s", phrase, issued.str("rendered_text"))
		}
	}
	if strings.Contains(text, "Gastroenteritis") {
		t.Fatal("a diagnosis the patient did not agree to disclose appears on the certificate")
	}
	if got := h.do("POST", "/api/v1/documents/"+docID+"/issue", c.doctor, nil); got.errCode() != "document_state" {
		t.Fatalf("issued twice: %v", got.body)
	}
	h.expect(h.do("POST", "/api/v1/documents/"+docID+"/cancel", c.doctor, nil), 422, "withdraw without reason")
	cancelled := h.expect(h.do("POST", "/api/v1/documents/"+docID+"/cancel", c.doctor,
		map[string]any{"reason": "Issued to the wrong employer"}), 200, "withdraw")
	if cancelled.str("status") != "cancelled" || cancelled.str("document_number") != want || cancelled.str("rendered_text") != issued.str("rendered_text") {
		t.Fatalf("a withdrawn certificate must keep its number and text: %v", cancelled.body)
	}
}

func TestReferralDisclosesOnlyWhatWasAskedFor(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.newClinic()
	pid := h.chronicPatient(c, "Mpho", "Mokoena")
	h.regimen(c, pid, "Amlodipine")
	h.expect(h.do("POST", "/api/v1/patients/"+pid+"/allergies", c.doctor,
		map[string]any{"substance": "Codeine", "reaction": "Rash", "severity": "moderate"}), 201, "allergy")

	issue := func(body map[string]any) string {
		t.Helper()
		id := h.expect(h.do("POST", "/api/v1/patients/"+pid+"/referrals", c.nurse, body), 201, "referral").str("id")
		return h.expect(h.do("POST", "/api/v1/documents/"+id+"/issue", c.doctor, nil), 200, "issue").str("rendered_text")
	}
	h.expect(h.do("POST", "/api/v1/patients/"+pid+"/referrals", c.nurse, map[string]any{"reason": "x"}), 422, "no recipient")
	plain := issue(map[string]any{"to_name": "Ms L Physio", "to_speciality": "Physiotherapist", "reason": "Lower back pain"})
	if strings.Contains(plain, "Amlodipine") || strings.Contains(plain, "Codeine") {
		t.Fatalf("medication or allergies disclosed without being asked for:\n%s", plain)
	}
	full := issue(map[string]any{"to_name": "Dr A Botha", "to_speciality": "Cardiologist", "urgency": "urgent",
		"reason": "Exertional chest pain", "include_medication": true, "include_allergies": true})
	for _, want := range []string{fmt.Sprintf("REF-%d-0002", h.year()), "Amlodipine", "mg tablet: 1 tablet twice daily", "Codeine: Rash (moderate)", "URGENT"} {
		if !strings.Contains(full, want) {
			t.Errorf("referral missing %q:\n%s", want, full)
		}
	}
	if !strings.Contains(plain, fmt.Sprintf("REF-%d-0001", h.year())) {
		t.Errorf("first referral should be number 0001:\n%s", plain)
	}
	list := h.expect(h.do("GET", "/api/v1/patients/"+pid+"/documents", c.doctor, nil), 200, "list").body["items"].([]any)
	if len(list) != 2 {
		t.Fatalf("documents: %d", len(list))
	}
}

func TestDosesAndResponses(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	c := h.newClinic()
	pid := h.chronicPatient(c, "Lerato", "Tau")
	other := h.chronicPatient(c, "Other", "Person")
	reg := h.regimen(c, pid, "Metformin")
	otherReg := h.regimen(c, other, "Metformin")
	now := h.clock.Now()
	times := []string{now.Add(-time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339),
		now.Add(5 * time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339)}
	r := h.expect(h.do("POST", "/api/v1/regimens/"+reg+"/doses", c.nurse, map[string]any{"times": times}), 200, "schedule")
	if r.body["added"].(float64) != 3 {
		t.Fatalf("expected 3 doses (one duplicate), got %v", r.body["added"])
	}
	h.expect(h.do("POST", "/api/v1/regimens/"+otherReg+"/doses", c.nurse,
		map[string]any{"times": []string{now.Add(-time.Hour).Format(time.RFC3339)}}), 200, "other schedule")

	email := "lerato-" + strings.ToLower(pid[:6]) + "@" + c.slug + ".test"
	h.expect(h.do("POST", "/api/v1/users", c.adminToken, map[string]any{"email": email, "full_name": "Lerato Tau",
		"role": "patient", "patient_id": pid, "password": adminPassword}), 201, "patient login")
	pt := h.mustLogin(c.slug, email, adminPassword)

	// With a login of her own she can answer, so the scheduler adds her
	// regular doses too; the three added by hand are still there.
	doses := h.expect(h.do("GET", "/api/v1/patients/"+pid+"/doses", pt, nil), 200, "own doses").body["items"].([]any)
	byTime := map[string]string{}
	manual := 0
	for _, d := range doses {
		m := d.(map[string]any)
		byTime[m["scheduled_at"].(string)] = m["id"].(string)
		if m["source"] == "manual" {
			manual++
		}
	}
	if manual != 3 {
		t.Fatalf("manual doses: %d", manual)
	}
	due := byTime[now.Add(-time.Hour).UTC().Format("2006-01-02T15:04:05Z")]
	later := byTime[now.Add(5*time.Hour).UTC().Format("2006-01-02T15:04:05Z")]
	if due == "" || later == "" {
		t.Fatalf("could not find doses by time: %v", byTime)
	}
	taken := h.expect(h.do("POST", "/api/v1/doses/"+due+"/response", pt, map[string]any{"status": "taken"}), 200, "taken")
	if taken.str("response_channel") != "portal" || taken.str("status") != "taken" {
		t.Fatalf("a patient's own answer must be recorded as the portal: %v", taken.body)
	}
	if got := h.do("POST", "/api/v1/doses/"+due+"/response", pt, map[string]any{"status": "skipped"}); got.errCode() != "dose_state" {
		t.Fatalf("answered twice: %v", got.body)
	}
	if got := h.do("POST", "/api/v1/doses/"+later+"/response", pt, map[string]any{"status": "taken"}); got.errCode() != "dose_not_due" {
		t.Fatalf("a dose five hours away was confirmed: %v", got.body)
	}
	otherDoses := h.expect(h.do("GET", "/api/v1/patients/"+other+"/doses", c.nurse, nil), 200, "other doses").body["items"].([]any)
	otherDose := otherDoses[0].(map[string]any)["id"].(string)
	h.expect(h.do("POST", "/api/v1/doses/"+otherDose+"/response", pt, map[string]any{"status": "taken"}), 404, "someone else's dose")
	h.expect(h.do("GET", "/api/v1/patients/"+other+"/doses", pt, nil), 404, "someone else's dose list")
	staff := h.expect(h.do("POST", "/api/v1/doses/"+otherDose+"/response", c.nurse,
		map[string]any{"status": "skipped", "reason": "Ran out of tablets"}), 200, "staff")
	if staff.str("response_channel") != "staff" || staff.str("skip_reason") != "Ran out of tablets" {
		t.Fatalf("staff answer: %v", staff.body)
	}

	h.expect(h.do("POST", "/api/v1/regimens/"+reg+"/status", c.doctor, map[string]any{"status": "paused"}), 422, "no reason")
	h.expect(h.do("POST", "/api/v1/regimens/"+reg+"/status", c.nurse, map[string]any{"status": "paused", "reason": "x"}), 403, "nurse")
	paused := h.expect(h.do("POST", "/api/v1/regimens/"+reg+"/status", c.doctor,
		map[string]any{"status": "paused", "reason": "Vomiting; hold until review"}), 200, "pause")
	if paused.str("status") != "paused" || paused.str("status_reason") == "" || paused.body["status_changed_by"] != c.doctorID {
		t.Fatalf("pause not recorded: %v", paused.body)
	}
	h.expect(h.do("POST", "/api/v1/regimens/"+reg+"/status", c.doctor, map[string]any{"status": "stopped", "reason": "r"}), 200, "stop")
	if got := h.do("POST", "/api/v1/regimens/"+reg+"/status", c.doctor, map[string]any{"status": "active", "reason": "r"}); got.errCode() != "regimen_state" {
		t.Fatalf("a stopped regimen was restarted: %v", got.body)
	}
	regs := h.expect(h.do("GET", "/api/v1/patients/"+pid+"/regimens", pt, nil), 200, "own regimens").body["items"].([]any)
	if len(regs) != 1 {
		t.Fatalf("regimens: %d", len(regs))
	}
}

// A valid session and a valid record ID from another practice must answer
// "not found" on every stage 2 read and write path.
func TestStage2CrossTenantAccessIsNotFound(t *testing.T) {
	fake := &fakeDrafter{result: &ai.Result{Draft: ai.Draft{Flags: []string{}}}}
	h := newHarness(t, harnessOpts{drafter: fake})
	a := h.newClinic()
	b := h.newClinic()

	pb := h.chronicPatient(b, "Bongi", "Zulu")
	h.grantAIConsent(b, pb, true)
	regB := h.regimen(b, pb, "Metformin")
	now := h.clock.Now()
	h.expect(h.do("POST", "/api/v1/regimens/"+regB+"/doses", b.nurse,
		map[string]any{"times": []string{now.Add(-time.Hour).Format(time.RFC3339)}}), 200, "dose")
	doseB := h.expect(h.do("GET", "/api/v1/patients/"+pb+"/doses", b.nurse, nil), 200, "doses").body["items"].([]any)[0].(map[string]any)["id"].(string)
	encB := h.signedEncounter(b, pb)
	draftEnc := h.expect(h.do("POST", "/api/v1/patients/"+pb+"/encounters", b.doctor, map[string]any{}), 201, "draft").str("id")
	h.expect(h.do("POST", "/api/v1/encounters/"+draftEnc+"/draft-note", b.doctor, map[string]any{"dictation": "d"}), 201, "ai")
	draftB := h.expect(h.do("GET", "/api/v1/encounters/"+draftEnc+"/drafts", b.doctor, nil), 200, "drafts").body["items"].([]any)[0].(map[string]any)["id"].(string)
	rxB := h.expect(h.do("POST", "/api/v1/patients/"+pb+"/prescriptions", b.doctor, map[string]any{
		"items": []any{map[string]any{"regimen_id": regB, "quantity": "60"}}, "repeats_authorised": 3}), 201, "rx").body["prescription"].(map[string]any)["id"].(string)
	labB := h.expect(h.do("POST", "/api/v1/patients/"+pb+"/lab-results", b.nurse, map[string]any{
		"lab_name": "Ampath", "panel": "U&E", "items": []any{map[string]any{"analyte": "K", "value": "6.9", "abnormal_flag": "critical_high"}}}), 201, "lab").str("id")
	docB := h.expect(h.do("POST", "/api/v1/patients/"+pb+"/referrals", b.nurse, map[string]any{"to_name": "Dr X", "reason": "r"}), 201, "doc").str("id")
	callsBefore := fake.callCount()

	checks := []struct {
		method, path string
		token        string
		body         any
	}{
		{"GET", "/api/v1/patients/" + pb + "/regimens", a.doctor, nil},
		{"POST", "/api/v1/patients/" + pb + "/regimens", a.doctor, map[string]any{"medication_id": h.medication(a.doctor, "Metformin"), "dose": "1", "frequency": "once_daily"}},
		{"POST", "/api/v1/regimens/" + regB + "/status", a.doctor, map[string]any{"status": "paused", "reason": "r"}},
		{"POST", "/api/v1/regimens/" + regB + "/doses", a.nurse, map[string]any{"times": []string{now.Format(time.RFC3339)}}},
		{"GET", "/api/v1/patients/" + pb + "/doses", a.nurse, nil},
		{"POST", "/api/v1/doses/" + doseB + "/response", a.nurse, map[string]any{"status": "taken"}},
		{"GET", "/api/v1/patients/" + pb + "/encounters", a.doctor, nil},
		{"POST", "/api/v1/patients/" + pb + "/encounters", a.doctor, map[string]any{}},
		{"GET", "/api/v1/encounters/" + encB, a.doctor, nil},
		{"PATCH", "/api/v1/encounters/" + draftEnc, a.doctor, map[string]any{"assessment": "x"}},
		{"POST", "/api/v1/encounters/" + draftEnc + "/sign", a.doctor, nil},
		{"POST", "/api/v1/encounters/" + encB + "/amendments", a.doctor, map[string]any{"field": "plan", "new_value": "x", "reason": "r"}},
		{"POST", "/api/v1/encounters/" + draftEnc + "/draft-note", a.doctor, map[string]any{"dictation": "d"}},
		{"GET", "/api/v1/encounters/" + draftEnc + "/drafts", a.doctor, nil},
		{"POST", "/api/v1/encounters/" + draftEnc + "/drafts/" + draftB + "/flag", a.doctor, map[string]any{"reason": "r"}},
		{"POST", "/api/v1/patients/" + pb + "/prescriptions", a.doctor, map[string]any{"items": []any{map[string]any{"regimen_id": regB, "quantity": "1"}}}},
		{"GET", "/api/v1/patients/" + pb + "/prescriptions", a.doctor, nil},
		{"GET", "/api/v1/prescriptions/" + rxB, a.doctor, nil},
		{"POST", "/api/v1/prescriptions/" + rxB + "/repeat", a.doctor, nil},
		{"POST", "/api/v1/prescriptions/" + rxB + "/cancel", a.doctor, map[string]any{"reason": "r"}},
		{"POST", "/api/v1/patients/" + pb + "/lab-results", a.nurse, map[string]any{"lab_name": "L", "panel": "P", "items": []any{map[string]any{"analyte": "a", "value": "1"}}}},
		{"GET", "/api/v1/patients/" + pb + "/lab-results", a.nurse, nil},
		{"GET", "/api/v1/lab-results/" + labB, a.nurse, nil},
		{"POST", "/api/v1/lab-results/" + labB + "/acknowledge", a.nurse, nil},
		{"POST", "/api/v1/patients/" + pb + "/referrals", a.nurse, map[string]any{"to_name": "Dr X", "reason": "r"}},
		{"POST", "/api/v1/patients/" + pb + "/sick-notes", a.nurse, map[string]any{"basis": "examination", "examined_on": model.DateOf(now).String(), "valid_from": model.DateOf(now).String(), "valid_to": model.DateOf(now).String()}},
		{"GET", "/api/v1/patients/" + pb + "/documents", a.nurse, nil},
		{"GET", "/api/v1/documents/" + docB, a.nurse, nil},
		{"POST", "/api/v1/documents/" + docB + "/issue", a.doctor, nil},
		{"POST", "/api/v1/documents/" + docB + "/cancel", a.nurse, nil},
	}
	for _, ck := range checks {
		if got := h.do(ck.method, ck.path, ck.token, ck.body); got.status != 404 {
			t.Errorf("%s %s from another practice: %d %v", ck.method, ck.path, got.status, got.body)
		}
	}
	if fake.callCount() != callsBefore {
		t.Error("another practice's dictation reached the model")
	}
	inbox := h.expect(h.do("GET", "/api/v1/lab-results", a.doctor, nil), 200, "a inbox").body["items"].([]any)
	if len(inbox) != 0 {
		t.Errorf("practice A's inbox shows %d of practice B's results", len(inbox))
	}
	// And B's records are unchanged.
	rx := h.expect(h.do("GET", "/api/v1/prescriptions/"+rxB, b.doctor, nil), 200, "b rx")
	if rx.str("status") != "active" || rx.body["repeats_used"].(float64) != 0 {
		t.Errorf("B's prescription changed: %v", rx.body)
	}
	lab := h.expect(h.do("GET", "/api/v1/lab-results/"+labB, b.nurse, nil), 200, "b lab")
	if lab.body["acknowledged_at"] != nil {
		t.Errorf("B's result was acknowledged from A: %v", lab.body)
	}
}
