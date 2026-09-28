package model

import (
	"errors"
	"time"
)

// South African identity numbers are thirteen digits: YYMMDD SSSS C A Z.
//
//	YYMMDD  date of birth
//	SSSS    sequence (historically 0000-4999 female, 5000-9999 male)
//	C       citizenship: 0 citizen, 1 permanent resident, 2 refugee
//	A       formerly race, now always 8 or 9 on new numbers
//	Z       Luhn check digit
//
// The sex digits are deliberately not checked against the recorded sex: a
// person's legal sex can change and the old number stays valid.

var (
	ErrSAIDFormat   = errors.New("must be 13 digits")
	ErrSAIDChecksum = errors.New("check digit is wrong; the number was probably mistyped")
	ErrSAIDDate     = errors.New("does not contain a valid date of birth")
)

// ValidateSAID checks format, date and Luhn checksum.
func ValidateSAID(id string) error {
	if len(id) != 13 {
		return ErrSAIDFormat
	}
	for i := 0; i < 13; i++ {
		if id[i] < '0' || id[i] > '9' {
			return ErrSAIDFormat
		}
	}
	if _, ok := saidDate(id); !ok {
		return ErrSAIDDate
	}
	if !luhnValid(id) {
		return ErrSAIDChecksum
	}
	return nil
}

// SAIDMatchesDOB reports whether the YYMMDD prefix agrees with a date of
// birth. The century is ambiguous in the number itself, so only the last two
// digits of the year are compared.
func SAIDMatchesDOB(id string, dob Date) bool {
	d, ok := saidDate(id)
	if !ok {
		return false
	}
	return d.Year%100 == dob.Year%100 && d.Month == dob.Month && d.Day == dob.Day
}

func saidDate(id string) (Date, bool) {
	yy := int(id[0]-'0')*10 + int(id[1]-'0')
	mm := int(id[2]-'0')*10 + int(id[3]-'0')
	dd := int(id[4]-'0')*10 + int(id[5]-'0')
	// Pick a century only to validate the day of month (29 February).
	year := 2000 + yy
	if mm < 1 || mm > 12 || dd < 1 {
		return Date{}, false
	}
	t := time.Date(year, time.Month(mm), dd, 0, 0, 0, 0, time.UTC)
	if t.Day() != dd {
		// 29 Feb in a non-leap 20yy may be valid in 19yy; try that century.
		t = time.Date(1900+yy, time.Month(mm), dd, 0, 0, 0, 0, time.UTC)
		if t.Day() != dd {
			return Date{}, false
		}
		year = 1900 + yy
	}
	return Date{Year: year, Month: time.Month(mm), Day: dd}, true
}

func luhnValid(digits string) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}
