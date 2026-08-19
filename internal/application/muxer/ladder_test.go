package muxer

import "testing"

func TestStrategyUnderBudgetPrefersRealSource(t *testing.T) {
	target := int64(10_000_000)
	if !strategyUnderBudget(&tierStrategy{kind: stratSource, estBits: 3_400_000}, target) {
		t.Fatal("expected an under-budget source to be preferred")
	}
	if strategyUnderBudget(&tierStrategy{kind: stratTranscode, estBits: 6_000_000}, target) {
		t.Fatal("transcode must not be treated as a real source")
	}
	if strategyUnderBudget(&tierStrategy{kind: stratSource, estBits: 11_300_000}, target) {
		t.Fatal("over-budget source must not be preferred")
	}
}
