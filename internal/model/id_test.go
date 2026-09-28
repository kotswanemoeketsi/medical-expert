package model

import "testing"

func TestIDRoundTrip(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	s := id.String()
	if len(s) != 36 {
		t.Fatalf("expected canonical 36-char form, got %q", s)
	}
	back, err := ParseID(s)
	if err != nil {
		t.Fatal(err)
	}
	if back != id {
		t.Fatal("round trip changed the value")
	}
	if id[6]&0xf0 != 0x40 {
		t.Error("expected version 4")
	}
}

func TestParseIDRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "abc", "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"} {
		if _, err := ParseID(s); err == nil {
			t.Errorf("ParseID accepted %q", s)
		}
	}
}

func TestIDScanFromBinary(t *testing.T) {
	orig := MustNewID()
	var got ID
	if err := got.Scan(orig.Bytes()); err != nil {
		t.Fatal(err)
	}
	if got != orig {
		t.Fatal("binary scan mismatch")
	}
}
