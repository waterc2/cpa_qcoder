package main

import "testing"

func TestParseStoredModernFingerprint(t *testing.T) {
	raw := []byte(`{
		"type": "qoder",
		"secret": "dt-test",
		"region": "global",
		"checkin_fingerprint": {
			"machine_id": "mid-1",
			"machine_token": "mid-1",
			"machine_type": "5",
			"client_ip": "mid-1",
			"client_type": "10",
			"version": "1.1.69",
			"scene": "app",
			"product": "app",
			"data_policy": "agree",
			"machine_code": "mc-1"
		}
	}`)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	fp := sa.Fingerprint.toAccountFingerprint()
	if fp == nil {
		t.Fatal("modern fingerprint was dropped")
	}
	if fp.MachineID != "mid-1" || fp.MachineToken != "mid-1" || fp.ClientIP != "mid-1" {
		t.Fatalf("fingerprint identity not preserved: %+v", fp)
	}
	if fp.ClientType != "10" || fp.Version != "1.1.69" || fp.Scene != "app" ||
		fp.Product != "app" || fp.DataPolicy != "agree" || fp.MachineCode != "mc-1" {
		t.Fatalf("modern protocol fields not preserved: %+v", fp)
	}
}

func TestLegacyFingerprintStaysOnOldProtocol(t *testing.T) {
	// Check-in-only fingerprints have a machine_id but no machine_token. They
	// must not switch the bridge to the modern inference protocol.
	fp := &checkinFingerprint{MachineID: "mid-only", MachineType: "5"}
	if fp.hasRealFingerprint() {
		t.Fatal("machine_id without machine_token reported as modern fingerprint")
	}
	if got := fp.toAccountFingerprint(); got != nil {
		t.Fatalf("legacy fingerprint converted to modern: %+v", got)
	}
}

func TestStaticModelsForAuthAddsModernModelsOnlyWithFingerprint(t *testing.T) {
	has := func(models []string, want string) bool {
		for _, m := range models {
			if m == want {
				return true
			}
		}
		return false
	}
	ids := func(sa *storedAuth) []string {
		var out []string
		for _, m := range staticModelsForAuth(sa) {
			out = append(out, m.ID)
		}
		return out
	}

	legacy := ids(&storedAuth{})
	if has(legacy, "smodel") || has(legacy, "cmodel") || has(legacy, "fast") {
		t.Fatalf("legacy account exposed modern models: %v", legacy)
	}

	modern := ids(&storedAuth{Fingerprint: &checkinFingerprint{
		MachineID:    "mid-1",
		MachineToken: "mid-1",
		MachineType:  "5",
	}})
	for _, want := range []string{"smodel", "cmodel", "fast"} {
		if !has(modern, want) {
			t.Errorf("modern account missing %s in fallback catalog: %v", want, modern)
		}
	}
}

func TestFingerprintCacheKeyTracksFingerprintChanges(t *testing.T) {
	base := &checkinFingerprint{MachineID: "mid-1", MachineToken: "mid-1", MachineType: "5"}
	same := &checkinFingerprint{MachineID: "mid-1", MachineToken: "mid-1", MachineType: "5"}
	changed := &checkinFingerprint{MachineID: "mid-2", MachineToken: "mid-2", MachineType: "5"}

	k1 := fingerprintCacheKey(base.toAccountFingerprint())
	if k1 == "" {
		t.Fatal("empty cache key for a modern fingerprint")
	}
	if k2 := fingerprintCacheKey(same.toAccountFingerprint()); k2 != k1 {
		t.Fatalf("same fingerprint produced different cache keys: %q != %q", k1, k2)
	}
	if k3 := fingerprintCacheKey(changed.toAccountFingerprint()); k3 == k1 {
		t.Fatal("changed fingerprint reused the cached bridge key")
	}
	if got := fingerprintCacheKey((&checkinFingerprint{MachineID: "mid-only"}).toAccountFingerprint()); got != "" {
		t.Fatalf("legacy fingerprint got a modern cache key: %q", got)
	}
}
