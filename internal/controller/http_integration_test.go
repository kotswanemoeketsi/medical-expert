//go:build integration

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/medical-expert-system/internal/db"
	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/secure"
	"github.com/yourorg/medical-expert-system/internal/service"
)

// Full HTTP paths against a real database. Run with:
//   MES_TEST_DSN='mes:secret@tcp(127.0.0.1:3306)/mes_test' go test -tags=integration ./...

const adminPassword = "kettle-orbit-mango-42"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type harness struct {
	t     *testing.T
	srv   *httptest.Server
	reg   *service.Registry
	clock *clock
}

type harnessOpts struct {
	requireMFA     bool
	loginPerMinute int
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	dsn := os.Getenv("MES_TEST_DSN")
	if dsn == "" {
		t.Skip("MES_TEST_DSN not set")
	}
	ctx := context.Background()
	conn, err := db.Open(ctx, dsn, db.Options{MaxOpen: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := db.Migrate(ctx, conn, nil); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, err := service.NewRegistry(service.Deps{
		DB: conn, Logger: quiet, Hasher: &secure.PBKDF2Hasher{Iterations: 1000},
		Config: service.Config{RequireMFA: o.requireMFA}, Now: clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.loginPerMinute == 0 {
		o.loginPerMinute = 10_000
	}
	api := New(reg, quiet, Options{LoginPerMinute: o.loginPerMinute})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, reg: reg, clock: clk}
}

type resp struct {
	status int
	body   map[string]any
	header http.Header
}

func (r resp) errCode() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func (r resp) str(key string) string { s, _ := r.body[key].(string); return s }

func (h *harness) do(method, path, token string, body any) resp {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, header: res.Header, body: map[string]any{}}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			h.t.Fatalf("%s %s: response is not a JSON object: %s", method, path, raw)
		}
	}
	return out
}

func (h *harness) expect(r resp, status int, what string) resp {
	h.t.Helper()
	if r.status != status {
		h.t.Fatalf("%s: status %d, want %d; body %v", what, r.status, status, r.body)
	}
	return r
}

type practice struct {
	slug       string
	adminEmail string
	adminToken string
}

// newPractice provisions a practice and signs its administrator in. With
// MFA required the administrator's session is left in the setup state.
func (h *harness) newPractice() practice {
	h.t.Helper()
	slug := "p-" + strings.ToLower(model.MustNewID().String()[:10])
	res, err := h.reg.Provision(context.Background(), service.ProvisionInput{
		Slug: slug, Name: "Clinic " + slug, AdminEmail: "admin@" + slug + ".test", AdminName: "Ada Admin",
		AdminPassword: adminPassword,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	p := practice{slug: slug, adminEmail: res.Admin.Email}
	r := h.login(slug, p.adminEmail, adminPassword)
	p.adminToken = r.str("token")
	return p
}

func (h *harness) login(slug, email, password string) resp {
	h.t.Helper()
	return h.do("POST", "/api/v1/auth/login", "", map[string]string{
		"tenant_slug": slug, "email": email, "password": password,
	})
}

func (h *harness) mustLogin(slug, email, password string) string {
	h.t.Helper()
	r := h.expect(h.login(slug, email, password), 200, "login "+email)
	return r.str("token")
}

// createStaff creates a user through the API and returns its id.
func (h *harness) createStaff(p practice, role model.Role) (id, email string) {
	h.t.Helper()
	email = fmt.Sprintf("%s-%s@%s.test", role, strings.ToLower(model.MustNewID().String()[:6]), p.slug)
	body := map[string]any{"email": email, "full_name": "Staff " + string(role), "role": role, "password": adminPassword}
	if role.IsClinical() {
		body["professional_reg_no"] = "MP0654321"
	}
	r := h.expect(h.do("POST", "/api/v1/users", p.adminToken, body), 201, "create "+string(role))
	return r.str("id"), email
}

func (h *harness) createPatient(token string, body map[string]any) resp {
	h.t.Helper()
	return h.do("POST", "/api/v1/patients", token, body)
}

func patientBody(first, last, dob string) map[string]any {
	return map[string]any{"first_name": first, "last_name": last, "date_of_birth": dob, "sex": "female"}
}

// code returns the TOTP code for the step `offset` steps from now.
func (h *harness) code(secret string, offset int) string {
	h.t.Helper()
	c, err := secure.TOTPCode(secret, uint64(int64(secure.CounterAt(h.clock.Now()))+int64(offset)))
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

// wrongCode returns a six-digit code valid for no step in the window.
func (h *harness) wrongCode(secret string) string {
	valid := map[string]bool{h.code(secret, -1): true, h.code(secret, 0): true, h.code(secret, 1): true}
	for i := 0; ; i++ {
		c := fmt.Sprintf("%06d", i)
		if !valid[c] {
			return c
		}
	}
}

// enrol takes a session in the setup state through enrolment and returns
// the secret and the recovery codes.
func (h *harness) enrol(token string) (secret string, recovery []string) {
	h.t.Helper()
	r := h.expect(h.do("POST", "/api/v1/auth/mfa/enrol", token, nil), 200, "enrol")
	secret = r.str("secret")
	r = h.expect(h.do("POST", "/api/v1/auth/mfa/enrol/confirm", token, map[string]string{"code": h.code(secret, 0)}), 200, "confirm")
	for _, c := range r.body["recovery_codes"].([]any) {
		recovery = append(recovery, c.(string))
	}
	return secret, recovery
}

// ---------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	r := h.expect(h.do("GET", "/healthz", "", nil), 200, "healthz")
	if r.str("status") != "ok" {
		t.Fatalf("unexpected body %v", r.body)
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	_, doctorEmail := h.createStaff(p, model.RoleDoctor)
	suspendedID, suspendedEmail := h.createStaff(p, model.RoleNurse)
	h.expect(h.do("PATCH", "/api/v1/users/"+suspendedID+"/status", p.adminToken,
		map[string]string{"status": "suspended", "reason": "left the practice"}), 200, "suspend")

	attempts := map[string]resp{
		"unknown practice": h.login("no-such-practice", doctorEmail, adminPassword),
		"unknown email":    h.login(p.slug, "nobody@"+p.slug+".test", adminPassword),
		"wrong password":   h.login(p.slug, doctorEmail, "not-the-right-one-99"),
		"suspended":        h.login(p.slug, suspendedEmail, adminPassword),
		"empty":            h.login("", "", ""),
	}
	var first map[string]any
	for name, r := range attempts {
		if r.status != 401 {
			t.Errorf("%s: status %d", name, r.status)
		}
		e := r.body["error"].(map[string]any)
		delete(e, "request_id")
		if first == nil {
			first = e
			continue
		}
		if fmt.Sprint(e) != fmt.Sprint(first) {
			t.Errorf("%s: response differs from the others:\n%v\n%v", name, e, first)
		}
	}
}

func TestLockoutAfterRepeatedFailures(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	_, email := h.createStaff(p, model.RoleNurse)
	for i := 0; i < 5; i++ {
		h.expect(h.login(p.slug, email, "wrong-password-attempt"), 401, "bad attempt")
	}
	h.expect(h.login(p.slug, email, adminPassword), 401, "correct password while locked")
	h.clock.Advance(16 * time.Minute)
	h.expect(h.login(p.slug, email, adminPassword), 200, "after the lock lapses")

	// An administrator can unlock without waiting.
	id, email2 := h.createStaff(p, model.RoleDoctor)
	for i := 0; i < 5; i++ {
		h.expect(h.login(p.slug, email2, "wrong-password-attempt"), 401, "bad attempt")
	}
	list := h.expect(h.do("GET", "/api/v1/users", p.adminToken, nil), 200, "list")
	for _, it := range list.body["items"].([]any) {
		if u := it.(map[string]any); u["id"] == id && u["locked"] != true {
			t.Fatalf("locked account not shown as locked: %v", u)
		}
	}
	h.expect(h.do("PATCH", "/api/v1/users/"+id+"/status", p.adminToken, map[string]string{"status": "active"}), 200, "unlock")
	h.expect(h.login(p.slug, email2, adminPassword), 200, "after an administrator unlocks")

	r := h.expect(h.do("GET", "/api/v1/audit?action=auth.account.locked", p.adminToken, nil), 200, "audit")
	if items := r.body["items"].([]any); len(items) != 2 {
		t.Fatalf("expected a lock event per locked account in the audit trail, got %d", len(items))
	}
}

func TestMFAEnrolmentAndSignIn(t *testing.T) {
	h := newHarness(t, harnessOpts{requireMFA: true})
	p := h.newPractice()

	r := h.login(p.slug, p.adminEmail, adminPassword)
	h.expect(r, 200, "password login")
	if r.str("state") != "setup" || r.body["mfa_setup_required"] != true {
		t.Fatalf("an administrator without MFA should be in setup state: %v", r.body)
	}
	setup := r.str("token")
	if got := h.do("GET", "/api/v1/patients", setup, nil); got.status != 403 || got.errCode() != "mfa_setup_required" {
		t.Fatalf("setup session reached patient data: %d %v", got.status, got.body)
	}
	me := h.expect(h.do("GET", "/api/v1/me", setup, nil), 200, "me in setup")
	if me.body["session"].(map[string]any)["state"] != "setup" {
		t.Fatalf("me: %v", me.body)
	}

	en := h.expect(h.do("POST", "/api/v1/auth/mfa/enrol", setup, nil), 200, "enrol")
	secret := en.str("secret")
	if !strings.HasPrefix(en.str("otpauth_uri"), "otpauth://totp/") {
		t.Fatalf("bad provisioning URI: %v", en.body)
	}
	if got := h.do("POST", "/api/v1/auth/mfa/enrol/confirm", setup, map[string]string{"code": h.wrongCode(secret)}); got.errCode() != "invalid_code" {
		t.Fatalf("wrong code accepted at enrolment: %v", got.body)
	}
	enrolCode := h.code(secret, 0)
	cf := h.expect(h.do("POST", "/api/v1/auth/mfa/enrol/confirm", setup, map[string]string{"code": enrolCode}), 200, "confirm")
	if n := len(cf.body["recovery_codes"].([]any)); n != 10 {
		t.Fatalf("expected 10 recovery codes, got %d", n)
	}
	h.expect(h.do("GET", "/api/v1/patients", setup, nil), 200, "same session after enrolment")
	h.expect(h.do("POST", "/api/v1/auth/logout", setup, nil), 204, "logout")
	h.expect(h.do("GET", "/api/v1/me", setup, nil), 401, "after logout")

	r = h.expect(h.login(p.slug, p.adminEmail, adminPassword), 200, "second login")
	if r.str("state") != "pending" || r.body["mfa_required"] != true || r.body["user"] != nil {
		t.Fatalf("expected a pending session with no user details: %v", r.body)
	}
	pending := r.str("token")
	if got := h.do("GET", "/api/v1/me", pending, nil); got.errCode() != "mfa_required" {
		t.Fatalf("pending session reached /me: %v", got.body)
	}
	// The enrolment code is spent, even though its thirty seconds have not
	// passed.
	if got := h.do("POST", "/api/v1/auth/mfa/verify", pending, map[string]string{"code": enrolCode}); got.errCode() != "invalid_code" {
		t.Fatalf("replayed enrolment code accepted: %v", got.body)
	}
	v := h.expect(h.do("POST", "/api/v1/auth/mfa/verify", pending, map[string]string{"code": h.code(secret, 1)}), 200, "verify")
	full := v.str("token")
	if full == "" || full == pending || v.str("state") != "active" {
		t.Fatalf("verify must issue a new full token: %v", v.body)
	}
	h.expect(h.do("GET", "/api/v1/me", pending, nil), 401, "pending token after exchange")
	h.expect(h.do("GET", "/api/v1/patients", full, nil), 200, "full session")
	st := h.expect(h.do("GET", "/api/v1/auth/mfa/status", full, nil), 200, "status")
	if st.body["enabled"] != true || st.body["recovery_codes_remaining"].(float64) != 10 {
		t.Fatalf("status: %v", st.body)
	}
}

// With MFA on, a password alone must reach nothing but the verify and
// logout endpoints — on every route the API serves, read or write.
func TestPasswordAloneIsNotEnoughOnceMFAIsOn(t *testing.T) {
	h := newHarness(t, harnessOpts{requireMFA: true})
	p := h.newPractice()
	h.enrol(p.adminToken)
	pending := h.expect(h.login(p.slug, p.adminEmail, adminPassword), 200, "login").str("token")

	api := New(nil, nil, Options{})
	someID := model.MustNewID().String()
	for _, rt := range api.Routes() {
		key := rt.Method + " " + rt.Path
		if rt.Public || pendingAllowed[key] {
			continue
		}
		path := rt.Path
		for _, param := range []string{"{id}", "{allergyId}", "{conditionId}"} {
			path = strings.ReplaceAll(path, param, someID)
		}
		var body any
		if rt.Method != "GET" {
			body = map[string]any{}
		}
		got := h.do(rt.Method, path, pending, body)
		if got.status != 403 || got.errCode() != "mfa_required" {
			t.Errorf("%s with a password-only session: %d %v", key, got.status, got.body)
		}
	}
}

func TestRecoveryCodeWorksOnce(t *testing.T) {
	h := newHarness(t, harnessOpts{requireMFA: true})
	p := h.newPractice()
	_, codes := h.enrol(p.adminToken)

	pending := h.mustLogin(p.slug, p.adminEmail, adminPassword)
	typed := strings.ToLower(strings.ReplaceAll(codes[0], "-", " "))
	h.expect(h.do("POST", "/api/v1/auth/mfa/verify", pending, map[string]string{"recovery_code": typed}), 200, "recovery code")

	pending = h.mustLogin(p.slug, p.adminEmail, adminPassword)
	if got := h.do("POST", "/api/v1/auth/mfa/verify", pending, map[string]string{"recovery_code": codes[0]}); got.errCode() != "invalid_code" {
		t.Fatalf("recovery code accepted twice: %v", got.body)
	}
}

func TestSecondFactorFailuresCountTowardsLockout(t *testing.T) {
	h := newHarness(t, harnessOpts{requireMFA: true})
	p := h.newPractice()
	secret, _ := h.enrol(p.adminToken)

	// Someone holding the password but not the phone: three guesses, then a
	// fresh sign-in to try for a fresh count, then two more. The password
	// stage must not reset the count, so the fifth guess locks the account.
	pending := h.mustLogin(p.slug, p.adminEmail, adminPassword)
	for i := 0; i < 3; i++ {
		h.expect(h.do("POST", "/api/v1/auth/mfa/verify", pending, map[string]string{"code": h.wrongCode(secret)}), 401, "wrong code")
	}
	pending = h.mustLogin(p.slug, p.adminEmail, adminPassword)
	for i := 0; i < 2; i++ {
		h.expect(h.do("POST", "/api/v1/auth/mfa/verify", pending, map[string]string{"code": h.wrongCode(secret)}), 401, "wrong code")
	}
	// Locked: the pending session is gone and the password alone no longer
	// opens a new one.
	h.expect(h.do("POST", "/api/v1/auth/mfa/verify", pending, map[string]string{"code": h.code(secret, 0)}), 401, "after lock")
	h.expect(h.login(p.slug, p.adminEmail, adminPassword), 401, "login while locked")
}

func TestAdminMFAResetRevokesSessions(t *testing.T) {
	h := newHarness(t, harnessOpts{requireMFA: true})
	p := h.newPractice()
	h.enrol(p.adminToken)
	doctorID, doctorEmail := h.createStaff(p, model.RoleDoctor)

	doctor := h.mustLogin(p.slug, doctorEmail, adminPassword)
	h.enrol(doctor)
	h.expect(h.do("GET", "/api/v1/patients", doctor, nil), 200, "doctor active")

	if got := h.do("POST", "/api/v1/users/"+doctorID+"/mfa/reset", p.adminToken, map[string]string{}); got.status != 422 {
		t.Fatalf("reset without a reason: %d", got.status)
	}
	h.expect(h.do("POST", "/api/v1/users/"+doctorID+"/mfa/reset", p.adminToken,
		map[string]string{"reason": "lost phone"}), 204, "reset")
	h.expect(h.do("GET", "/api/v1/patients", doctor, nil), 401, "doctor's old session")

	r := h.expect(h.login(p.slug, doctorEmail, adminPassword), 200, "login after reset")
	if r.str("state") != "setup" {
		t.Fatalf("after a reset the doctor must enrol again, got %v", r.body)
	}
}

func TestSuspensionRevokesImmediately(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	id, email := h.createStaff(p, model.RoleDoctor)
	token := h.mustLogin(p.slug, email, adminPassword)
	h.expect(h.do("GET", "/api/v1/patients", token, nil), 200, "before")

	h.expect(h.do("PATCH", "/api/v1/users/"+id+"/status", p.adminToken, map[string]string{"status": "suspended"}), 422, "no reason")
	h.expect(h.do("PATCH", "/api/v1/users/"+id+"/status", p.adminToken,
		map[string]string{"status": "suspended", "reason": "investigation"}), 200, "suspend")
	h.expect(h.do("GET", "/api/v1/patients", token, nil), 401, "after suspension")

	h.expect(h.do("PATCH", "/api/v1/users/"+id+"/status", p.adminToken, map[string]string{"status": "active"}), 200, "reactivate")
	h.expect(h.do("GET", "/api/v1/patients", token, nil), 401, "a revoked session stays revoked")
	h.mustLogin(p.slug, email, adminPassword)

	users := h.expect(h.do("GET", "/api/v1/users", p.adminToken, nil), 200, "list")
	for _, it := range users.body["items"].([]any) {
		u := it.(map[string]any)
		if _, ok := u["status"]; !ok {
			t.Fatal("user listing omits status")
		}
		if _, ok := u["mfa_enabled"]; !ok {
			t.Fatal("user listing omits mfa_enabled")
		}
		if _, leaked := u["password_hash"]; leaked {
			t.Fatal("user listing leaks the password hash")
		}
	}
}

func TestAdminCannotActOnOwnAccount(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	me := h.expect(h.do("GET", "/api/v1/me", p.adminToken, nil), 200, "me")
	myID := me.body["user"].(map[string]any)["id"].(string)
	if got := h.do("PATCH", "/api/v1/users/"+myID+"/status", p.adminToken,
		map[string]string{"status": "suspended", "reason": "oops"}); got.errCode() != "self_action" {
		t.Fatalf("self-suspension: %v", got.body)
	}
	if got := h.do("POST", "/api/v1/users/"+myID+"/mfa/reset", p.adminToken,
		map[string]string{"reason": "x"}); got.errCode() != "self_action" {
		t.Fatalf("self MFA reset: %v", got.body)
	}
}

func TestClinicalStaffNeedRegistrationNumbers(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	r := h.do("POST", "/api/v1/users", p.adminToken, map[string]any{
		"email": "dr@" + p.slug + ".test", "full_name": "Dr No Number", "role": "doctor", "password": adminPassword,
	})
	if r.status != 422 || !strings.Contains(fmt.Sprint(r.body), "professional_reg_no") {
		t.Fatalf("doctor without registration number: %d %v", r.status, r.body)
	}
	_, email := h.createStaff(p, model.RoleNurse)
	r = h.do("POST", "/api/v1/users", p.adminToken, map[string]any{
		"email": strings.ToUpper(email), "full_name": "Dup", "role": "nurse", "professional_reg_no": "X1", "password": adminPassword,
	})
	if r.errCode() != "email_taken" {
		t.Fatalf("duplicate email: %v", r.body)
	}
}

func TestPatientCaptureAndDuplicatePrompt(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	_, nurseEmail := h.createStaff(p, model.RoleNurse)
	nurse := h.mustLogin(p.slug, nurseEmail, adminPassword)

	body := patientBody("Thabo", "Nkosi", "1980-01-01")
	body["national_id"] = "8001015009087"
	body["mobile"] = "082 123 4567"
	body["preferred_channel"] = "whatsapp"
	first := h.expect(h.createPatient(nurse, body), 201, "create")
	if first.str("mobile") != "+27821234567" || first.str("id_type") != "sa_id" {
		t.Fatalf("not normalised: %v", first.body)
	}

	// Same name and date of birth, no ID number: a prompt, not a block.
	twin := patientBody("THABO", "nkosi", "1980-01-01")
	r := h.expect(h.createPatient(nurse, twin), 409, "possible duplicate")
	e := r.body["error"].(map[string]any)
	if e["code"] != "possible_duplicate" || e["overridable"] != true || len(e["candidates"].([]any)) != 1 {
		t.Fatalf("unexpected duplicate response: %v", e)
	}
	twin["confirm_not_duplicate"] = true
	h.expect(h.createPatient(nurse, twin), 201, "confirmed not a duplicate")

	// The same ID number is the same person and cannot be confirmed past.
	same := patientBody("Someone", "Else", "1980-01-01")
	same["national_id"] = "8001015009087"
	same["confirm_not_duplicate"] = true
	r = h.expect(h.createPatient(nurse, same), 409, "same national id")
	if r.errCode() != "national_id_exists" || r.body["error"].(map[string]any)["overridable"] != false {
		t.Fatalf("national ID collision: %v", r.body)
	}
}

func TestPatientValidation(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	body := patientBody("Anele", "Zulu", "1990-05-05")
	body["preferred_channel"] = "whatsapp"
	r := h.expect(h.createPatient(p.adminToken, body), 422, "whatsapp without mobile")
	fields := fmt.Sprint(r.body["error"].(map[string]any)["fields"])
	if !strings.Contains(fields, "mobile") {
		t.Fatalf("expected a mobile field error, got %s", fields)
	}
	body = patientBody("Anele", "Zulu", "1990-05-05")
	body["alergies"] = "penicillin"
	if got := h.createPatient(p.adminToken, body); got.status != 400 || !strings.Contains(fmt.Sprint(got.body), "alergies") {
		t.Fatalf("a misspelt field was silently ignored: %d %v", got.status, got.body)
	}
}

func TestPatientRoleSeesOnlyTheirOwnRecord(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	mine := h.expect(h.createPatient(p.adminToken, patientBody("Nomsa", "Mahlangu", "1975-03-03")), 201, "p1").str("id")
	other := h.expect(h.createPatient(p.adminToken, patientBody("Sipho", "Khumalo", "1968-08-08")), 201, "p2").str("id")
	email := "nomsa@" + p.slug + ".test"
	h.expect(h.do("POST", "/api/v1/users", p.adminToken, map[string]any{
		"email": email, "full_name": "Nomsa Mahlangu", "role": "patient", "patient_id": mine, "password": adminPassword,
	}), 201, "patient login")
	if got := h.do("POST", "/api/v1/users", p.adminToken, map[string]any{
		"email": "second@" + p.slug + ".test", "full_name": "Second", "role": "patient", "patient_id": mine, "password": adminPassword,
	}); got.errCode() != "patient_has_login" {
		t.Fatalf("second login for one patient: %v", got.body)
	}

	pt := h.mustLogin(p.slug, email, adminPassword)
	h.expect(h.do("GET", "/api/v1/patients/"+mine, pt, nil), 200, "own record")
	h.expect(h.do("GET", "/api/v1/patients/"+mine+"/summary", pt, nil), 200, "own summary")
	h.expect(h.do("GET", "/api/v1/patients/"+mine+"/consents", pt, nil), 200, "own consents")
	// Someone else's record is not found — not forbidden, which would
	// confirm it exists.
	h.expect(h.do("GET", "/api/v1/patients/"+other, pt, nil), 404, "another patient")
	h.expect(h.do("GET", "/api/v1/patients/"+other+"/summary", pt, nil), 404, "another summary")
	h.expect(h.do("GET", "/api/v1/patients/"+other+"/consents", pt, nil), 404, "another's consents")
	h.expect(h.do("GET", "/api/v1/patients", pt, nil), 403, "search")
	h.expect(h.do("PUT", "/api/v1/patients/"+mine, pt, patientBody("Nomsa", "Mahlangu", "1975-03-03")), 403, "edit")

	r := h.expect(h.do("GET", "/api/v1/audit?action=patient.access_denied", p.adminToken, nil), 200, "audit")
	if len(r.body["items"].([]any)) < 2 {
		t.Fatalf("denied attempts should be audited: %v", r.body)
	}
}

// A valid session and a valid record ID from another practice must answer
// "not found" on every read and write path.
func TestCrossTenantAccessIsNotFound(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	a := h.newPractice()
	b := h.newPractice()
	_, docEmail := h.createStaff(a, model.RoleDoctor)
	doctorA := h.mustLogin(a.slug, docEmail, adminPassword)
	_, docBEmail := h.createStaff(b, model.RoleDoctor)
	doctorB := h.mustLogin(b.slug, docBEmail, adminPassword)
	bUserID, _ := h.createStaff(b, model.RoleNurse)

	pb := h.expect(h.createPatient(b.adminToken, patientBody("Lindiwe", "Sithole", "1982-02-02")), 201, "b patient").str("id")
	allergy := h.expect(h.do("POST", "/api/v1/patients/"+pb+"/allergies", doctorB,
		map[string]any{"substance": "Penicillin", "severity": "severe"}), 201, "b allergy").str("id")
	condition := h.expect(h.do("POST", "/api/v1/patients/"+pb+"/conditions", doctorB,
		map[string]any{"description": "Hypertension", "icd10_code": "I10", "is_chronic": true}), 201, "b condition").str("id")

	checks := []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/patients/" + pb, nil},
		{"GET", "/api/v1/patients/" + pb + "/summary", nil},
		{"GET", "/api/v1/patients/" + pb + "/consents", nil},
		{"PUT", "/api/v1/patients/" + pb, patientBody("Hijack", "Attempt", "1982-02-02")},
		{"POST", "/api/v1/patients/" + pb + "/allergies", map[string]any{"substance": "Latex"}},
		{"PATCH", "/api/v1/patients/" + pb + "/allergies/" + allergy, map[string]any{"status": "entered_in_error"}},
		{"POST", "/api/v1/patients/" + pb + "/conditions", map[string]any{"description": "Asthma"}},
		{"PATCH", "/api/v1/patients/" + pb + "/conditions/" + condition, map[string]any{"status": "resolved", "is_chronic": false}},
		{"POST", "/api/v1/patients/" + pb + "/consents", map[string]any{"purpose": "ai_assisted_review", "granted": true, "method": "verbal"}},
		{"PATCH", "/api/v1/patients/" + pb + "/review-interval", map[string]any{"review_interval_months": 3}},
	}
	for _, c := range checks {
		if got := h.do(c.method, c.path, doctorA, c.body); got.status != 404 {
			t.Errorf("%s %s from another practice: %d %v", c.method, c.path, got.status, got.body)
		}
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PATCH", "/api/v1/users/" + bUserID + "/status", map[string]any{"status": "suspended", "reason": "x"}},
		{"POST", "/api/v1/users/" + bUserID + "/mfa/reset", map[string]any{"reason": "x"}},
	} {
		if got := h.do(c.method, c.path, a.adminToken, c.body); got.status != 404 {
			t.Errorf("%s %s from another practice's admin: %d %v", c.method, c.path, got.status, got.body)
		}
	}
	search := h.expect(h.do("GET", "/api/v1/patients?q=Sithole", doctorA, nil), 200, "search")
	if n := len(search.body["items"].([]any)); n != 0 {
		t.Errorf("search in A found %d of B's patients", n)
	}
	// And B's data is untouched.
	sum := h.expect(h.do("GET", "/api/v1/patients/"+pb+"/summary", doctorB, nil), 200, "b summary")
	if sum.body["patient"].(map[string]any)["first_name"] != "Lindiwe" || len(sum.body["allergies"].([]any)) != 1 {
		t.Errorf("B's record changed: %v", sum.body)
	}
}

func TestConsentLifecycle(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	id := h.expect(h.createPatient(p.adminToken, patientBody("Palesa", "Mokoena", "1995-09-09")), 201, "patient").str("id")

	r := h.expect(h.do("GET", "/api/v1/patients/"+id+"/consents", p.adminToken, nil), 200, "empty")
	for _, c := range r.body["current"].([]any) {
		m := c.(map[string]any)
		if m["granted"] != false || m["recorded"] != false {
			t.Fatalf("no record must read as not granted: %v", m)
		}
	}
	for _, granted := range []bool{true, false} {
		h.expect(h.do("POST", "/api/v1/patients/"+id+"/consents", p.adminToken,
			map[string]any{"purpose": "ai_assisted_review", "granted": granted, "method": "written"}), 201, "record")
		h.clock.Advance(time.Second)
	}
	if got := h.do("POST", "/api/v1/patients/"+id+"/consents", p.adminToken,
		map[string]any{"purpose": "ai_assisted_review", "method": "written"}); got.status != 422 {
		t.Fatalf("a decision without granted must be refused, got %d", got.status)
	}
	r = h.expect(h.do("GET", "/api/v1/patients/"+id+"/consents", p.adminToken, nil), 200, "after")
	if n := len(r.body["history"].([]any)); n != 2 {
		t.Fatalf("history should keep both decisions, got %d", n)
	}
	for _, c := range r.body["current"].([]any) {
		m := c.(map[string]any)
		if m["purpose"] == "ai_assisted_review" && (m["granted"] != false || m["recorded"] != true) {
			t.Fatalf("latest decision was a withdrawal: %v", m)
		}
	}
}

func TestClinicalDetailIsForClinicians(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	_, docEmail := h.createStaff(p, model.RoleDoctor)
	doc := h.mustLogin(p.slug, docEmail, adminPassword)
	id := h.expect(h.createPatient(p.adminToken, patientBody("Bongani", "Ndlovu", "1960-12-12")), 201, "patient").str("id")

	h.expect(h.do("GET", "/api/v1/patients/"+id+"/summary", p.adminToken, nil), 403, "admin summary")
	h.expect(h.do("POST", "/api/v1/patients/"+id+"/allergies", p.adminToken, map[string]any{"substance": "x"}), 403, "admin allergy")

	al := h.expect(h.do("POST", "/api/v1/patients/"+id+"/allergies", doc,
		map[string]any{"substance": "Penicillin", "reaction": "Anaphylaxis", "severity": "life_threatening"}), 201, "allergy").str("id")
	cond := h.expect(h.do("POST", "/api/v1/patients/"+id+"/conditions", doc,
		map[string]any{"description": "Type 2 diabetes", "icd10_code": "e11.9", "is_chronic": true}), 201, "condition")
	if cond.str("icd10_code") != "E11.9" {
		t.Fatalf("ICD-10 not normalised: %v", cond.body)
	}
	h.expect(h.do("PATCH", "/api/v1/patients/"+id+"/conditions/"+cond.str("id"), doc, map[string]any{"status": "resolved"}), 422, "without is_chronic")
	h.expect(h.do("PATCH", "/api/v1/patients/"+id+"/allergies/"+al, doc, map[string]any{"status": "entered_in_error"}), 204, "withdraw")

	sum := h.expect(h.do("GET", "/api/v1/patients/"+id+"/summary", doc, nil), 200, "summary")
	if n := len(sum.body["allergies"].([]any)); n != 0 {
		t.Fatalf("an allergy entered in error still shows: %d", n)
	}
	if n := len(sum.body["conditions"].([]any)); n != 1 {
		t.Fatalf("expected the condition, got %d", n)
	}
	h.expect(h.do("PATCH", "/api/v1/patients/"+id+"/review-interval", doc, map[string]any{"review_interval_months": 6}), 204, "interval")
	h.expect(h.do("PATCH", "/api/v1/patients/"+id+"/review-interval", doc, map[string]any{"review_interval_months": 99}), 422, "bad interval")
}

func TestPatientReadsAreAudited(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	docID, docEmail := h.createStaff(p, model.RoleDoctor)
	doc := h.mustLogin(p.slug, docEmail, adminPassword)
	id := h.expect(h.createPatient(p.adminToken, patientBody("Kagiso", "Molefe", "1988-04-04")), 201, "patient").str("id")
	h.expect(h.do("GET", "/api/v1/patients/"+id, doc, nil), 200, "read")

	r := h.expect(h.do("GET", "/api/v1/audit?subject_id="+id, p.adminToken, nil), 200, "audit")
	found := false
	for _, it := range r.body["items"].([]any) {
		e := it.(map[string]any)
		if e["action"] == "patient.read" && e["actor_id"] == docID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the doctor's read is not in the audit trail: %v", r.body)
	}
	// Staff other than administrators cannot read the trail.
	h.expect(h.do("GET", "/api/v1/audit", doc, nil), 403, "doctor reading audit")
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	p := h.newPractice()
	_, email := h.createStaff(p, model.RoleNurse)
	s1 := h.mustLogin(p.slug, email, adminPassword)
	s2 := h.mustLogin(p.slug, email, adminPassword)

	h.expect(h.do("POST", "/api/v1/auth/password", s1, map[string]string{
		"current_password": "wrong-current-password", "new_password": "violet-harbour-tram-8"}), 422, "wrong current")
	h.expect(h.do("POST", "/api/v1/auth/password", s1, map[string]string{
		"current_password": adminPassword, "new_password": "short"}), 422, "weak new")
	h.expect(h.do("POST", "/api/v1/auth/password", s1, map[string]string{
		"current_password": adminPassword, "new_password": "violet-harbour-tram-8"}), 204, "change")
	h.expect(h.do("GET", "/api/v1/me", s1, nil), 200, "current session survives")
	h.expect(h.do("GET", "/api/v1/me", s2, nil), 401, "other session revoked")
	h.expect(h.login(p.slug, email, adminPassword), 401, "old password")
	h.mustLogin(p.slug, email, "violet-harbour-tram-8")
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	for _, r := range []resp{
		h.do("GET", "/healthz", "", nil),
		h.do("GET", "/api/v1/me", "", nil),
		h.do("GET", "/api/v1/nope", "", nil),
		h.login("x", "y", "z"),
	} {
		if r.header.Get("Cache-Control") != "no-store" || r.header.Get("X-Content-Type-Options") != "nosniff" ||
			r.header.Get("X-Request-ID") == "" {
			t.Errorf("status %d missing security headers: %v", r.status, r.header)
		}
	}
}

func TestLoginIsRateLimitedPerAddress(t *testing.T) {
	h := newHarness(t, harnessOpts{loginPerMinute: 3})
	for i := 0; i < 3; i++ {
		h.login("any", "one@x.test", "password-guess-123")
	}
	r := h.login("any", "one@x.test", "password-guess-123")
	if r.status != 429 || r.header.Get("Retry-After") == "" || r.errCode() != "rate_limited" {
		t.Fatalf("expected 429 with Retry-After, got %d %v", r.status, r.body)
	}
}
