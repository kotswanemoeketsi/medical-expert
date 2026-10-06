package model

import "testing"

func rate(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

func TestResponseAndAdherenceAreDifferentFacts(t *testing.T) {
	// A patient who answers every reminder with "skipped".
	skipper := DoseCounts{Skipped: 10}
	if rate(skipper.ResponseRate()) != 1 || rate(skipper.AdherenceRate()) != 0 {
		t.Fatalf("skipper: response %v, adherence %v", rate(skipper.ResponseRate()), rate(skipper.AdherenceRate()))
	}
	mixed := DoseCounts{Taken: 6, Skipped: 1, Missed: 3}
	if rate(mixed.ResponseRate()) != 0.7 || rate(mixed.AdherenceRate()) != 0.6 {
		t.Fatalf("mixed: %v %v", rate(mixed.ResponseRate()), rate(mixed.AdherenceRate()))
	}
	third := DoseCounts{Taken: 1, Missed: 2}
	if rate(third.AdherenceRate()) != 0.333 {
		t.Fatalf("rounding: %v", rate(third.AdherenceRate()))
	}
	if (DoseCounts{}).ResponseRate() != nil {
		t.Fatal("no doses must read as no data, not 0%")
	}
	p := NewAdherencePeriod(30, mixed)
	if p.Days != 30 || rate(p.ResponseRate) != 0.7 || p.Due() != 10 {
		t.Fatalf("period: %+v", p)
	}
}

func TestResponseBuckets(t *testing.T) {
	var b ResponseBuckets
	for _, c := range [][2]int{{0, 10}, {1, 10}, {4, 10}, {5, 10}, {7, 10}, {8, 10}, {9, 10}, {10, 10}, {0, 0}, {3, 3}} {
		b.Add(c[0], c[1])
	}
	want := ResponseBuckets{None: 1, Low: 2, Mid: 2, High: 2, Complete: 2}
	if b != want {
		t.Fatalf("buckets %+v, want %+v", b, want)
	}
	// 79.9% is still under 80.
	var edge ResponseBuckets
	edge.Add(799, 1000)
	if edge.Mid != 1 {
		t.Fatalf("79.9%% landed in %+v", edge)
	}
}
