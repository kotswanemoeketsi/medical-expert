package phone

import "testing"

func TestNormalizeE164(t *testing.T) {
	good := map[string]string{
		"082 123 4567":      "+27821234567",
		"0821234567":        "+27821234567",
		"+27 82 123 4567":   "+27821234567",
		"+27821234567":      "+27821234567",
		"27821234567":       "+27821234567",
		"0027821234567":     "+27821234567",
		"(082) 123-4567":    "+27821234567",
		"011 555 0123":      "+27115550123",
		"+263 77 123 4567":  "+263771234567",
		"+44 20 7946 0958":  "+442079460958",
		" 072-000-1111 ":    "+27720001111",
		"0027 72 000 1111":  "+27720001111",
		"+1 (202) 555-0147": "+12025550147",
	}
	for in, want := range good {
		got, err := NormalizeE164(in)
		if err != nil || got != want {
			t.Errorf("NormalizeE164(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "abc", "082 123 456", "082 123 45678", "12345", "+27 82 123 456",
		"+27 82 123 45678", "082-CALL-NOW", "0000000000", "+0821234567",
	}
	for _, in := range bad {
		if got, err := NormalizeE164(in); err == nil {
			t.Errorf("NormalizeE164(%q) accepted as %q", in, got)
		}
	}
}

func TestLooksLikeNumber(t *testing.T) {
	if !LooksLikeNumber("082 123 4567") || !LooksLikeNumber("+27821234567") {
		t.Error("numbers not recognised")
	}
	if LooksLikeNumber("Nkosi") || LooksLikeNumber("8001") || LooksLikeNumber("Thabo 0821234567") {
		t.Error("non-numbers recognised as numbers")
	}
}
