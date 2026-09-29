package main

import (
	"bufio"
	"encoding/csv"
	"strings"
	"testing"
)

func TestSeparatorFollowsTheHeaderRow(t *testing.T) {
	cases := map[string]rune{
		"generic_name,strength,dosage_form\nMetformin,500 mg,tablet\n":           ',',
		"generic_name;strength;dosage_form\r\nMetformin;500 mg;tablet\r\n":       ';',
		"\ufeffgeneric_name;strength;dosage_form\nParacetamol;120 mg/5 ml;syrup": ';',
		// Commas inside a later row do not change a semicolon header.
		"generic_name;strength;dosage_form\n\"Iron, folic acid\";1,5 mg;tablet\n": ';',
		"generic_name": ',',
		"":             ',',
	}
	for in, want := range cases {
		br := bufio.NewReader(strings.NewReader(in))
		if got := separator(br); got != want {
			t.Errorf("separator(%q) = %q, want %q", in, got, want)
		}
	}

	// Peeking must not consume anything the CSV reader needs.
	br := bufio.NewReader(strings.NewReader("generic_name;strength;dosage_form\nMetformin;500 mg;tablet\n"))
	r := csv.NewReader(br)
	r.Comma = separator(br)
	recs, err := r.ReadAll()
	if err != nil || len(recs) != 2 || recs[0][0] != "generic_name" || recs[1][1] != "500 mg" {
		t.Fatalf("records after peeking: %q, %v", recs, err)
	}
}
