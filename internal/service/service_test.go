package service

import (
	"strings"
	"testing"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
)

var today = model.NewDate(2026, 9, 28)

func validPatient() PatientInput {
	return PatientInput{
		FirstName: "  Lerato ", LastName: "Dlamini", DateOfBirth: model.NewDate(1980, 1, 1),
		Sex: model.SexFemale, NationalID: "8001015009087", Mobile: "082 123 4567",
		PreferredChannel: model.ChannelWhatsApp,
	}
}

func fields(v model.ValidationErrors) map[string]string {
	out := map[string]string{}
	for _, e := range v {
		out[e.Field] = e.Message
	}
	return out
}

func TestPatientInputNormalises(t *testing.T) {
	in := validPatient()
	if v := in.Validate(today); len(v) > 0 {
		t.Fatalf("valid input rejected: %v", v)
	}
	if in.FirstName != "Lerato" {
		t.Errorf("name not trimmed: %q", in.FirstName)
	}
	if in.Mobile != "+27821234567" {
		t.Errorf("mobile not normalised to E.164: %q", in.Mobile)
	}
	if in.IDType != model.IDTypeSAID {
		t.Errorf("13-digit ID should default to sa_id, got %q", in.IDType)
	}
	if in.SmokingStatus != model.SmokingUnknown || in.AlcoholUse != model.AlcoholUnknown || in.Status != model.PatientActive {
		t.Errorf("defaults not applied: %+v", in)
	}
}

func TestPatientNeedsAMobileForPhoneChannels(t *testing.T) {
	for _, ch := range []model.Channel{model.ChannelWhatsApp, model.ChannelSMS} {
		in := validPatient()
		in.Mobile, in.PreferredChannel = "", ch
		f := fields(in.Validate(today))
		if !strings.Contains(f["mobile"], "is required when reminders are sent by") {
			t.Errorf("%s without a mobile: got %v", ch, f)
		}
	}
	in := validPatient()
	in.Mobile, in.PreferredChannel = "", model.ChannelNone
	if v := in.Validate(today); len(v) > 0 {
		t.Errorf("no mobile is fine when no reminders are sent: %v", v)
	}
}

func TestPatientSAIDMustMatchDateOfBirth(t *testing.T) {
	in := validPatient()
	in.DateOfBirth = model.NewDate(1980, 1, 2)
	if f := fields(in.Validate(today)); f["national_id"] != "does not match the date of birth" {
		t.Fatalf("mismatched DOB accepted: %v", f)
	}
	in = validPatient()
	in.NationalID = "8001015009088"
	if f := fields(in.Validate(today)); !strings.Contains(f["national_id"], "check digit") {
		t.Fatalf("bad checksum accepted: %v", f)
	}
	in = validPatient()
	in.NationalID, in.IDType = "a1234567", model.IDTypePassport
	if v := in.Validate(today); len(v) > 0 {
		t.Fatalf("passport rejected: %v", v)
	}
	if in.NationalID != "A1234567" {
		t.Errorf("passport not upper-cased: %q", in.NationalID)
	}
	in = validPatient()
	in.NationalID, in.IDType = "X-99", ""
	if f := fields(in.Validate(today)); f["id_type"] == "" {
		t.Fatalf("an ID number of unknown type was accepted without id_type: %v", f)
	}
}

func TestPatientRejectsImpossibleValues(t *testing.T) {
	in := validPatient()
	in.FirstName, in.DateOfBirth, in.Sex = "", model.NewDate(2027, 1, 1), "m"
	in.NationalID = ""
	in.Email, in.PreferredChannel = "not-an-email", "pigeon"
	start := model.TimeOfDay{Minutes: 7 * 60}
	in.WakingStart = &start
	months := 36
	in.ReviewIntervalMonths = &months
	f := fields(in.Validate(today))
	for _, want := range []string{"first_name", "date_of_birth", "sex", "email", "preferred_channel", "waking_end", "review_interval_months"} {
		if f[want] == "" {
			t.Errorf("expected a problem with %s; got %v", want, f)
		}
	}
}

func TestCreateUserRequiresRegistrationForClinicalRoles(t *testing.T) {
	in := CreateUserInput{Email: "Dr.Naidoo@Greenside.co.za ", FullName: "Dr P Naidoo", Role: model.RoleDoctor,
		Password: "kettle-orbit-mango-42"}
	f := fields(in.Validate())
	if !strings.Contains(f["professional_reg_no"], "is required") {
		t.Fatalf("doctor without HPCSA number accepted: %v", f)
	}
	if in.Email != "dr.naidoo@greenside.co.za" {
		t.Errorf("email not normalised: %q", in.Email)
	}
	in.ProfessionalRegNo = "mp0123456"
	if v := in.Validate(); len(v) > 0 {
		t.Fatalf("valid doctor rejected: %v", v)
	}
	if in.ProfessionalRegNo != "MP0123456" {
		t.Errorf("registration number not upper-cased: %q", in.ProfessionalRegNo)
	}
}

func TestCreateUserRoleRules(t *testing.T) {
	base := CreateUserInput{Email: "x@y.co.za", FullName: "X Y", Password: "kettle-orbit-mango-42"}
	cases := map[model.Role]string{
		model.RoleSuperAdmin: "role",
		model.RoleCaregiver:  "role",
		model.Role("owner"):  "role",
		model.RolePatient:    "patient_id",
	}
	for role, field := range cases {
		in := base
		in.Role = role
		if f := fields(in.Validate()); f[field] == "" {
			t.Errorf("role %s: expected a problem with %s, got %v", role, field, f)
		}
	}
	in := base
	in.Role = model.RoleClinicAdmin
	pid := model.MustNewID()
	in.PatientID = &pid
	if f := fields(in.Validate()); f["patient_id"] == "" {
		t.Error("patient_id accepted on a staff account")
	}
	in = base
	in.Role, in.Password = model.RoleClinicAdmin, "Password12345"
	if f := fields(in.Validate()); f["password"] == "" {
		t.Error("weak password accepted")
	}
}

func TestValidEmail(t *testing.T) {
	for _, ok := range []string{"a@b.co", "thandi.mokoena@greenside.co.za", "x+tag@example.org"} {
		if !validEmail(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "a", "a@b", "a b@c.co", "<a@b.co>", "Name <a@b.co>", "a@b.co\n"} {
		if validEmail(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestChangedFieldsNamesWithoutValues(t *testing.T) {
	a := model.Patient{FirstName: "A", Mobile: "+27821111111"}
	b := a
	b.Mobile = "+27822222222"
	got := changedFields(&a, &b)
	if len(got) != 1 || got[0] != "mobile" {
		t.Fatalf("got %v", got)
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.SessionIdle != 30*time.Minute || c.SessionMax != 12*time.Hour || c.MFAChallengeTTL != 5*time.Minute ||
		c.MaxLoginAttempts != 5 || c.Lockout != 15*time.Minute || c.MFAIssuer == "" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestICD10Pattern(t *testing.T) {
	for _, ok := range []string{"E11.9", "I10", "J45.909", "B20", "Z34.0"} {
		if !icd10Pattern.MatchString(ok) {
			t.Errorf("rejected %s", ok)
		}
	}
	for _, bad := range []string{"e11", "11.9", "E1", "E11.", "E11.12345"} {
		if icd10Pattern.MatchString(bad) {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestTodayIsSouthAfricanDate(t *testing.T) {
	// 23:30 UTC on 28 September is already 29 September in Johannesburg.
	reg := &Registry{clock: func() time.Time { return time.Date(2026, 9, 28, 23, 30, 0, 0, time.UTC) }}
	b := &base{reg: reg}
	if got := b.today(); got != model.NewDate(2026, 9, 29) {
		t.Fatalf("today = %v, want 2026-09-29", got)
	}
}
