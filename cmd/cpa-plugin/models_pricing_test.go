package main

import "testing"

// TestFallbackDeepSeekNotFree guards the 2026-11-02 upstream repricing.
//
// The live Global /algo/api/v2/model/list (fetched for every deployed account
// on 2026-11-02) sends dfmodel price_factor=0.1: DeepSeek-Flash lost its free
// entry. The only 0-priced entry left in that list is qfmodel. Before this
// commit the fallback catalog still advertised dfmodel at 0, so the panel and
// the pre-first-query catalog told the operator DeepSeek was free while every
// request was actually being billed against the account's credit bucket.
//
// This test fails if dfmodel drifts back to 0 (i.e. someone re-syncs the table
// from the stale 2026-10 snapshot) and if a new DeepSeek entry is added as a
// free model without a live re-snapshot behind it.
func TestFallbackDeepSeekNotFree(t *testing.T) {
	want := map[string]float64{
		"dfmodel": 0.1, // DeepSeek-Flash — was 0 until 2026-11-02
		"dmodel":  0.5, // DeepSeek-V4-Pro
	}
	seen := map[string]bool{}
	for _, m := range fallbackModels {
		if exp, ok := want[m.Key]; ok {
			seen[m.Key] = true
			if m.PriceFactor != exp {
				t.Fatalf("%s PriceFactor = %v, want %v (DeepSeek is not free upstream since 2026-11-02)", m.Key, m.PriceFactor, exp)
			}
		}
		if m.Key == "qfmodel" && m.PriceFactor != 0 {
			t.Fatalf("qfmodel PriceFactor = %v, want 0 (still the only free model upstream)", m.PriceFactor)
		}
	}
	for k := range want {
		if !seen[k] {
			t.Fatalf("fallback catalog lost %s", k)
		}
	}
}
