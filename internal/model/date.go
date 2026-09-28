package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Date is a calendar date with no time or zone: a date of birth, an onset
// date. Kept distinct from time.Time so nobody shifts a birthday by a day
// converting between time zones.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

const dateLayout = "2006-01-02"

func NewDate(y int, m time.Month, d int) Date { return Date{Year: y, Month: m, Day: d} }

// DateOf returns the calendar date of t in t's own location.
func DateOf(t time.Time) Date {
	y, m, d := t.Date()
	return Date{Year: y, Month: m, Day: d}
}

func ParseDate(s string) (Date, error) {
	t, err := time.Parse(dateLayout, strings.TrimSpace(s))
	if err != nil {
		return Date{}, fmt.Errorf("invalid date %q: expected YYYY-MM-DD", s)
	}
	return DateOf(t), nil
}

func (d Date) IsZero() bool { return d.Year == 0 && d.Month == 0 && d.Day == 0 }

// Time returns midnight UTC on the date.
func (d Date) Time() time.Time { return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC) }

func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.Time().Format(dateLayout)
}

func (d Date) Before(o Date) bool { return d.Time().Before(o.Time()) }
func (d Date) After(o Date) bool  { return d.Time().After(o.Time()) }

// AgeAt returns completed years at the given moment (in the moment's zone).
func (d Date) AgeAt(at time.Time) int {
	y, m, day := at.Date()
	age := y - d.Year
	if m < d.Month || (m == d.Month && day < d.Day) {
		age--
	}
	return age
}

func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(d.String())
}

func (d *Date) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*d = Date{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("date must be a string in YYYY-MM-DD form")
	}
	if strings.TrimSpace(s) == "" {
		*d = Date{}
		return nil
	}
	parsed, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

func (d Date) Value() (driver.Value, error) {
	if d.IsZero() {
		return nil, nil
	}
	return d.String(), nil
}

func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = Date{}
		return nil
	case time.Time:
		*d = DateOf(v)
		return nil
	case []byte:
		return d.scanString(string(v))
	case string:
		return d.scanString(v)
	default:
		return fmt.Errorf("cannot scan %T into Date", src)
	}
}

func (d *Date) scanString(s string) error {
	if len(s) >= 10 {
		s = s[:10]
	}
	parsed, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// TimeOfDay is a wall-clock time with minute precision, such as the start of
// a patient's waking hours.
type TimeOfDay struct {
	Minutes int // minutes since midnight, 0..1439
}

func ParseTimeOfDay(s string) (TimeOfDay, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return TimeOfDay{}, fmt.Errorf("invalid time %q: expected HH:MM", s)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return TimeOfDay{}, fmt.Errorf("invalid time %q: expected HH:MM", s)
	}
	return TimeOfDay{Minutes: h*60 + m}, nil
}

func (t TimeOfDay) String() string { return fmt.Sprintf("%02d:%02d", t.Minutes/60, t.Minutes%60) }

func (t TimeOfDay) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

func (t *TimeOfDay) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("time must be a string in HH:MM form")
	}
	parsed, err := ParseTimeOfDay(s)
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

func (t TimeOfDay) Value() (driver.Value, error) { return t.String() + ":00", nil }

func (t *TimeOfDay) Scan(src any) error {
	switch v := src.(type) {
	case []byte:
		return t.scan(string(v))
	case string:
		return t.scan(v)
	case time.Time:
		*t = TimeOfDay{Minutes: v.Hour()*60 + v.Minute()}
		return nil
	default:
		return fmt.Errorf("cannot scan %T into TimeOfDay", src)
	}
}

func (t *TimeOfDay) scan(s string) error {
	parsed, err := ParseTimeOfDay(s)
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}
