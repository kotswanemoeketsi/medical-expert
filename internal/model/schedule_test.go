package model

import (
	"testing"
	"time"
)

var sast = time.FixedZone("SAST", 2*3600)

func hm(h, m int) TimeOfDay { return TimeOfDay{Minutes: h*60 + m} }

func clockTimes(ts []time.Time) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Format("01-02 15:04")
	}
	return out
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func regimenFrom(freq Frequency, start Date) *Regimen {
	return &Regimen{Frequency: freq, StartDate: start, Status: RegimenActive}
}

func TestDoseTimesFollowTheUsualClockForDefaultWakingHours(t *testing.T) {
	day := NewDate(2026, 10, 5)
	cases := map[Frequency][]string{
		FreqOnceDaily:       {"10-05 08:00"},
		FreqTwiceDaily:      {"10-05 08:00", "10-05 20:00"},
		FreqThreeTimesDaily: {"10-05 08:00", "10-05 14:00", "10-05 20:00"},
		FreqFourTimesDaily:  {"10-05 08:00", "10-05 12:00", "10-05 16:00", "10-05 20:00"},
		FreqAsNeeded:        {},
	}
	for freq, want := range cases {
		got := clockTimes(regimenFrom(freq, day).DoseTimesOn(day, DefaultWakingHours, sast))
		if !sameList(got, want) {
			t.Errorf("%s: got %v, want %v", freq, got, want)
		}
	}
}

func TestDoseTimesRespectThePatientsWakingHours(t *testing.T) {
	day := NewDate(2026, 10, 5)
	late := WakingHours{Start: hm(9, 30), End: hm(23, 30)}
	got := clockTimes(regimenFrom(FreqTwiceDaily, day).DoseTimesOn(day, late, sast))
	if !sameList(got, []string{"10-05 10:30", "10-05 22:30"}) {
		t.Errorf("late riser: %v", got)
	}

	// Night shift: awake 20:00 to 08:00. The morning dose belongs to the
	// evening before, and lands on the next calendar day.
	night := WakingHours{Start: hm(20, 0), End: hm(8, 0)}
	got = clockTimes(regimenFrom(FreqTwiceDaily, day).DoseTimesOn(day, night, sast))
	if !sameList(got, []string{"10-05 21:00", "10-06 07:00"}) {
		t.Errorf("night shift: %v", got)
	}
	if !night.Contains(hm(3, 0).Minutes) || night.Contains(hm(12, 0).Minutes) {
		t.Error("night window membership wrong")
	}

	// A window too short for margins uses all of it.
	short := WakingHours{Start: hm(12, 0), End: hm(14, 0)}
	got = clockTimes(regimenFrom(FreqThreeTimesDaily, day).DoseTimesOn(day, short, sast))
	if !sameList(got, []string{"10-05 12:00", "10-05 13:00", "10-05 14:00"}) {
		t.Errorf("short window: %v", got)
	}

	p := &Patient{}
	if p.Waking() != DefaultWakingHours {
		t.Error("a patient without waking hours gets the default")
	}
	s, e := hm(6, 0), hm(22, 0)
	p.WakingStart, p.WakingEnd = &s, &e
	if p.Waking() != (WakingHours{Start: s, End: e}) {
		t.Error("the patient's own hours are used")
	}
}

func TestAlternateDayAndWeeklyCountFromTheStartDate(t *testing.T) {
	start := NewDate(2026, 10, 1) // a Thursday
	alt := regimenFrom(FreqEveryOtherDay, start)
	weekly := regimenFrom(FreqWeekly, start)
	var altDays, weeklyDays []int
	for d := 1; d <= 15; d++ {
		day := NewDate(2026, 10, d)
		if len(alt.DoseTimesOn(day, DefaultWakingHours, sast)) == 1 {
			altDays = append(altDays, d)
		}
		if len(weekly.DoseTimesOn(day, DefaultWakingHours, sast)) == 1 {
			weeklyDays = append(weeklyDays, d)
		}
	}
	if len(altDays) != 8 || altDays[0] != 1 || altDays[1] != 3 || altDays[7] != 15 {
		t.Errorf("every other day: %v", altDays)
	}
	if len(weeklyDays) != 3 || weeklyDays[0] != 1 || weeklyDays[1] != 8 || weeklyDays[2] != 15 {
		t.Errorf("weekly: %v", weeklyDays)
	}
}

func TestDoseTimesStayWithinTheRegimensDates(t *testing.T) {
	start, end := NewDate(2026, 10, 3), NewDate(2026, 10, 4)
	g := regimenFrom(FreqOnceDaily, start)
	g.EndDate = &end
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, sast)
	to := time.Date(2026, 10, 8, 0, 0, 0, 0, sast)
	got := clockTimes(g.DoseTimesBetween(from, to, DefaultWakingHours, sast))
	if !sameList(got, []string{"10-03 08:00", "10-04 08:00"}) {
		t.Errorf("bounded regimen: %v", got)
	}

	// [from, to) is half-open, and a window crossing midnight contributes
	// the previous evening's early-morning dose.
	night := WakingHours{Start: hm(20, 0), End: hm(8, 0)}
	g = regimenFrom(FreqTwiceDaily, NewDate(2026, 9, 1))
	from = time.Date(2026, 10, 6, 7, 0, 0, 0, sast)
	to = time.Date(2026, 10, 6, 21, 0, 0, 0, sast)
	got = clockTimes(g.DoseTimesBetween(from, to, night, sast))
	if !sameList(got, []string{"10-06 07:00"}) {
		t.Errorf("half-open window across midnight: %v", got)
	}
}

func TestDoseTimesSurviveADaylightSavingChange(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip("no time zone data")
	}
	// Clocks went forward at 01:00 on 29 March 2026.
	g := regimenFrom(FreqFourTimesDaily, NewDate(2026, 3, 1))
	got := g.DoseTimesOn(NewDate(2026, 3, 29), DefaultWakingHours, london)
	if len(got) != 4 {
		t.Fatalf("got %v", got)
	}
	for _, x := range got {
		if x.In(london).Hour() < 8 || x.In(london).Hour() > 20 {
			t.Errorf("dose outside waking hours after the change: %v", x.In(london))
		}
	}
}
