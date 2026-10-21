package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"qoder2api/account"
	"qoder2api/internal/bridge"
)

func TestParseStoredBarePAT(t *testing.T) {
	sa, err := parseStored([]byte("pt-abc123"))
	if err != nil {
		t.Fatalf("parse bare pat: %v", err)
	}
	if sa.Secret != "pt-abc123" || sa.AuthMode != "pat" {
		t.Fatalf("unexpected: %+v", sa)
	}
}

func TestParseStoredOAuth(t *testing.T) {
	raw := []byte(`{"type":"qoder","device_token":"dt-xyz","refresh_token":"drt-1","region":"cn","name":"alice"}`)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse oauth: %v", err)
	}
	if sa.AuthMode != "oauth" || sa.Region != "cn" || sa.Name != "alice" {
		t.Fatalf("unexpected: %+v", sa)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(sa.Secret), &payload); err != nil {
		t.Fatalf("secret not json: %v", err)
	}
	if payload["device_token"] != "dt-xyz" {
		t.Fatalf("secret device_token = %q", payload["device_token"])
	}
}

func TestParseStoredPluginShape(t *testing.T) {
	raw := []byte(`{"secret":"pt-1","region":"global","account_id":"u1","name":"bob","auth_mode":"pat"}`)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sa.AccountID != "u1" || authIDFor(sa) != "qoder-u1" {
		t.Fatalf("authID = %q", authIDFor(sa))
	}
}

func TestRegistration(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("not ok: %s", raw)
	}
	var reg registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatalf("registration: %v", err)
	}
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema = %d", reg.SchemaVersion)
	}
	if !reg.Capabilities.Executor || !reg.Capabilities.AuthProvider || !reg.Capabilities.ModelProvider {
		t.Fatalf("capabilities = %+v", reg.Capabilities)
	}
	if len(reg.Capabilities.ExecutorInputFormats) == 0 {
		t.Fatal("no input formats")
	}
}

func TestParseAuthOwnership(t *testing.T) {
	// Another provider's file must not be claimed.
	other := pluginapi.AuthParseRequest{Provider: "codex", FileName: "codex.json", RawJSON: []byte(`{"type":"codex","access_token":"x"}`)}
	raw, _ := json.Marshal(other)
	out, err := handleParseAuth(raw)
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}
	var env envelope
	json.Unmarshal(out, &env)
	var resp pluginapi.AuthParseResponse
	json.Unmarshal(env.Result, &resp)
	if resp.Handled {
		t.Fatal("claimed another provider's file")
	}
}

func TestParseStoredNested(t *testing.T) {
	// CPA auth-file convention nests provider fields under "auth".
	raw := []byte(`{"type":"qoder","provider":"qoder","disabled":true,"auth":{"secret":"pt-9","region":"global","account_id":"u9","auth_mode":"pat"}}`)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse nested: %v", err)
	}
	if sa.Secret != "pt-9" || sa.AccountID != "u9" || !sa.Disabled {
		t.Fatalf("unexpected: %+v", sa)
	}
}

func TestAuthFileDocRoundTrip(t *testing.T) {
	priority := 1
	sa := &storedAuth{Secret: "pt-1", Region: "global", AccountID: "u1", Name: "bob", AuthMode: "pat", Priority: &priority}
	doc := authFileDoc(sa)
	var probe struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
		Secret   string `json:"secret"`
	}
	if err := json.Unmarshal(doc, &probe); err != nil {
		t.Fatalf("doc: %v", err)
	}
	if probe.Type != providerName || probe.Provider != providerName || probe.Secret != "pt-1" {
		t.Fatalf("unexpected doc: %+v", probe)
	}
	back, err := parseStored(doc)
	if err != nil {
		t.Fatalf("round trip parse: %v", err)
	}
	if back.AccountID != "u1" || back.Secret != "pt-1" {
		t.Fatalf("round trip: %+v", back)
	}
	if back.Priority == nil || *back.Priority != 1 {
		t.Fatalf("priority did not round trip: %+v", back.Priority)
	}
}

func TestParseAuthOwn(t *testing.T) {
	req := pluginapi.AuthParseRequest{Provider: "qoder", FileName: "qoder-u1.json", RawJSON: []byte(`{"type":"qoder","secret":"pt-1","region":"global","account_id":"u1","name":"bob"}`)}
	raw, _ := json.Marshal(req)
	out, err := handleParseAuth(raw)
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}
	var env envelope
	json.Unmarshal(out, &env)
	var resp pluginapi.AuthParseResponse
	json.Unmarshal(env.Result, &resp)
	if !resp.Handled {
		t.Fatal("did not claim own file")
	}
	if resp.Auth.Provider != providerName {
		t.Fatalf("provider = %q", resp.Auth.Provider)
	}
	if resp.Auth.ID != "" {
		t.Fatalf("parse should leave ID empty, got %q", resp.Auth.ID)
	}
	if resp.Auth.FileName != "qoder-u1.json" {
		t.Fatalf("filename = %q", resp.Auth.FileName)
	}
}

func TestModelStatic(t *testing.T) {
	out, err := handleModelStatic([]byte(`{}`))
	if err != nil {
		t.Fatalf("model static: %v", err)
	}
	var env envelope
	json.Unmarshal(out, &env)
	var resp pluginapi.ModelResponse
	json.Unmarshal(env.Result, &resp)
	if resp.Provider != providerName || len(resp.Models) == 0 {
		t.Fatalf("unexpected model response: provider=%q models=%d", resp.Provider, len(resp.Models))
	}
	// The fallback catalog must expose human display names, not raw keys.
	for _, m := range resp.Models {
		if m.ID == "qmodel_38max" && m.DisplayName != "Qwen3.8-Max" {
			t.Fatalf("fallback display name for qmodel_38max = %q, want Qwen3.8-Max", m.DisplayName)
		}
		if m.DisplayName == "" || m.DisplayName == m.ID && strings.HasPrefix(m.ID, "q") && strings.HasSuffix(m.ID, "model") {
			t.Fatalf("raw-key display name leaked: id=%q name=%q", m.ID, m.DisplayName)
		}
	}
}

func TestClientNeedsSSEFrame(t *testing.T) {
	if clientNeedsSSEFrame(map[string]any{"request_path": "/v1/chat/completions"}) {
		t.Fatal("chat completions should not be framed")
	}
	if !clientNeedsSSEFrame(map[string]any{"request_path": "/v1/messages"}) {
		t.Fatal("claude messages should be framed")
	}
}

func TestModelSelectionFilter(t *testing.T) {
	account.SetDataRoot(t.TempDir())
	modelSelMu.Lock()
	modelSelData = nil
	modelSelMu.Unlock()
	defer func() {
		modelSelMu.Lock()
		modelSelData = nil
		modelSelMu.Unlock()
	}()

	all := staticModelsRaw()
	if len(all) < 2 {
		t.Fatalf("need at least 2 static models, got %d", len(all))
	}
	target := all[0].ID

	if err := saveModelSelection(&modelSelection{Disabled: []string{target}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	filtered := applyModelSelection(all)
	for _, m := range filtered {
		if m.ID == target {
			t.Fatalf("%s should be filtered out", target)
		}
	}
	if len(filtered) != len(all)-1 {
		t.Fatalf("filtered len = %d, want %d", len(filtered), len(all)-1)
	}
}

func TestModelConfigUpdate(t *testing.T) {
	account.SetDataRoot(t.TempDir())
	modelSelMu.Lock()
	modelSelData = nil
	modelSelMu.Unlock()
	defer func() {
		modelSelMu.Lock()
		modelSelData = nil
		modelSelMu.Unlock()
	}()

	body, _ := json.Marshal(map[string]any{"disabled": []string{"a", "b", "a", "", " A "}})
	out := handleModelConfigUpdate(pluginapi.ManagementRequest{Body: body})
	if err, ok := out["error"]; ok {
		t.Fatalf("update error: %v", err)
	}
	dis, _ := out["disabled"].([]string)
	// "a" and " A " dedupe case-insensitively, "" dropped.
	if len(dis) != 2 {
		t.Fatalf("disabled = %v (want 2 entries)", dis)
	}
}

// TestBuildQoderRequestModelOverride guards the P0 fix: CPA resolves
// oauth-model-alias entries before calling the executor and passes the real
// upstream key in ExecutorRequest.Model, while the payload keeps the client
// alias. The plugin must use the override for the upstream call.
func TestBuildQoderRequestModelOverride(t *testing.T) {
	body := []byte(`{"model":"qoder-auto","messages":[{"role":"user","content":"hi"}]}`)
	qr, err := buildQoderRequest(body, "auto", false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if qr.model != "auto" {
		t.Fatalf("upstream model = %q, want auto", qr.model)
	}
	if qr.clientModel != "qoder-auto" {
		t.Fatalf("client model = %q, want qoder-auto", qr.clientModel)
	}

	// Empty override must fall back to the payload model.
	qr2, err := buildQoderRequest(body, "", false)
	if err != nil {
		t.Fatalf("build fallback: %v", err)
	}
	if qr2.model != "qoder-auto" {
		t.Fatalf("fallback model = %q, want qoder-auto", qr2.model)
	}
}

// TestEnsureModelEnabled guards the P1 fix: the host keeps its old model
// registry after a selection change, so the executor must reject disabled
// models itself.
func TestEnsureModelEnabled(t *testing.T) {
	account.SetDataRoot(t.TempDir())
	modelSelMu.Lock()
	modelSelData = nil
	modelSelMu.Unlock()
	defer func() {
		modelSelMu.Lock()
		modelSelData = nil
		modelSelMu.Unlock()
	}()

	if err := saveModelSelection(&modelSelection{Disabled: []string{"efficient"}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := ensureModelEnabled(&qoderRequest{model: "efficient", clientModel: "qoder-efficient"}); err == nil {
		t.Fatal("disabled model was allowed")
	}
	if err := ensureModelEnabled(&qoderRequest{model: "auto", clientModel: "qoder-auto"}); err != nil {
		t.Fatalf("enabled model rejected: %v", err)
	}
}

// TestMergeCatalogPrefersLiveDisplayName guards the model-name fix: the static
// fallback used to be merged first, so the panel showed raw keys like
// "qmodel_38max" instead of the live display name "Qwen3.8-Max".
func TestMergeCatalogPrefersLiveDisplayName(t *testing.T) {
	live := []bridge.QoderModel{{Key: "qmodel_38max", DisplayName: "Qwen3.8-Max", Enable: true, PriceFactor: 0.2}}
	fallback := []bridge.QoderModel{
		{Key: "qmodel_38max", DisplayName: "qmodel_38max", Enable: true},
		{Key: "only-static", DisplayName: "Only Static", Enable: true},
	}
	out := mergeCatalog(live, fallback)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	byID := map[string]catalogModel{}
	for _, m := range out {
		byID[m.Info.ID] = m
	}
	if got := byID["qmodel_38max"].Info.DisplayName; got != "Qwen3.8-Max" {
		t.Fatalf("display name = %q, want Qwen3.8-Max", got)
	}
	if got := byID["qmodel_38max"].PriceFactor; got != 0.2 {
		t.Fatalf("price factor = %v, want 0.2", got)
	}
	if _, ok := byID["only-static"]; !ok {
		t.Fatal("static-only model missing")
	}
}

func TestDetectAuthMode(t *testing.T) {
	cases := map[string]string{
		`{"device_token":"dt-a"}`: "oauth",
		"dt-abc":                  "oauth",
		"drt-xyz":                 "oauth",
		"pt-123":                  "pat",
		"":                        "pat",
	}
	for secret, want := range cases {
		if got := detectAuthMode(secret); got != want {
			t.Fatalf("detectAuthMode(%q) = %q, want %q", secret, got, want)
		}
	}
}

// TestConfigUpdatePersists verifies panel config changes are written to
// plugin_settings.json and reloaded on the next configure.
func TestConfigUpdatePersists(t *testing.T) {
	account.SetDataRoot(t.TempDir())
	configMu.Lock()
	activeConfig = pluginConfig{CheckinTimes: []string{"10:00"}, DefaultRegion: "global"}
	configMu.Unlock()
	defer func() {
		configMu.Lock()
		activeConfig = pluginConfig{CheckinTimes: []string{"10:00"}, DefaultRegion: "global"}
		configMu.Unlock()
	}()

	body, _ := json.Marshal(map[string]any{
		"checkin_auto":     true,
		"checkin_times":    []string{"08:00", "20:00"},
		"checkin_on_start": true,
		"default_region":   "cn",
	})
	if out := handleConfigUpdate(pluginapi.ManagementRequest{Body: body}); out["error"] != nil {
		t.Fatalf("update error: %v", out["error"])
	}
	if _, err := os.Stat(persistedConfigPath()); err != nil {
		t.Fatalf("settings file not written: %v", err)
	}

	// Reset and reload as if CPA restarted.
	configMu.Lock()
	activeConfig = pluginConfig{CheckinTimes: []string{"10:00"}, DefaultRegion: "global"}
	configMu.Unlock()
	loadPersistedConfig()

	cfg := currentConfig()
	if !cfg.AutoCheckin || !cfg.CheckinOnStart {
		t.Fatalf("booleans not restored: %+v", cfg)
	}
	if cfg.DefaultRegion != "cn" {
		t.Fatalf("region = %q, want cn", cfg.DefaultRegion)
	}
	if len(cfg.CheckinTimes) != 2 || cfg.CheckinTimes[0] != "08:00" {
		t.Fatalf("times = %v", cfg.CheckinTimes)
	}
}

func TestParseSecretTokens(t *testing.T) {
	cases := []struct {
		name    string
		secret  string
		wantDev string
		wantRef string
	}{
		{"json", `{"device_token":"dt-abc","refresh_token":"drt-xyz"}`, "dt-abc", "drt-xyz"},
		{"json spaces", `  {"device_token":"dt-abc","refresh_token":"drt-xyz"}  `, "dt-abc", "drt-xyz"},
		{"bare device", "dt-abc", "dt-abc", ""},
		{"bare refresh", "drt-xyz", "", "drt-xyz"},
		{"pat", "pt-123", "", ""},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dev, ref := parseSecretTokens(tc.secret)
			if dev != tc.wantDev || ref != tc.wantRef {
				t.Fatalf("parseSecretTokens(%q) = (%q, %q), want (%q, %q)", tc.secret, dev, ref, tc.wantDev, tc.wantRef)
			}
		})
	}
}

func resetModelSelectionForTest(t *testing.T) {
	t.Helper()
	modelSelMu.Lock()
	modelSelData = nil
	modelSelMu.Unlock()
	t.Cleanup(func() {
		modelSelMu.Lock()
		modelSelData = nil
		modelSelMu.Unlock()
	})
}

func TestModelAliasPersistenceAndView(t *testing.T) {
	account.SetDataRoot(t.TempDir())
	resetModelSelectionForTest(t)

	if err := saveModelSelection(&modelSelection{Aliases: map[string]string{"gmodel": "my-glm"}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := modelAliasFor("gmodel"); got != "my-glm" {
		t.Fatalf("modelAliasFor(gmodel) = %q", got)
	}
	if upstream, ok := resolveModelAlias("my-glm"); !ok || upstream != "gmodel" {
		t.Fatalf("resolveModelAlias(my-glm) = (%q, %v)", upstream, ok)
	}

	models := applyModelAliases(staticModelsRaw())
	found := false
	for _, m := range models {
		if m.ID == "my-glm" {
			found = true
			if m.Name != "my-glm" || m.DisplayName != "my-glm" {
				t.Fatalf("alias metadata not applied: %+v", m)
			}
		}
		if m.ID == "gmodel" {
			t.Fatalf("original id leaked after alias: %+v", m)
		}
	}
	if !found {
		t.Fatal("aliased model missing from exposed list")
	}
}

func TestModelConfigUpdateAliases(t *testing.T) {
	account.SetDataRoot(t.TempDir())
	resetModelSelectionForTest(t)

	body, _ := json.Marshal(map[string]any{
		"aliases": map[string]string{"gmodel": "my-glm"},
	})
	out := handleModelConfigUpdate(pluginapi.ManagementRequest{Body: body})
	if err, ok := out["error"]; ok {
		t.Fatalf("update error: %v", err)
	}
	if got := modelAliasFor("gmodel"); got != "my-glm" {
		t.Fatalf("alias not saved: %q", got)
	}
	models, _ := out["models"].([]map[string]any)
	for _, m := range models {
		if m["id"] == "gmodel" && m["alias"] != "my-glm" {
			t.Fatalf("view alias = %v", m["alias"])
		}
	}

	// An alias must not collide with another model's exposed name.
	bad, _ := json.Marshal(map[string]any{
		"aliases": map[string]string{"gmodel": "auto"},
	})
	if out := handleModelConfigUpdate(pluginapi.ManagementRequest{Body: bad}); out["error"] == nil {
		t.Fatal("expected alias collision error")
	}
}

func TestBuildQoderRequestPluginAlias(t *testing.T) {
	account.SetDataRoot(t.TempDir())
	resetModelSelectionForTest(t)
	if err := saveModelSelection(&modelSelection{Aliases: map[string]string{"gmodel": "my-glm"}}); err != nil {
		t.Fatalf("save: %v", err)
	}

	body := []byte(`{"model":"my-glm","messages":[{"role":"user","content":"hi"}]}`)
	qr, err := buildQoderRequest(body, "my-glm", false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if qr.model != "gmodel" || qr.clientModel != "my-glm" {
		t.Fatalf("alias routing = model %q client %q", qr.model, qr.clientModel)
	}
	if !qr.opts.SkipModelMap {
		t.Fatal("explicit alias must skip bridge.MapModel")
	}

	// Host may resolve the plugin alias before invoking the executor.
	qr2, err := buildQoderRequest(body, "gmodel", false)
	if err != nil {
		t.Fatalf("build resolved: %v", err)
	}
	if qr2.model != "gmodel" || !qr2.opts.SkipModelMap {
		t.Fatalf("resolved alias = model %q skip %v", qr2.model, qr2.opts.SkipModelMap)
	}
}

func resetUsageStoreForTest(t *testing.T) {
	t.Helper()
	usageStore = &requestUsageStore{perDay: map[string]map[string]*usageDayAgg{}}
	usageMetaCache = &usageMetaResolver{}
	t.Cleanup(func() {
		usageStore = &requestUsageStore{perDay: map[string]map[string]*usageDayAgg{}}
		usageMetaCache = &usageMetaResolver{}
	})
}

func TestUsagePluginRecordsAndQueries(t *testing.T) {
	account.SetDataRoot(t.TempDir())
	resetUsageStoreForTest(t)

	rec := pluginapi.UsageRecord{
		Provider:    providerName,
		Model:       "gmodel",
		Alias:       "my-glm",
		AuthIndex:   "auth-1",
		AuthID:      "qoder-u1",
		RequestedAt: time.Now().Add(-2 * time.Second),
		Latency:     1500 * time.Millisecond,
		Detail: pluginapi.UsageDetail{
			InputTokens:  12,
			OutputTokens: 34,
			CachedTokens: 5,
			TotalTokens:  46,
		},
	}
	raw, _ := json.Marshal(rec)
	if _, err := handleUsage(raw); err != nil {
		t.Fatalf("handleUsage: %v", err)
	}

	out := handleUsageQuery(pluginapi.ManagementRequest{Query: map[string][]string{"limit": {"10"}}})
	records, _ := out["records"].([]requestUsageRecord)
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	got := records[0]
	if got.Model != "gmodel" || got.Alias != "my-glm" || got.AuthIndex != "auth-1" {
		t.Fatalf("record = %+v", got)
	}
	if got.InputTokens != 12 || got.OutputTokens != 34 || got.CachedTokens != 5 || got.TotalTokens != 46 || got.LatencyMS != 1500 {
		t.Fatalf("tokens/latency = %+v", got)
	}
	totals, _ := out["totals"].(map[string]any)
	if totals["today_requests"] != int64(1) || totals["total_requests"] != int64(1) {
		t.Fatalf("totals = %+v", totals)
	}

	// A failed record is retained and counted.
	failRec := pluginapi.UsageRecord{
		Provider:    providerName,
		Model:       "auto",
		AuthIndex:   "auth-2",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     pluginapi.UsageFailure{StatusCode: 502, Body: "upstream unavailable"},
	}
	failRaw, _ := json.Marshal(failRec)
	if _, err := handleUsage(failRaw); err != nil {
		t.Fatalf("handleUsage failed record: %v", err)
	}
	out = handleUsageQuery(pluginapi.ManagementRequest{Query: map[string][]string{"limit": {"10"}}})
	records, _ = out["records"].([]requestUsageRecord)
	if len(records) != 2 || !records[0].Failed || records[0].StatusCode != 502 {
		t.Fatalf("failed record = %+v", records)
	}
}

func TestUsageStorePersistsAcrossReload(t *testing.T) {
	account.SetDataRoot(t.TempDir())
	resetUsageStoreForTest(t)

	rec := pluginapi.UsageRecord{
		Provider:    providerName,
		Model:       "auto",
		AuthIndex:   "auth-persist",
		RequestedAt: time.Now(),
		Detail:      pluginapi.UsageDetail{InputTokens: 5, OutputTokens: 6, TotalTokens: 11},
	}
	raw, _ := json.Marshal(rec)
	if _, err := handleUsage(raw); err != nil {
		t.Fatalf("handleUsage: %v", err)
	}
	path := usageStore.path
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("usage file not written: %v", err)
	}

	// Simulate a plugin restart: new store, same data dir.
	usageStore = &requestUsageStore{perDay: map[string]map[string]*usageDayAgg{}}
	out := handleUsageQuery(pluginapi.ManagementRequest{Query: map[string][]string{"limit": {"10"}}})
	records, _ := out["records"].([]requestUsageRecord)
	if len(records) != 1 || records[0].TotalTokens != 11 {
		t.Fatalf("reloaded records = %+v", records)
	}
}
