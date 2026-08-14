package muxer

import "testing"

func TestFindMatchingPair(t *testing.T) {
	// Scenario: best video (V0) is an extended cut (9000s), best audio (A0)
	// is the theatrical cut (8160s) — they don't match. The second-best video
	// (V1) is the theatrical cut and matches A0.
	vi, ai := findMatchingPair([]float64{9000, 8160, 8000}, []float64{8160, 8100})
	if vi != 1 || ai != 0 {
		t.Errorf("expected (1, 0), got (%d, %d)", vi, ai)
	}
}

func TestFindMatchingPairSecondAudio(t *testing.T) {
	// V0=8160 vs A0=9000 don't match. Pull V1=9000: it matches A0=9000 first
	// (per the algorithm, the next best video is tried before the next best
	// audio). So the pair is (V1, A0) = (1, 0).
	vi, ai := findMatchingPair([]float64{8160, 9000, 7800}, []float64{9000, 8160})
	if vi != 1 || ai != 0 {
		t.Errorf("expected (1, 0), got (%d, %d)", vi, ai)
	}
}

func TestFindMatchingPairCrossComparison(t *testing.T) {
	// V0=9000, A0=9000, A1=7800. V0 and A0 match immediately.
	vi, ai := findMatchingPair([]float64{9000, 8000}, []float64{9000, 7800})
	if vi != 0 || ai != 0 {
		t.Errorf("expected (0, 0), got (%d, %d)", vi, ai)
	}
}

func TestFindMatchingPairNoMatchFallsBack(t *testing.T) {
	// No durations are close enough to each other within 95%.
	vi, ai := findMatchingPair([]float64{9000, 8800}, []float64{7800, 8000})
	if vi != -1 || ai != -1 {
		t.Errorf("expected (-1, -1), got (%d, %d)", vi, ai)
	}
}

func TestFindMatchingPairSkipsUnknown(t *testing.T) {
	// Unknown durations (-1) are skipped; V1 (8160) matches A0 (8160).
	vi, ai := findMatchingPair([]float64{-1, 8160}, []float64{8160})
	if vi != 1 || ai != 0 {
		t.Errorf("expected (1, 0), got (%d, %d)", vi, ai)
	}
}

func TestFindMatchingPairEmpty(t *testing.T) {
	vi, ai := findMatchingPair(nil, []float64{8160})
	if vi != -1 {
		t.Errorf("expected no match for empty videos, got (%d, %d)", vi, ai)
	}
	vi, ai = findMatchingPair([]float64{8160}, nil)
	if vi != -1 {
		t.Errorf("expected no match for empty audios, got (%d, %d)", vi, ai)
	}
}
