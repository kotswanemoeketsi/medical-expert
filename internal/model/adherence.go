package model

import "math"

// DoseCounts are closed doses over a period. Pending doses are left out:
// their time has not passed, or their answer window is still open, and
// counting them would make every future dose count against the patient.
type DoseCounts struct {
	Taken   int `json:"taken"`
	Skipped int `json:"skipped"`
	Missed  int `json:"missed"`
}

// Due is every closed dose.
func (c DoseCounts) Due() int { return c.Taken + c.Skipped + c.Missed }

// Answered is every dose the patient said something about.
func (c DoseCounts) Answered() int { return c.Taken + c.Skipped }

func ratio(n, d int) *float64 {
	if d == 0 {
		return nil // no doses is "no data", not 0%
	}
	r := math.Round(float64(n)/float64(d)*1000) / 1000
	return &r
}

// ResponseRate is whether the patient answered at all: a confirmed skip
// counts. AdherenceRate is whether they reported taking the dose. They are
// kept apart on purpose: a patient who reliably answers "skipped" is fully
// responsive and taking nothing, and needs a different phone call from one
// who never answers.
func (c DoseCounts) ResponseRate() *float64  { return ratio(c.Answered(), c.Due()) }
func (c DoseCounts) AdherenceRate() *float64 { return ratio(c.Taken, c.Due()) }

// AdherencePeriod is one rolling window (BR-4.6).
type AdherencePeriod struct {
	Days int `json:"days"`
	DoseCounts
	ResponseRate  *float64 `json:"response_rate"`
	AdherenceRate *float64 `json:"adherence_rate"`
}

// NewAdherencePeriod fills in the rates.
func NewAdherencePeriod(days int, c DoseCounts) AdherencePeriod {
	return AdherencePeriod{Days: days, DoseCounts: c, ResponseRate: c.ResponseRate(), AdherenceRate: c.AdherenceRate()}
}

// RegimenAdherence is one medicine's record over 7, 30 and 90 days.
type RegimenAdherence struct {
	RegimenID      ID                `json:"regimen_id"`
	MedicationName string            `json:"medication_name"`
	Periods        []AdherencePeriod `json:"periods"`
}

// ResponseBuckets count patients by how often they answer. Ten patients at
// 80% and ten at 0% average to the same figure as twenty at 40%, and only
// one of those tells staff whom to phone.
type ResponseBuckets struct {
	None     int `json:"none"`       // 0%
	Low      int `json:"1_to_49"`    // 1-49%
	Mid      int `json:"50_to_79"`   // 50-79%
	High     int `json:"80_to_99"`   // 80-99%
	Complete int `json:"every_dose"` // 100%
}

// Add places one patient. Patients with no closed doses are not placed:
// they have nothing to answer yet.
func (b *ResponseBuckets) Add(answered, due int) {
	if due <= 0 {
		return
	}
	switch pct := float64(answered) * 100 / float64(due); {
	case answered == 0:
		b.None++
	case answered >= due:
		b.Complete++
	case pct < 50:
		b.Low++
	case pct < 80:
		b.Mid++
	default:
		b.High++
	}
}
