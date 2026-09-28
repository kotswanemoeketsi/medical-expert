package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRolesRequiringMFAAndRegistration(t *testing.T) {
	cases := []struct {
		role         Role
		mfa, regNo   bool
		staff, valid bool
	}{
		{RoleSuperAdmin, true, false, false, true},
		{RoleClinicAdmin, true, false, true, true},
		{RoleDoctor, true, true, true, true},
		{RoleNurse, true, true, true, true},
		{RolePatient, false, false, false, true},
		{RoleCaregiver, false, false, false, true},
		{Role("receptionist"), false, false, false, false},
	}
	for _, c := range cases {
		if got := c.role.RequiresMFA(); got != c.mfa {
			t.Errorf("%s RequiresMFA = %v, want %v", c.role, got, c.mfa)
		}
		if got := c.role.RequiresProfessionalRegistration(); got != c.regNo {
			t.Errorf("%s RequiresProfessionalRegistration = %v, want %v", c.role, got, c.regNo)
		}
		if got := c.role.IsStaff(); got != c.staff {
			t.Errorf("%s IsStaff = %v, want %v", c.role, got, c.staff)
		}
		if got := c.role.Valid(); got != c.valid {
			t.Errorf("%s Valid = %v, want %v", c.role, got, c.valid)
		}
	}
}

func TestLockoutExpires(t *testing.T) {
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	until := now.Add(10 * time.Minute)
	u := User{Status: UserActive, LockedUntil: &until}
	if u.CanAuthenticate(now) {
		t.Fatal("a locked account must not authenticate")
	}
	if !u.CanAuthenticate(until.Add(time.Second)) {
		t.Fatal("the lock should lapse once locked_until has passed")
	}
	u.Status = UserSuspended
	if u.CanAuthenticate(until.Add(time.Hour)) {
		t.Fatal("a suspended account must not authenticate")
	}
}

func TestConsentAbsentMeansNotGranted(t *testing.T) {
	state := ConsentState{}
	for _, p := range ConsentPurposes {
		if state.Granted(p) {
			t.Fatalf("%s reads as granted with no record", p)
		}
	}
	state[ConsentAIAssistedReview] = Consent{Purpose: ConsentAIAssistedReview, Granted: false}
	if state.Granted(ConsentAIAssistedReview) {
		t.Fatal("an explicit refusal reads as granted")
	}
	st := state.Statuses()
	if len(st) != len(ConsentPurposes) {
		t.Fatalf("expected every purpose in the status list, got %d", len(st))
	}
	for _, s := range st {
		if s.Purpose == ConsentDataProcessing && s.Recorded {
			t.Fatal("an unrecorded purpose is reported as recorded")
		}
		if s.Purpose == ConsentAIAssistedReview && !s.Recorded {
			t.Fatal("a recorded refusal is reported as unrecorded")
		}
	}
}

func TestDateJSONAndAge(t *testing.T) {
	var d Date
	if err := json.Unmarshal([]byte(`"1980-02-29"`), &d); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	if string(b) != `"1980-02-29"` {
		t.Fatalf("round trip gave %s", b)
	}
	if got := d.AgeAt(time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)); got != 45 {
		t.Fatalf("age the day before a leap-day birthday: got %d, want 45", got)
	}
	if got := d.AgeAt(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); got != 46 {
		t.Fatalf("age after the birthday: got %d, want 46", got)
	}
	if err := json.Unmarshal([]byte(`"29/02/1980"`), &d); err == nil {
		t.Fatal("a non-ISO date was accepted")
	}
}

func TestDateScanFromDriverTypes(t *testing.T) {
	var d Date
	if err := d.Scan(time.Date(1975, 6, 16, 0, 0, 0, 0, time.UTC)); err != nil || d.String() != "1975-06-16" {
		t.Fatalf("scan time.Time: %v %v", d, err)
	}
	if err := d.Scan([]byte("1975-06-16")); err != nil || d.String() != "1975-06-16" {
		t.Fatalf("scan bytes: %v %v", d, err)
	}
}

func TestTimeOfDay(t *testing.T) {
	var tod TimeOfDay
	if err := tod.Scan([]byte("06:30:00")); err != nil || tod.String() != "06:30" {
		t.Fatalf("scan: %v %v", tod, err)
	}
	for _, bad := range []string{"24:00", "7", "07:60", "ab:cd"} {
		if _, err := ParseTimeOfDay(bad); err == nil {
			t.Errorf("ParseTimeOfDay accepted %q", bad)
		}
	}
}

func TestSAIDValidation(t *testing.T) {
	// 8001015009087 is the widely published example number: 1 Jan 1980.
	good := "8001015009087"
	if err := ValidateSAID(good); err != nil {
		t.Fatalf("valid number rejected: %v", err)
	}
	if !SAIDMatchesDOB(good, NewDate(1980, 1, 1)) {
		t.Fatal("DOB should match the ID prefix")
	}
	if SAIDMatchesDOB(good, NewDate(1980, 1, 2)) {
		t.Fatal("a different DOB matched")
	}
	cases := map[string]error{
		"8001015009088":  ErrSAIDChecksum, // last digit changed
		"800101500908":   ErrSAIDFormat,
		"80010150090877": ErrSAIDFormat,
		"80O1015009087":  ErrSAIDFormat,
		"8013015009087":  ErrSAIDDate, // month 13
		"8002305009087":  ErrSAIDDate, // 30 February
	}
	for id, want := range cases {
		if err := ValidateSAID(id); err != want {
			t.Errorf("ValidateSAID(%s) = %v, want %v", id, err, want)
		}
	}
}

func TestValidationErrorsErrIsNilWhenEmpty(t *testing.T) {
	var v ValidationErrors
	if v.Err() != nil {
		t.Fatal("empty ValidationErrors must produce a nil error")
	}
	v.Add("mobile", "is required when reminders are sent by %s", "WhatsApp")
	if v.Err() == nil || v[0].Message != "is required when reminders are sent by WhatsApp" {
		t.Fatalf("unexpected: %#v", v)
	}
}
