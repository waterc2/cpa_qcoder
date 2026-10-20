package account

import (
	"encoding/json"
	"testing"
)

// TestParseQuotaPayloadProTrial guards the fix for "Available Credits": a Pro
// Trial account has userQuota.remaining=0 but addOnQuota.remaining=83, and the
// official CLI reports 83 (sum of every bucket). The plugin used to show 0.
func TestParseQuotaPayloadProTrial(t *testing.T) {
	raw := `{
		"expiresAt": 1793305019110,
		"isQuotaExceeded": false,
		"usageType": "credits",
		"userQuota": {"total": 300, "used": 300, "remaining": 0, "percentage": 1, "unit": "credits"},
		"addOnQuota": {"total": 200, "used": 117, "remaining": 83, "percentage": 0.59, "unit": "credits"}
	}`
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	info := ParseQuotaPayload(result, "Pro Trial")
	if info.Plan != "Pro Trial" {
		t.Fatalf("plan = %q", info.Plan)
	}
	if info.AvailableCredits != 83 {
		t.Fatalf("available credits = %v, want 83", info.AvailableCredits)
	}
	if info.AddonQuota == nil || info.AddonQuota.Remaining != 83 {
		t.Fatalf("addon quota = %+v", info.AddonQuota)
	}
	if info.UserQuota == nil || info.UserQuota.Total != 300 {
		t.Fatalf("user quota = %+v", info.UserQuota)
	}
}

// TestParseQuotaPayloadWrapped covers /sash/api/v2/me/usage, which nests the
// payload under "qoderUsage".
func TestParseQuotaPayloadWrapped(t *testing.T) {
	raw := `{"displayMode":"qoder","qoderUsage":{"isQuotaExceeded":true,"userQuota":{"total":0,"used":0,"remaining":0}}}`
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	info := ParseQuotaPayload(result, "Free")
	if !info.IsQuotaExceeded {
		t.Fatal("expected is_quota_exceeded")
	}
	// Zero buckets are dropped, so available credits stay 0.
	if info.AvailableCredits != 0 {
		t.Fatalf("available credits = %v, want 0", info.AvailableCredits)
	}
	if info.UserQuota != nil {
		t.Fatalf("zero user quota should be nil, got %+v", info.UserQuota)
	}
}

func TestParseQuotaPayloadOrgBucket(t *testing.T) {
	raw := `{"orgResourcePackage":{"total":50,"used":10,"remaining":40,"unit":"credits"}}`
	var result map[string]interface{}
	_ = json.Unmarshal([]byte(raw), &result)
	info := ParseQuotaPayload(result, "Team")
	if info.OrgResourcePackage == nil || info.OrgResourcePackage.Remaining != 40 {
		t.Fatalf("org bucket = %+v", info.OrgResourcePackage)
	}
	if info.AvailableCredits != 40 {
		t.Fatalf("available credits = %v, want 40", info.AvailableCredits)
	}
}

func TestParseQuotaPayloadDedicatedPackages(t *testing.T) {
	raw := `{
		"userQuota": {"total": 300, "used": 300, "remaining": 0, "unit": "credits"},
		"dedicatedResourcePackages": [{
			"id": "pkg_sota",
			"name": "SOTA 专属积分",
			"used": 200,
			"total": 2000,
			"remaining": 1800,
			"unit": "credits",
			"expiresAt": 1793305019110,
			"available": true,
			"status": "active",
			"displayLabels": [{"dimension": "title", "value": "SOTA", "valueI18n": {"zh-CN": "SOTA 专属积分"}}]
		}]
	}`
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	info := ParseQuotaPayload(result, "Pro Trial")
	if len(info.DedicatedPackages) != 1 {
		t.Fatalf("dedicated packages = %+v", info.DedicatedPackages)
	}
	p := info.DedicatedPackages[0]
	if p.ID != "pkg_sota" || p.Remaining != 1800 || !p.Available || p.ExpiresAtMs == 0 {
		t.Fatalf("package = %+v", p)
	}
	if len(p.Labels) != 1 || p.Labels[0].ValueI18n["zh-CN"] != "SOTA 专属积分" {
		t.Fatalf("labels = %+v", p.Labels)
	}
	// 专属资源包单独成桶，不并入 AvailableCredits（与上游语义一致）。
	if info.AvailableCredits != 0 {
		t.Fatalf("available credits = %v, want 0", info.AvailableCredits)
	}
}
