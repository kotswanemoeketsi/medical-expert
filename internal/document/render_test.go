package document

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yourorg/medical-expert-system/internal/model"
)

// flat joins wrapped lines so a phrase can be found wherever the wrap fell.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

var (
	practice = Practice{Name: "Greenside Clinic", Address: "12 Main Road, Greenside", Phone: "+27115550123",
		Email: "reception@greenside.co.za", PracticeNumber: "0123456"}
	doctor  = Practitioner{Name: "Dr Priya Naidoo", RegistrationNumber: "MP0712345"}
	patient = Patient{FullName: "Thabo Nkosi", DateOfBirth: model.NewDate(1980, 1, 1), Sex: model.SexMale,
		IDType: model.IDTypeSAID, IDNumber: "8001015009087", MedicalAidName: "Discovery Health",
		MedicalAidNumber: "123456789", AgeYears: 46}
	issued = model.NewDate(2026, 9, 28)
)

func referral(meds, allergies bool) ReferralData {
	return ReferralData{
		Practice: practice, Author: doctor, Patient: patient, Number: "REF-2026-0001", IssuedOn: issued,
		Body: model.ReferralBody{ToName: "Dr A Botha", ToSpeciality: "Cardiologist", ToPractice: "Heart Care Centre",
			Urgency: "urgent", Reason: "Exertional chest pain for two weeks.", ClinicalSummary: "ECG attached.",
			IncludeMedication: meds, IncludeAllergies: allergies},
		Medication: []string{"Metformin 500 mg tablet: 1 tablet twice daily, oral"},
		Allergies:  []string{"Penicillin: anaphylaxis (life threatening)"},
	}
}

func TestReferralIncludesMedicationAndAllergiesOnlyWhenAsked(t *testing.T) {
	without := Referral(referral(false, false))
	if strings.Contains(without, "Metformin") || strings.Contains(without, "Penicillin") ||
		strings.Contains(without, "CURRENT MEDICATION") || strings.Contains(without, "KNOWN ALLERGIES") {
		t.Fatalf("medication or allergies disclosed without being requested:\n%s", without)
	}
	with := flat(Referral(referral(true, true)))
	for _, want := range []string{"CURRENT MEDICATION", "Metformin 500 mg tablet", "KNOWN ALLERGIES", "Penicillin"} {
		if !strings.Contains(with, want) {
			t.Errorf("requested content missing %q:\n%s", want, with)
		}
	}
	d := referral(false, true)
	d.Allergies = nil
	if !strings.Contains(Referral(d), "No known allergies recorded.") {
		t.Error("an empty allergy list must say so rather than print nothing")
	}
}

func TestReferralContent(t *testing.T) {
	out := flat(Referral(referral(false, false)))
	for _, want := range []string{
		"GREENSIDE CLINIC", "Practice no. 0123456", "REF-2026-0001", "Date: 28 September 2026", "Urgency: URGENT",
		"To: Dr A Botha (Cardiologist)", "Heart Care Centre", "Re: Thabo Nkosi", "Born 1 January 1980 (46 years), male",
		"SA ID 8001015009087", "Medical aid: Discovery Health, no. 123456789", "Dear Dr A Botha,",
		"REASON FOR REFERRAL", "Exertional chest pain", "HPCSA registration MP0712345",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func sickNote(basis model.CertificateBasis) SickNoteData {
	return SickNoteData{Practice: practice, Practitioner: doctor, Patient: patient, Number: "MC-2026-0007", IssuedOn: issued,
		Body: model.SickNoteBody{Basis: basis, ExaminedOn: issued, ValidFrom: issued, ValidTo: model.NewDate(2026, 9, 30),
			Fitness: "unfit", CertificateFor: "work", EmployeeNumber: "4471"}}
}

func TestSickNoteStatesItsBasis(t *testing.T) {
	exam := flat(SickNote(sickNote(model.BasisExamination)))
	if !strings.Contains(exam, "certify that I examined Thabo Nkosi (born 1 January 1980, employee number 4471) on 28 September 2026.") ||
		!strings.Contains(exam, "In my professional opinion") {
		t.Fatalf("examination wording missing:\n%s", exam)
	}
	report := flat(SickNote(sickNote(model.BasisPatientReport)))
	if !strings.Contains(report, "certify that Thabo Nkosi (born 1 January 1980, employee number 4471) consulted me on 28 September 2026.") ||
		!strings.Contains(report, "based on what the patient reported to me, not on my own examination") ||
		!strings.Contains(report, "According to the patient") || strings.Contains(report, "I examined") {
		t.Fatalf("patient-report wording wrong:\n%s", report)
	}
	d := sickNote(model.BasisExamination)
	d.Body.EmployeeNumber = ""
	if out := flat(SickNote(d)); !strings.Contains(out, "I examined Thabo Nkosi (born 1 January 1980) on 28 September 2026.") {
		t.Fatalf("wording without an employee number:\n%s", out)
	}
}

func TestSickNoteWithholdsDiagnosisUnlessDisclosed(t *testing.T) {
	d := sickNote(model.BasisExamination)
	d.Body.Diagnosis = "Influenza"
	withheld := flat(SickNote(d))
	if strings.Contains(withheld, "Influenza") || !strings.Contains(withheld, "not disclosed, in accordance with patient confidentiality") {
		t.Fatalf("diagnosis disclosed without consent:\n%s", withheld)
	}
	d.Body.DiscloseDiagnosis = true
	disclosed := flat(SickNote(d))
	if !strings.Contains(disclosed, "Influenza (disclosed with the patient's consent).") {
		t.Fatalf("consented diagnosis missing:\n%s", disclosed)
	}
}

func TestSickNoteDisclosesBackdating(t *testing.T) {
	d := sickNote(model.BasisExamination)
	if strings.Contains(SickNote(d), "BACKDATED") {
		t.Fatal("a certificate starting on the consultation day is not backdated")
	}
	d.Body.ValidFrom = model.NewDate(2026, 9, 26)
	d.Body.BackdateReason = "Seen on the first day the practice reopened after the weekend"
	out := flat(SickNote(d))
	if !strings.Contains(out, "BACKDATED") || !strings.Contains(out, "reopened after the weekend") {
		t.Fatalf("backdating not disclosed:\n%s", out)
	}
}

func TestSickNoteLeavesOutTheIDNumber(t *testing.T) {
	// An employer needs the name and date of birth, not the ID number.
	out := flat(SickNote(sickNote(model.BasisExamination)))
	if strings.Contains(out, "8001015009087") {
		t.Fatalf("ID number printed on a certificate for an employer:\n%s", out)
	}
	if !strings.Contains(out, "employee number 4471") || !strings.Contains(out, "from 28 September 2026 to 30 September 2026 inclusive") {
		t.Fatalf("details missing:\n%s", out)
	}
}

func TestLightDutiesAndSchool(t *testing.T) {
	d := sickNote(model.BasisExamination)
	d.Body.Fitness = "light_duties"
	if !strings.Contains(flat(SickNote(d)), "fit for light duties only") {
		t.Fatal("light duties wording missing")
	}
	d.Body.Fitness, d.Body.CertificateFor = "unfit", "school"
	if !strings.Contains(flat(SickNote(d)), "the patient is unfit to attend school from") {
		t.Fatalf("school wording wrong:\n%s", SickNote(d))
	}
	d.Body.CertificateFor = "other"
	if !strings.Contains(flat(SickNote(d)), "the patient is unfit for duty from") {
		t.Fatalf("duty wording wrong:\n%s", SickNote(d))
	}
}

func TestSingleDayCertificateAndPrintedPhone(t *testing.T) {
	d := sickNote(model.BasisExamination)
	d.Body.ValidTo = d.Body.ValidFrom
	out := flat(SickNote(d))
	if !strings.Contains(out, "unfit for work on 28 September 2026.") {
		t.Fatalf("single-day wording:\n%s", out)
	}
	if !strings.Contains(out, "Tel 011 555 0123") {
		t.Fatalf("practice phone not printed the South African way:\n%s", out)
	}
}

func TestListItemsHangUnderTheirBullet(t *testing.T) {
	d := referral(true, false)
	d.Medication = []string{"Metformin 500 mg tablet: 1 tablet twice daily, oral (with breakfast and supper, and a glass of water, starting on the first day of the month)"}
	out := Referral(d)
	start := strings.Index(out, "- Metformin")
	if start < 0 {
		t.Fatalf("item missing:\n%s", out)
	}
	lines := strings.Split(out[start:], "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[1], "  ") || strings.HasPrefix(lines[1], "   ") || lines[2] != "" {
		t.Fatalf("continuation should be indented two spaces under the text:\n%s", out[start:])
	}
	for _, line := range lines[:2] {
		if n := utf8.RuneCountInString(line); n > width {
			t.Fatalf("line of %d characters: %q", n, line)
		}
	}
}

func TestLinesStayWithinWidth(t *testing.T) {
	d := referral(true, true)
	d.Body.Reason = strings.Repeat("A long clinical explanation that goes on and on. ", 20)
	for _, line := range strings.Split(Referral(d), "\n") {
		if n := utf8.RuneCountInString(line); n > width {
			t.Fatalf("line of %d characters: %q", n, line)
		}
	}
}
