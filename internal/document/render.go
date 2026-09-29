// Package document renders referral letters and medical certificates as
// plain text. Rendering happens once, when a document is issued; the text is
// stored and never regenerated, so a template change next year does not
// alter a certificate issued today.
package document

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/phone"
)

const width = 76

// Practice is the letterhead.
type Practice struct {
	Name           string
	Address        string
	Phone          string
	Email          string
	PracticeNumber string
}

// Practitioner is who signs.
type Practitioner struct {
	Name               string
	RegistrationNumber string
}

// Patient is who the document is about.
type Patient struct {
	FullName         string
	DateOfBirth      model.Date
	Sex              model.Sex
	IDType           model.IDType
	IDNumber         string
	MedicalAidName   string
	MedicalAidNumber string
	AgeYears         int
}

// ReferralData is everything a referral letter shows. Medication and
// Allergies are only rendered when the body asks for them: a referral to a
// physiotherapist does not need the full medication list, and sending it
// anyway is a disclosure nobody requested.
type ReferralData struct {
	Practice   Practice
	Author     Practitioner
	Patient    Patient
	Number     string
	IssuedOn   model.Date
	Body       model.ReferralBody
	Medication []string
	Allergies  []string
}

// SickNoteData is everything a medical certificate shows.
type SickNoteData struct {
	Practice     Practice
	Practitioner Practitioner
	Patient      Patient
	Number       string
	IssuedOn     model.Date
	Body         model.SickNoteBody
}

// LongDate formats a date the South African way: 1 January 2026.
func LongDate(d model.Date) string {
	if d.IsZero() {
		return ""
	}
	return d.Time().Format("2 January 2006")
}

func letterhead(b *strings.Builder, p Practice) {
	b.WriteString(strings.ToUpper(p.Name) + "\n")
	if p.Address != "" {
		b.WriteString(p.Address + "\n")
	}
	var contact []string
	if p.Phone != "" {
		contact = append(contact, "Tel "+phone.Display(p.Phone))
	}
	if p.Email != "" {
		contact = append(contact, p.Email)
	}
	if p.PracticeNumber != "" {
		contact = append(contact, "Practice no. "+p.PracticeNumber)
	}
	if len(contact) > 0 {
		b.WriteString(strings.Join(contact, " | ") + "\n")
	}
	b.WriteString(strings.Repeat("-", width) + "\n\n")
}

// titleLine puts a title on the left and the document number on the right.
func titleLine(b *strings.Builder, title, number string) {
	gap := width - utf8.RuneCountInString(title) - utf8.RuneCountInString(number)
	if gap < 2 {
		gap = 2
	}
	b.WriteString(title + strings.Repeat(" ", gap) + number + "\n")
}

// wrap breaks text into lines of at most width runes, keeping the author's
// own line breaks and indenting continuation lines by indent spaces.
func wrap(text string, indent int) string {
	pad := strings.Repeat(" ", indent)
	var out []string
	for _, para := range strings.Split(strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n"), "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := pad + words[0]
		for _, w := range words[1:] {
			if utf8.RuneCountInString(line)+1+utf8.RuneCountInString(w) > width {
				out = append(out, line)
				line = pad + w
				continue
			}
			line += " " + w
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func section(b *strings.Builder, heading, text string) {
	b.WriteString(heading + "\n")
	b.WriteString(wrap(text, 0) + "\n\n")
}

func list(b *strings.Builder, heading string, items []string, none string) {
	b.WriteString(heading + "\n")
	if len(items) == 0 {
		b.WriteString(none + "\n\n")
		return
	}
	for _, it := range items {
		// Continuation lines hang under the text, not under the dash.
		b.WriteString("- " + strings.TrimPrefix(wrap(it, 2), "  ") + "\n")
	}
	b.WriteString("\n")
}

func idLabel(t model.IDType) string {
	switch t {
	case model.IDTypeSAID:
		return "SA ID"
	case model.IDTypePassport:
		return "Passport"
	}
	return "ID"
}

func signature(b *strings.Builder, p Practitioner, practice Practice) {
	b.WriteString(p.Name + "\n")
	if p.RegistrationNumber != "" {
		b.WriteString("HPCSA registration " + p.RegistrationNumber + "\n")
	}
	b.WriteString(practice.Name + "\n")
}

// Referral renders a referral letter.
func Referral(d ReferralData) string {
	var b strings.Builder
	letterhead(&b, d.Practice)
	titleLine(&b, "REFERRAL LETTER", d.Number)
	b.WriteString("Date: " + LongDate(d.IssuedOn) + "\n")
	if strings.EqualFold(d.Body.Urgency, "urgent") {
		b.WriteString("Urgency: URGENT\n")
	} else {
		b.WriteString("Urgency: Routine\n")
	}
	b.WriteString("\n")

	to := d.Body.ToName
	if d.Body.ToSpeciality != "" {
		to += " (" + d.Body.ToSpeciality + ")"
	}
	b.WriteString("To:  " + to + "\n")
	if d.Body.ToPractice != "" {
		b.WriteString("     " + d.Body.ToPractice + "\n")
	}
	b.WriteString("\n")

	b.WriteString("Re:  " + d.Patient.FullName + "\n")
	born := "     Born " + LongDate(d.Patient.DateOfBirth)
	if d.Patient.AgeYears > 0 {
		born += fmt.Sprintf(" (%d years)", d.Patient.AgeYears)
	}
	if d.Patient.Sex != "" && d.Patient.Sex != model.SexUnknown {
		born += ", " + string(d.Patient.Sex)
	}
	b.WriteString(born + "\n")
	if d.Patient.IDNumber != "" {
		b.WriteString("     " + idLabel(d.Patient.IDType) + " " + d.Patient.IDNumber + "\n")
	}
	if d.Patient.MedicalAidName != "" {
		aid := "     Medical aid: " + d.Patient.MedicalAidName
		if d.Patient.MedicalAidNumber != "" {
			aid += ", no. " + d.Patient.MedicalAidNumber
		}
		b.WriteString(aid + "\n")
	}
	b.WriteString("\n")

	b.WriteString("Dear " + d.Body.ToName + ",\n\n")
	b.WriteString("Thank you for seeing this patient.\n\n")
	section(&b, "REASON FOR REFERRAL", d.Body.Reason)
	if strings.TrimSpace(d.Body.ClinicalSummary) != "" {
		section(&b, "CLINICAL SUMMARY", d.Body.ClinicalSummary)
	}
	if d.Body.IncludeMedication {
		list(&b, "CURRENT MEDICATION", d.Medication, "None recorded.")
	}
	if d.Body.IncludeAllergies {
		list(&b, "KNOWN ALLERGIES", d.Allergies, "No known allergies recorded.")
	}
	b.WriteString("Yours sincerely,\n\n\n")
	signature(&b, d.Author, d.Practice)
	return b.String()
}

// SickNote renders a medical certificate.
//
// Two rules are built in because they are not optional under HPCSA
// guidance: the certificate states whether it rests on the practitioner's
// own examination or on what the patient reported, and the diagnosis is
// withheld unless the patient agreed to its disclosure — with the
// certificate saying so, so an employer does not read the absence as an
// oversight. A backdated certificate says that it is backdated, and why.
func SickNote(d SickNoteData) string {
	var b strings.Builder
	body := d.Body
	letterhead(&b, d.Practice)
	titleLine(&b, "MEDICAL CERTIFICATE", d.Number)
	b.WriteString("Date of issue: " + LongDate(d.IssuedOn) + "\n\n")

	who := d.Patient.FullName + " (born " + LongDate(d.Patient.DateOfBirth)
	if body.EmployeeNumber != "" {
		who += ", employee number " + body.EmployeeNumber
	}
	who += ")"
	practitioner := d.Practitioner.Name
	if d.Practitioner.RegistrationNumber != "" {
		practitioner += " (HPCSA registration " + d.Practitioner.RegistrationNumber + ")"
	}
	period := "from " + LongDate(body.ValidFrom) + " to " + LongDate(body.ValidTo) + " inclusive"
	if body.ValidFrom == body.ValidTo {
		period = "on " + LongDate(body.ValidFrom)
	}
	unfit := map[string]string{"work": "unfit for work", "school": "unfit to attend school"}[body.CertificateFor]
	if unfit == "" {
		unfit = "unfit for duty"
	}

	switch body.Basis {
	case model.BasisPatientReport:
		b.WriteString(wrap("I, "+practitioner+", certify that "+who+" consulted me on "+LongDate(body.ExaminedOn)+".", 0) + "\n\n")
		b.WriteString(wrap("This certificate is based on what the patient reported to me, not on my own examination.", 0) + "\n\n")
		if body.Fitness == "light_duties" {
			b.WriteString(wrap("According to the patient, they were fit for light duties only "+period+".", 0) + "\n\n")
		} else {
			b.WriteString(wrap("According to the patient, they were "+unfit+" "+period+".", 0) + "\n\n")
		}
	default:
		b.WriteString(wrap("I, "+practitioner+", certify that I examined "+who+" on "+LongDate(body.ExaminedOn)+".", 0) + "\n\n")
		if body.Fitness == "light_duties" {
			b.WriteString(wrap("In my professional opinion the patient is fit for light duties only "+period+".", 0) + "\n\n")
		} else {
			b.WriteString(wrap("In my professional opinion the patient is "+unfit+" "+period+".", 0) + "\n\n")
		}
	}

	b.WriteString("DIAGNOSIS\n")
	if body.DiscloseDiagnosis && strings.TrimSpace(body.Diagnosis) != "" {
		b.WriteString(wrap(strings.TrimSpace(body.Diagnosis)+" (disclosed with the patient's consent).", 0) + "\n\n")
	} else {
		b.WriteString(wrap("The nature of the illness is not disclosed, in accordance with patient confidentiality.", 0) + "\n\n")
	}

	if body.ValidFrom.Before(body.ExaminedOn) {
		b.WriteString("BACKDATED\n")
		b.WriteString(wrap("This certificate covers a period that began before the consultation on "+
			LongDate(body.ExaminedOn)+". Reason: "+strings.TrimSpace(body.BackdateReason), 0) + "\n\n")
	}
	if strings.TrimSpace(body.Remarks) != "" {
		section(&b, "REMARKS", body.Remarks)
	}
	b.WriteString("Signed: ____________________________\n\n")
	signature(&b, d.Practitioner, d.Practice)
	if d.Practice.Address != "" {
		b.WriteString(d.Practice.Address + "\n")
	}
	return b.String()
}
