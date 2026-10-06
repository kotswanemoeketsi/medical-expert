package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/service"
)

func TestRateLimiterRefillsOverTime(t *testing.T) {
	l := NewRateLimiter(60, 3) // one per second, bursts of three
	clock := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return clock }
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("10.0.0.1"); !ok {
			t.Fatalf("request %d within the burst refused", i+1)
		}
	}
	ok, wait := l.Allow("10.0.0.1")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("fourth request: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("10.0.0.2"); !ok {
		t.Fatal("a different address shares the first one's bucket")
	}
	clock = clock.Add(time.Second)
	if ok, _ := l.Allow("10.0.0.1"); !ok {
		t.Fatal("a token should have refilled after one second")
	}
}

func TestRateLimiterForgetsIdleAddresses(t *testing.T) {
	l := NewRateLimiter(60, 3)
	clock := time.Now()
	l.now = func() time.Time { return clock }
	l.Allow("a")
	l.Allow("b")
	clock = clock.Add(10 * time.Minute)
	l.Allow("c")
	if len(l.buckets) != 1 {
		t.Fatalf("idle buckets not swept: %d remain", len(l.buckets))
	}
}

func TestClientIPTrustsForwardedForOnlyWhenConfigured(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.10:51234"
	r.Header.Set("X-Forwarded-For", "203.0.113.99, 198.51.100.7")
	if got := clientIP(r, false); got != "192.0.2.10" {
		t.Errorf("untrusted: got %s, want the socket address", got)
	}
	// Right-most entry: the one our own proxy appended. The left-most was
	// supplied by the client and could say anything.
	if got := clientIP(r, true); got != "198.51.100.7" {
		t.Errorf("trusted: got %s, want the right-most entry", got)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := clientIP(r, true); got != "192.0.2.10" {
		t.Errorf("garbage header should fall back to the socket address, got %s", got)
	}
}

func TestMFAGate(t *testing.T) {
	cases := []struct {
		state        service.SessionState
		method, path string
		want         error
	}{
		{service.StatePending, "POST", "/api/v1/auth/mfa/verify", nil},
		{service.StatePending, "POST", "/api/v1/auth/logout", nil},
		{service.StatePending, "GET", "/api/v1/me", service.ErrMFARequired},
		{service.StatePending, "GET", "/api/v1/patients", service.ErrMFARequired},
		{service.StatePending, "POST", "/api/v1/auth/mfa/enrol", service.ErrMFARequired},
		{service.StateSetup, "POST", "/api/v1/auth/mfa/enrol", nil},
		{service.StateSetup, "POST", "/api/v1/auth/mfa/enrol/confirm", nil},
		{service.StateSetup, "GET", "/api/v1/me", nil},
		{service.StateSetup, "GET", "/api/v1/patients", service.ErrMFASetupRequired},
		{service.StateSetup, "POST", "/api/v1/auth/mfa/verify", service.ErrMFASetupRequired},
		{service.StateActive, "GET", "/api/v1/patients", nil},
		{service.StateActive, "POST", "/api/v1/auth/mfa/verify", service.ErrNotPending},
		{service.SessionState("bogus"), "GET", "/api/v1/me", service.ErrUnauthenticated},
	}
	for _, c := range cases {
		if got := mfaGate(c.state, c.method, c.path); !errors.Is(got, c.want) {
			t.Errorf("%s %s %s: got %v, want %v", c.state, c.method, c.path, got, c.want)
		}
	}
}

func TestFailMapsServiceErrors(t *testing.T) {
	a := &API{log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{service.ErrNotFound, 404, "not_found"},
		{service.ErrForbidden, 403, "forbidden"},
		{service.ErrMFARequired, 403, "mfa_required"},
		{service.ErrInvalidCredentials, 401, "invalid_credentials"},
		{service.ErrEmailTaken, 409, "email_taken"},
		{&service.Error{Kind: service.KindInvalid, Code: "validation_failed", Message: "x",
			Fields: []model.FieldError{{Field: "mobile", Message: "is required"}}}, 422, "validation_failed"},
		{&badRequest{"bad"}, 400, "bad_request"},
		{errors.New("database on fire"), 500, "internal_error"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/x", nil).WithContext(context.WithValue(context.Background(), keyRequestID, "req-1"))
		a.fail(rec, req, c.err)
		if rec.Code != c.status {
			t.Errorf("%v: status %d, want %d", c.err, rec.Code, c.status)
		}
		var body struct {
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%v: body is not JSON: %s", c.err, rec.Body.String())
		}
		if body.Error["code"] != c.code || body.Error["request_id"] != "req-1" {
			t.Errorf("%v: body %v", c.err, body.Error)
		}
		if c.status == 500 && strings.Contains(rec.Body.String(), "on fire") {
			t.Error("an internal error leaked its detail to the client")
		}
		if c.status == 401 && rec.Header().Get("WWW-Authenticate") == "" {
			t.Error("401 without WWW-Authenticate")
		}
	}
}

func TestFailMergesDetails(t *testing.T) {
	a := &API{log: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/patients", nil)
	a.fail(rec, req, &service.Error{Kind: service.KindConflict, Code: "possible_duplicate", Message: "m",
		Details: map[string]any{"candidates": []string{"x"}, "code": "must-not-override"}})
	var body struct {
		Error map[string]any `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error["code"] != "possible_duplicate" || body.Error["candidates"] == nil {
		t.Fatalf("details not merged safely: %v", body.Error)
	}
}

func TestDecodeIsStrict(t *testing.T) {
	var dst struct {
		Name string `json:"name"`
	}
	cases := map[string]string{
		`{"name":"a","nmae":"b"}`: "Unknown field",
		`{"name":`:                "not valid JSON",
		``:                        "empty",
		`{"name":1}`:              "wrong type",
		`{"name":"a"}{"x":1}`:     "single JSON object",
	}
	for body, want := range cases {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		err := decode(r, &dst)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("decode(%q) = %v, want mention of %q", body, err, want)
		}
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"a"}`))
	r.Header.Set("Content-Type", "text/plain")
	if err := decode(r, &dst); err == nil {
		t.Error("non-JSON content type accepted")
	}
}

func TestDecodeOptionalAcceptsNoBodyWhateverItsLabel(t *testing.T) {
	var dst struct {
		Reason string `json:"reason"`
	}
	// PowerShell's Invoke-RestMethod sends a bodiless POST as a form.
	r := httptest.NewRequest("POST", "/", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := decodeOptional(r, &dst); err != nil {
		t.Errorf("empty form-labelled body rejected: %v", err)
	}
	// A form that does carry a body is still refused.
	r = httptest.NewRequest("POST", "/", strings.NewReader("reason=x"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := decodeOptional(r, &dst); err == nil {
		t.Error("form body accepted")
	}
	// A JSON body still arrives in full after the peek.
	r = httptest.NewRequest("POST", "/", strings.NewReader(`{"reason":"duplicate"}`))
	r.Header.Set("Content-Type", "application/json")
	if err := decodeOptional(r, &dst); err != nil || dst.Reason != "duplicate" {
		t.Errorf("JSON body: %q, %v", dst.Reason, err)
	}
	// Strictness is unchanged.
	r = httptest.NewRequest("POST", "/", strings.NewReader(`{"reasno":"x"}`))
	if err := decodeOptional(r, &dst); err == nil || !strings.Contains(err.Error(), "Unknown field") {
		t.Errorf("misspelt field: %v", err)
	}
}

func TestEveryRouteIsGatedOrDeliberatelyPublic(t *testing.T) {
	a := New(nil, nil, Options{})
	public := map[string]bool{"GET /healthz": true, "POST /api/v1/auth/login": true,
		"GET /api/v1/webhooks/whatsapp": true, "POST /api/v1/webhooks/whatsapp": true}
	seen := map[string]bool{}
	for _, rt := range a.Routes() {
		key := rt.Method + " " + rt.Path
		if seen[key] {
			t.Errorf("route %s registered twice", key)
		}
		seen[key] = true
		if rt.Public != public[key] {
			t.Errorf("route %s: public=%v, expected %v", key, rt.Public, public[key])
		}
		for _, role := range rt.Roles {
			if role == model.RoleSuperAdmin {
				t.Errorf("route %s grants super_admin routine access to practice data", key)
			}
		}
	}
	// Building the handler must not panic on conflicting patterns.
	_ = a.Handler()
	_ = http.StatusOK
}
