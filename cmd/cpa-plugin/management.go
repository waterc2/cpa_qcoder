package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"qoder2api/account"
)

// -----------------------------------------------------------------------------
// Management base path
// -----------------------------------------------------------------------------

var (
	managementBasePathCache   = "/v0/management"
	managementBasePathCacheMu sync.RWMutex
)

func loadedManagementBasePath() string {
	managementBasePathCacheMu.RLock()
	defer managementBasePathCacheMu.RUnlock()
	return managementBasePathCache
}

func setManagementBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	managementBasePathCacheMu.Lock()
	managementBasePathCache = p
	managementBasePathCacheMu.Unlock()
}

// -----------------------------------------------------------------------------
// Route registration
// -----------------------------------------------------------------------------

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List Qoder accounts (region, plan, quota, check-in streak)."},
			{Method: http.MethodGet, Path: base + "/accounts/quotas", Description: "Fetch live credits for every account (parallel)."},
			{Method: http.MethodPost, Path: base + "/checkin", Description: "Check in one account (account_id/auth_index) or all accounts."},
			{Method: http.MethodGet, Path: base + "/account/quota", Description: "Fetch live quota for one account (account_id/auth_index)."},
			{Method: http.MethodPost, Path: base + "/import", Description: "Import a Qoder PAT or OAuth credential into the host auth store."},
			{Method: http.MethodPost, Path: base + "/accounts/delete", Description: "Delete one Qoder account (account_id/auth_index)."},
			{Method: http.MethodGet, Path: base + "/checkin/config", Description: "Get auto check-in configuration."},
			{Method: http.MethodPost, Path: base + "/checkin/config", Description: "Update auto check-in configuration."},
			{Method: http.MethodGet, Path: base + "/models/config", Description: "List selectable models, exposure flags and mapping names."},
			{Method: http.MethodPost, Path: base + "/models/config", Description: "Save model selection and aliases (body: {disabled: [...], aliases: {modelId: alias}})."},
			{Method: http.MethodGet, Path: base + "/usage", Description: "Per-request Qoder usage log. Query: auth_index, limit (default 100), from, to."},
			{Method: http.MethodGet, Path: base + "/scheduler", Description: "Show per-account in-flight request counts and the concurrency limit."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "Qoder", Description: "Qoder account and check-in dashboard."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Browser resource route (unauthenticated).
	resPrefix := "/v0/resource/plugins/" + providerName
	if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
		return okEnvelope(mgmtHTMLResponse(panelHTML))
	}

	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch {
	case req.Method == http.MethodGet && path == base+"/accounts":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, map[string]any{"accounts": listAccounts()}))
	case req.Method == http.MethodGet && path == base+"/accounts/quotas":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleAllQuotas()))
	case req.Method == http.MethodPost && path == base+"/checkin":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCheckin(req)))
	case req.Method == http.MethodGet && path == base+"/account/quota":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleQuota(req)))
	case req.Method == http.MethodPost && path == base+"/import":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleImport(req)))
	case req.Method == http.MethodPost && path == base+"/accounts/delete":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleDelete(req)))
	case req.Method == http.MethodGet && path == base+"/checkin/config":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, configView()))
	case req.Method == http.MethodPost && path == base+"/checkin/config":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleConfigUpdate(req)))
	case req.Method == http.MethodGet && path == base+"/models/config":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, modelConfigView()))
	case req.Method == http.MethodPost && path == base+"/models/config":
		out := handleModelConfigUpdate(req)
		status := http.StatusOK
		if out["error"] != nil {
			status = http.StatusBadRequest
		}
		return okEnvelope(mgmtJSONResponse(status, out))
	case req.Method == http.MethodGet && path == base+"/usage":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleUsageQuery(req)))
	case req.Method == http.MethodGet && path == base+"/scheduler":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleSchedulerStatus()))
	case req.Method == http.MethodPost && path == base+"/scheduler/queue-breaker":
		out := handleQueueBreakerUpdate(req)
		status := http.StatusOK
		if out["error"] != nil {
			status = http.StatusBadRequest
		}
		return okEnvelope(mgmtJSONResponse(status, out))
	}
	return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
}

// -----------------------------------------------------------------------------
// Host auth store RPC
// -----------------------------------------------------------------------------

type rpcHostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type rpcHostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
}

func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostAuthList)
	if err != nil {
		return nil, err
	}
	var resp rpcHostAuthListResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("%s: decode result: %w", pluginabi.MethodHostAuthList, err)
	}
	out := make([]pluginapi.HostAuthFileEntry, 0, len(resp.Files))
	for _, f := range resp.Files {
		if isQoderAuthName(f.Name) {
			out = append(out, f)
		}
	}
	return out, nil
}

func hostAuthGet(authIndex string) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGet, body)
	if err != nil {
		return nil, err
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostAuthGet)
	if err != nil {
		return nil, err
	}
	var resp rpcHostAuthGetResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("%s: decode result: %w", pluginabi.MethodHostAuthGet, err)
	}
	return resp.JSON, nil
}

func hostAuthSave(name string, raw []byte) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty auth file name")
	}
	saveReq := pluginapi.HostAuthSaveRequest{Name: name, JSON: raw}
	saveBody, _ := json.Marshal(saveReq)
	respRaw, err := hostCall(pluginabi.MethodHostAuthSave, saveBody)
	if err != nil {
		return fmt.Errorf("host.auth.save: %w", err)
	}
	_, err = hostBridgeUnwrap(respRaw, pluginabi.MethodHostAuthSave)
	return err
}

// -----------------------------------------------------------------------------
// Account listing / check-in / quota / import / delete
// -----------------------------------------------------------------------------

type accountView struct {
	ID         string `json:"id"`
	AuthIndex  string `json:"auth_index"`
	FileName   string `json:"file_name"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Region     string `json:"region"`
	AuthMode   string `json:"auth_mode"`
	Plan       string `json:"plan,omitempty"`
	Disabled   bool   `json:"disabled"`
	Status     string `json:"status,omitempty"`
	StreakDays int    `json:"streak_days,omitempty"`
	TotalDays  int    `json:"total_claim_days,omitempty"`
}

// resolveAuth loads the storedAuth for a host auth entry.
func resolveAuth(entry pluginapi.HostAuthFileEntry) (*storedAuth, error) {
	raw, err := hostAuthGet(entry.AuthIndex)
	if err != nil {
		return nil, err
	}
	sa, err := parseStored(raw)
	if err != nil {
		return nil, err
	}
	if sa.Name == "" && entry.Label != "" {
		sa.Name = entry.Label
	}
	if sa.Email == "" && entry.Email != "" {
		sa.Email = entry.Email
	}
	return sa, nil
}

func listAccounts() []accountView {
	files, err := hostAuthList()
	if err != nil {
		return []accountView{}
	}
	out := make([]accountView, 0, len(files))
	for _, f := range files {
		view := accountView{
			ID:        f.ID,
			AuthIndex: f.AuthIndex,
			FileName:  f.Name,
			Name:      f.Label,
			Email:     f.Email,
			Disabled:  f.Disabled,
			Status:    f.Status,
		}
		if sa, err := resolveAuth(f); err == nil {
			if sa.Name != "" {
				view.Name = sa.Name
			}
			if sa.Email != "" {
				view.Email = sa.Email
			}
			view.Region = string(sa.region())
			view.AuthMode = sa.AuthMode
			view.Plan = sa.Plan
			if id := authIDFor(sa); view.ID == "" {
				view.ID = id
			}
			if streak, days, _ := localCheckinStats(firstNonEmpty(view.ID, view.AuthIndex)); days > 0 {
				view.StreakDays = streak
				view.TotalDays = days
			}
		}
		if view.Region == "" {
			view.Region = "global"
		}
		if view.Name == "" {
			view.Name = firstNonEmpty(view.Email, view.ID, view.FileName)
		}
		out = append(out, view)
	}
	return out
}

// findAuth locates a host auth entry by account id or auth index.
func findAuth(idOrIndex string) (pluginapi.HostAuthFileEntry, bool) {
	idOrIndex = strings.TrimSpace(idOrIndex)
	if idOrIndex == "" {
		return pluginapi.HostAuthFileEntry{}, false
	}
	files, err := hostAuthList()
	if err != nil {
		return pluginapi.HostAuthFileEntry{}, false
	}
	for _, f := range files {
		if f.AuthIndex == idOrIndex || f.ID == idOrIndex {
			return f, true
		}
		if sa, err := resolveAuth(f); err == nil {
			if authIDFor(sa) == idOrIndex {
				return f, true
			}
		}
	}
	return pluginapi.HostAuthFileEntry{}, false
}

func checkinAllAccounts() []CheckinResult {
	files, err := hostAuthList()
	if err != nil {
		return []CheckinResult{{Status: checkinStatusError, Message: fmt.Sprintf("读取账号失败: %v", err)}}
	}
	var results []CheckinResult
	for _, f := range files {
		sa, err := resolveAuth(f)
		if err != nil {
			results = append(results, CheckinResult{Account: f.Label, AccountID: f.ID, Status: checkinStatusError, Message: err.Error()})
			continue
		}
		name := firstNonEmpty(sa.Name, f.Label, sa.Email)
		id := firstNonEmpty(f.ID, authIDFor(sa))
		results = append(results, checkinStored(sa, id, name))
	}
	return results
}

func checkinOneAccount(idOrIndex string) CheckinResult {
	f, ok := findAuth(idOrIndex)
	if !ok {
		return CheckinResult{AccountID: idOrIndex, Status: checkinStatusError, Message: "账号不存在: " + idOrIndex}
	}
	sa, err := resolveAuth(f)
	if err != nil {
		return CheckinResult{Account: f.Label, AccountID: f.ID, Status: checkinStatusError, Message: err.Error()}
	}
	name := firstNonEmpty(sa.Name, f.Label, sa.Email)
	id := firstNonEmpty(f.ID, authIDFor(sa))
	return checkinStored(sa, id, name)
}

func handleCheckin(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AccountID string `json:"account_id"`
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	target := firstNonEmpty(body.AccountID, body.AuthIndex)
	if target != "" {
		r := checkinOneAccount(target)
		return map[string]any{"ok": true, "summary": map[string]int{r.Status: 1}, "results": []CheckinResult{r}}
	}
	results := checkinAllAccounts()
	summary := map[string]int{}
	for _, r := range results {
		summary[r.Status]++
	}
	return map[string]any{"ok": true, "summary": summary, "results": results}
}

func handleQuota(req pluginapi.ManagementRequest) map[string]any {
	target := firstNonEmpty(req.Query.Get("account_id"), req.Query.Get("auth_index"))
	f, ok := findAuth(target)
	if !ok {
		return map[string]any{"error": "account not found: " + target}
	}
	sa, err := resolveAuth(f)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	deviceToken := extractDeviceToken(sa.Secret)
	if deviceToken == "" {
		return map[string]any{"error": "account has no device token (PAT accounts must be queried via the API)"}
	}
	quota, err := account.FetchQuota(deviceToken, sa.region())
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"ok": true, "quota": quota}
}

// accountQuotaView is one row of the batch /accounts/quotas response.
type accountQuotaView struct {
	AuthIndex  string                     `json:"auth_index"`
	ID         string                     `json:"id"`
	Plan       string                     `json:"plan,omitempty"`
	Available  float64                    `json:"available_credits"`
	User       *account.QuotaBucket       `json:"user_quota,omitempty"`
	Addon      *account.QuotaBucket       `json:"addon_quota,omitempty"`
	Org        *account.QuotaBucket       `json:"org_resource_package,omitempty"`
	Dedicated  []account.DedicatedPackage `json:"dedicated_packages,omitempty"`
	IsExceeded bool                       `json:"is_quota_exceeded"`
	Error      string                     `json:"error,omitempty"`
}

// handleAllQuotas fetches live credits for every account in parallel (bounded
// concurrency) so the panel can show an "Available Credits" column like the
// official CLI overview. Per-account failures are reported in the row's error
// field instead of failing the whole batch.
func handleAllQuotas() map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	out := make([]accountQuotaView, len(files))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			view := accountQuotaView{AuthIndex: f.AuthIndex, ID: f.ID}
			sa, err := resolveAuth(f)
			if err != nil {
				view.Error = err.Error()
				out[i] = view
				return
			}
			if view.ID == "" {
				view.ID = authIDFor(sa)
			}
			deviceToken := extractDeviceToken(sa.Secret)
			if deviceToken == "" {
				view.Error = "无 device token"
				out[i] = view
				return
			}
			q, err := account.FetchQuota(deviceToken, sa.region())
			if err != nil {
				view.Error = err.Error()
				out[i] = view
				return
			}
			view.Plan = q.Plan
			view.Available = q.AvailableCredits
			view.User = q.UserQuota
			view.Addon = q.AddonQuota
			view.Org = q.OrgResourcePackage
			view.Dedicated = q.DedicatedPackages
			view.IsExceeded = q.IsQuotaExceeded
			out[i] = view
		}(i, f)
	}
	wg.Wait()
	return map[string]any{"ok": true, "quotas": out}
}

func handleImport(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Secret string `json:"secret"`
		PAT    string `json:"pat"`
		Region string `json:"region"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return map[string]any{"error": "invalid body: " + err.Error()}
	}
	secret := firstNonEmpty(body.Secret, body.PAT)
	if secret == "" {
		return map[string]any{"error": "secret is required"}
	}
	sa := &storedAuth{Secret: secret, Region: body.Region, Name: body.Name}
	sa.AuthMode = detectAuthMode(secret)
	if err := resolveAccountInfo(sa); err != nil {
		return map[string]any{"error": "凭证校验失败: " + err.Error()}
	}
	ad := toAuthData(sa)
	// Persist as a normal host auth file (top-level type/provider for routing).
	if err := hostAuthSave(ad.FileName, ad.StorageJSON); err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"ok": true, "id": ad.ID, "name": sa.Name, "email": sa.Email, "region": string(sa.region()), "file_name": ad.FileName}
}

func handleDelete(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AccountID string `json:"account_id"`
		AuthIndex string `json:"auth_index"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return map[string]any{"error": "invalid body: " + err.Error()}
	}
	target := firstNonEmpty(body.AccountID, body.AuthIndex)
	f, ok := findAuth(target)
	if !ok {
		return map[string]any{"error": "account not found: " + target}
	}
	if strings.TrimSpace(f.Path) == "" {
		return map[string]any{"error": "account has no backing file"}
	}
	// Disable the auth first so the host stops scheduling it, then remove the
	// file. The host watcher will drop the record.
	raw, err := hostAuthGet(f.AuthIndex)
	if err == nil {
		var doc map[string]any
		if json.Unmarshal(raw, &doc) == nil {
			doc["disabled"] = true
			if out, err := json.Marshal(doc); err == nil {
				_ = hostAuthSave(f.Name, out)
			}
		}
	}
	if err := deleteAuthFile(f.Path); err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"ok": true}
}

func configView() map[string]any {
	cfg := currentConfig()
	return map[string]any{
		"checkin_auto":     cfg.AutoCheckin,
		"checkin_times":    cfg.CheckinTimes,
		"checkin_on_start": cfg.CheckinOnStart,
		"default_region":   cfg.DefaultRegion,
		"max_concurrency":  cfg.MaxConcurrentPerAuth,
	}
}

func handleConfigUpdate(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		CheckinAuto    *bool    `json:"checkin_auto"`
		CheckinTimes   []string `json:"checkin_times"`
		CheckinOnStart *bool    `json:"checkin_on_start"`
		DefaultRegion  string   `json:"default_region"`
		MaxConcurrent  *int     `json:"max_concurrency"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return map[string]any{"error": "invalid body: " + err.Error()}
	}
	configMu.Lock()
	if body.CheckinAuto != nil {
		activeConfig.AutoCheckin = *body.CheckinAuto
	}
	if len(body.CheckinTimes) > 0 {
		activeConfig.CheckinTimes = body.CheckinTimes
	}
	if body.CheckinOnStart != nil {
		activeConfig.CheckinOnStart = *body.CheckinOnStart
	}
	if r := strings.TrimSpace(body.DefaultRegion); r != "" {
		activeConfig.DefaultRegion = r
	}
	if body.MaxConcurrent != nil && *body.MaxConcurrent > 0 {
		n := *body.MaxConcurrent
		if n > maxConcurrentPerAuthLimit {
			n = maxConcurrentPerAuthLimit
		}
		activeConfig.MaxConcurrentPerAuth = n
	}
	configMu.Unlock()
	savePersistedConfig()
	qoderScheduler.setMax(currentConfig().MaxConcurrentPerAuth)
	return configView()
}

// -----------------------------------------------------------------------------
// Responses
// -----------------------------------------------------------------------------

func mgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}
