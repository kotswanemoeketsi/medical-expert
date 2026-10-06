package model

import (
	"sort"
	"time"
)

// WakingHours is the part of the day a patient is awake, in the practice's
// local time (BR-4.2). An end before the start means the window crosses
// midnight: someone on night shift may be awake from 20:00 to 08:00.
type WakingHours struct {
	Start TimeOfDay `json:"start"`
	End   TimeOfDay `json:"end"`
}

// DefaultWakingHours applies to patients who have not given theirs.
var DefaultWakingHours = WakingHours{Start: TimeOfDay{Minutes: 7 * 60}, End: TimeOfDay{Minutes: 21 * 60}}

// Waking returns the patient's waking hours, or the default.
func (p *Patient) Waking() WakingHours {
	if p.WakingStart == nil || p.WakingEnd == nil || p.WakingStart.Minutes == p.WakingEnd.Minutes {
		return DefaultWakingHours
	}
	return WakingHours{Start: *p.WakingStart, End: *p.WakingEnd}
}

// Length is the window in minutes.
func (w WakingHours) Length() int {
	n := (w.End.Minutes - w.Start.Minutes + 1440) % 1440
	if n == 0 {
		return 1440
	}
	return n
}

// Contains reports whether a wall-clock time, in minutes after midnight,
// falls inside the window, ends included.
func (w WakingHours) Contains(minute int) bool {
	return (minute-w.Start.Minutes+1440)%1440 <= w.Length()
}

// offsets places n doses in the window, as minutes after it opens. The first
// dose is an hour after waking and the last an hour before sleep, with the
// rest evenly between, so 07:00 to 21:00 gives 08:00 and 20:00 for twice
// daily and 08:00, 12:00, 16:00 and 20:00 for four times. A window shorter
// than three hours has no margin. Times are rounded to five minutes.
func (w WakingHours) offsets(n int) []int {
	length := w.Length()
	margin := 60
	if length < 180 {
		margin = 0
	}
	first, last := margin, length-margin
	out := make([]int, n)
	for i := range out {
		off := first
		if n > 1 {
			off = first + (last-first)*i/(n-1)
		}
		out[i] = (off + 2) / 5 * 5
	}
	return out
}

// dosesOnScheduledDays is how many doses fall on a day that has any: every
// other day and weekly medicine is taken once on the days it is due.
func (f Frequency) dosesOnScheduledDays() int {
	switch f {
	case FreqTwiceDaily:
		return 2
	case FreqThreeTimesDaily:
		return 3
	case FreqFourTimesDaily:
		return 4
	case FreqOnceDaily, FreqEveryOtherDay, FreqWeekly:
		return 1
	}
	return 0 // as needed: nothing to schedule
}

func daysBetween(a, b Date) int {
	return int(b.Time().Sub(a.Time()).Hours() / 24)
}

// DoseTimesOn returns the doses of a waking day: the day whose waking hours
// start on the given date, so a night-shift window's early-morning doses
// belong to the evening before. Every-other-day and weekly regimens count
// from their start date. Status is the caller's concern; dates are not.
func (r *Regimen) DoseTimesOn(day Date, w WakingHours, loc *time.Location) []time.Time {
	n := r.Frequency.dosesOnScheduledDays()
	if n == 0 || day.Before(r.StartDate) || (r.EndDate != nil && day.After(*r.EndDate)) {
		return nil
	}
	switch since := daysBetween(r.StartDate, day); r.Frequency {
	case FreqEveryOtherDay:
		if since%2 != 0 {
			return nil
		}
	case FreqWeekly:
		if since%7 != 0 {
			return nil
		}
	}
	out := make([]time.Time, 0, n)
	for _, off := range w.offsets(n) {
		// time.Date carries minutes past midnight into the next day.
		t := time.Date(day.Year, day.Month, day.Day, 0, w.Start.Minutes+off, 0, 0, loc)
		if len(out) > 0 && !t.After(out[len(out)-1]) {
			continue // a window too short to separate the doses
		}
		out = append(out, t)
	}
	return out
}

// DoseTimesBetween returns every dose in [from, to), in order.
func (r *Regimen) DoseTimesBetween(from, to time.Time, w WakingHours, loc *time.Location) []time.Time {
	var out []time.Time
	// Start a day early: a window that crosses midnight puts yesterday's last
	// doses into today.
	day := DateOf(from.In(loc).AddDate(0, 0, -1))
	last := DateOf(to.In(loc))
	for !day.After(last) {
		for _, t := range r.DoseTimesOn(day, w, loc) {
			if !t.Before(from) && t.Before(to) {
				out = append(out, t)
			}
		}
		day = DateOf(day.Time().AddDate(0, 0, 1))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}
