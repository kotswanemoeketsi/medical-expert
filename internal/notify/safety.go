package notify

import (
	"fmt"
	"strings"
	"unicode"
)

// sensitiveTerms are words that would tell whoever holds the phone something
// about the patient's health: conditions, treatments, tests and results.
// Medicine names come from the catalogue on top of these.
var sensitiveTerms = []string{
	// conditions
	"hiv", "aids", "tb", "tuberculosis", "diabetes", "diabetic", "sugar diabetes", "cancer", "oncology",
	"tumour", "tumor", "hypertension", "high blood pressure", "blood pressure", "cholesterol", "asthma",
	"copd", "epilepsy", "seizure", "seizures", "depression", "anxiety", "bipolar", "schizophrenia",
	"psychiatric", "psychiatry", "mental health", "dementia", "alzheimer", "alzheimers", "parkinson",
	"parkinsons", "stroke", "heart failure", "cardiac", "kidney", "renal", "dialysis", "liver",
	"hepatitis", "cirrhosis", "arthritis", "gout", "thyroid", "hypothyroidism", "hyperthyroidism",
	"lupus", "sickle cell", "infertility", "fertility", "pregnancy", "pregnant", "antenatal", "abortion",
	"termination", "miscarriage", "std", "sti", "syphilis", "gonorrhoea", "gonorrhea", "chlamydia",
	"herpes", "addiction", "rehab", "rehabilitation", "alcoholism", "overdose",
	// treatments
	"arv", "arvs", "antiretroviral", "antiretrovirals", "prep", "pep", "insulin", "chemo",
	"chemotherapy", "radiotherapy", "radiation", "methadone", "contraceptive", "contraception",
	"antidepressant", "antidepressants", "antipsychotic", "antipsychotics", "inhaler", "injection",
	"tablets", "tablet", "pills", "pill", "capsules", "dose of",
	// tests and results
	"result", "results", "positive", "negative", "abnormal", "biopsy", "viral load", "cd4", "hba1c",
	"blood test", "blood tests", "scan", "x ray", "xray", "mri", "ultrasound", "pathology", "lab", "labs",
	"laboratory", "screening", "diagnosis", "diagnosed",
}

// normalise lowercases and turns everything that is not a letter or digit
// into single spaces, padded, so terms match on whole words: "art" matches
// "ART clinic" but not "Martin".
func normalise(s string) string {
	var b strings.Builder
	b.WriteByte(' ')
	space := true
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	if !space {
		b.WriteByte(' ')
	}
	return b.String()
}

// MedicineTerms turns the catalogue's names into terms for
// CheckNoClinicalDetail. A combination product is listed as
// "Tenofovir/lamivudine/dolutegravir", and a message naming any one of its
// medicines says as much as the full name, so each becomes a term of its
// own. Parts with digits, such as the "30" and "70" of "(30/70)", are
// strengths, not names, and are left out.
func MedicineTerms(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	add := func(s string) {
		s = strings.TrimSpace(s)
		if k := normalise(s); s != "" && !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	for _, name := range names {
		add(name)
		if !strings.ContainsAny(name, "/+") {
			continue
		}
		for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '+' }) {
			letters := 0
			for _, r := range part {
				if unicode.IsLetter(r) {
					letters++
				}
			}
			if letters >= 4 && !strings.ContainsAny(part, "0123456789()") {
				add(part)
			}
		}
	}
	return out
}

// ClinicalDetailError names the word that would have gone out.
type ClinicalDetailError struct {
	Param int // 1-based
	Term  string
}

func (e *ClinicalDetailError) Error() string {
	return fmt.Sprintf("message parameter %d contains %q, which could reveal something about the patient's health", e.Param, e.Term)
}

// CheckNoClinicalDetail refuses parameters that name a condition, a
// treatment, a test or a result, or any of the extra terms (the medicine
// names in the catalogue). The templates carry only names, times and the
// practice's details, so this is the second line of defence: it catches the
// practice whose name says what it treats, or a name field misused.
func CheckNoClinicalDetail(params []string, extra []string) error {
	for i, p := range params {
		text := normalise(p)
		for _, list := range [][]string{sensitiveTerms, extra} {
			for _, term := range list {
				t := normalise(term)
				if len(t) < 4 { // blank or a single character: too short to mean anything
					continue
				}
				if strings.Contains(text, t) {
					return &ClinicalDetailError{Param: i + 1, Term: strings.TrimSpace(t)}
				}
			}
		}
	}
	return nil
}
