package main

import "testing"

// TestFallbackDeepSeekNotFree guards the 2026-11-02 upstream repricing.
//
// The live Global /algo/api/v2/model/list (fetched for every deployed account
// on 2026-11-02) sends dfmodel price_factor=0.1: DeepSeek-Flash lost its free
// entry. Before this commit the fallback catalog still advertised dfmodel at
// 0, so the panel and the pre-first-query catalog told the operator DeepSeek
// was free while every request was actually being billed against the account's
// credit bucket.
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
	}
	for k := range want {
		if !seen[k] {
			t.Fatalf("fallback catalog lost %s", k)
		}
	}
}

// TestFallbackCatalogHasNoFreeModel locks the 2026-11-14 repricing.
//
// Upstream 5bd4083 (live pull 2026-11-13 CN / 2026-11-14 Global) ends the
// limited-free promo: qfmodel price_factor 0 → 0.1, which was the last
// 0-priced row this plugin shipped. After that pull the Global catalog has no
// free model at all, so any 0 in the fallback table is stale and actively
// hides that the traffic bills against the account's credit bucket — the same
// silent-billing class the 2026-11-02 dfmodel fix addressed.
func TestFallbackCatalogHasNoFreeModel(t *testing.T) {
	for _, m := range fallbackModels {
		if m.PriceFactor == 0 {
			t.Fatalf("%s PriceFactor = 0: the last free model (qfmodel) went paid 0.1 upstream on 2026-11-14; a 0 here advertises a free model that bills", m.Key)
		}
	}
}

// TestFallbackCatalogMatches20261114Snapshot pins the rows that changed in the
// 2026-11-14 catalog refresh, so a future resync cannot silently drop the new
// models or restore the stale Qwen3.7-Max row under the qmodel_latest key.
func TestFallbackCatalogMatches20261114Snapshot(t *testing.T) {
	want := map[string]struct {
		display string
		price   float64
		maxOut  int
		reason  bool
	}{
		"qmodel_latest": {"Qwen4-Max-Preview", 0.1, 32768, true},
		"jmodel":        {"Step-5-Preview", 0.3, 32768, true},
		"mmodel":        {"MiniMax-M3.1-Flash", 0.1, 32768, true},
		"qfmodel":       {"Qwen3.8-Flash", 0.1, 32768, true},
	}
	seen := map[string]bool{}
	for _, m := range fallbackModels {
		exp, ok := want[m.Key]
		if !ok {
			continue
		}
		seen[m.Key] = true
		if m.DisplayName != exp.display || m.PriceFactor != exp.price ||
			m.MaxOutputTokens != exp.maxOut || m.IsReasoning != exp.reason {
			t.Errorf("%s = {display:%q price:%v maxOut:%d reasoning:%v}, want {display:%q price:%v maxOut:%d reasoning:%v} (2026-11-14 catalog snapshot)",
				m.Key, m.DisplayName, m.PriceFactor, m.MaxOutputTokens, m.IsReasoning,
				exp.display, exp.price, exp.maxOut, exp.reason)
		}
	}
	for k := range want {
		if !seen[k] {
			t.Fatalf("fallback catalog lost %s (present in the 2026-11-14 Global catalog)", k)
		}
	}
}
