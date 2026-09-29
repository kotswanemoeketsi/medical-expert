// Package phone normalises telephone numbers to E.164.
//
// Getting this wrong means silent non-delivery of reminders, which looks
// exactly like a patient ignoring them, so numbers are normalised once, at
// capture, and stored in one form.
package phone

import (
	"errors"
	"strings"
)

// ErrInvalid means the input cannot be read as a telephone number.
var ErrInvalid = errors.New("is not a valid phone number")

// NormalizeE164 accepts the ways South Africans actually write numbers —
// "082 123 4567", "+27 82 123 4567", "27821234567", "0027821234567",
// "(082) 123-4567" — and returns "+27821234567". Numbers already in
// international form for other countries are kept, e.g. "+263 77 123 4567".
func NormalizeE164(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ErrInvalid
	}
	plus := strings.HasPrefix(s, "+")
	var digits strings.Builder
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' && i == 0:
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.' || r == '/':
		default:
			return "", ErrInvalid
		}
	}
	d := digits.String()

	switch {
	case plus:
		// already international
	case strings.HasPrefix(d, "00"):
		d = d[2:]
	case strings.HasPrefix(d, "0") && len(d) == 10:
		d = "27" + d[1:]
	case strings.HasPrefix(d, "27") && len(d) == 11:
		// international without the plus
	default:
		return "", ErrInvalid
	}

	if len(d) < 8 || len(d) > 15 || d[0] == '0' {
		return "", ErrInvalid
	}
	// South African numbers are always 27 + nine digits, and the national
	// number never starts with 0.
	if strings.HasPrefix(d, "27") && (len(d) != 11 || d[2] == '0') {
		return "", ErrInvalid
	}
	return "+" + d, nil
}

// Display formats a stored E.164 number for print. South African numbers are
// shown the way South Africans write them (033 555 0199); others are left
// in international form.
func Display(e164 string) string {
	if strings.HasPrefix(e164, "+27") && len(e164) == 12 {
		n := "0" + e164[3:]
		return n[:3] + " " + n[3:6] + " " + n[6:]
	}
	return e164
}

// LooksLikeNumber reports whether a search term is probably a phone number
// rather than a name, so a search can match on the normalised form.
func LooksLikeNumber(s string) bool {
	digits := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')':
		default:
			return false
		}
	}
	return digits >= 9
}
