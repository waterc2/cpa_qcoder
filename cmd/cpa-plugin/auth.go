package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"qoder2api/account"
)

// checkinFingerprint mirrors account.CheckinFingerprint: the official client
// cosy device fingerprint required by the Global daily check-in campaign.
type checkinFingerprint struct {
	MachineID    string `json:"machine_id"`
	MachineToken string `json:"machine_token"`
	MachineType  string `json:"machine_type"`
	MachineCode  string `json:"machine_code,omitempty"`
	Hostname     string `json:"hostname,omitempty"`
	MachineOS    string `json:"machine_os,omitempty"`
	Version      string `json:"version,omitempty"`
}

// storedAuth is the provider-owned credential payload persisted in the CPA auth
// record's StorageJSON. One record == one Qoder account.
type storedAuth struct {
	Secret      string              `json:"secret"`
	Region      string              `json:"region,omitempty"`
	AccountID   string              `json:"account_id,omitempty"`
	Name        string              `json:"name,omitempty"`
	Email       string              `json:"email,omitempty"`
	UserType    string              `json:"user_type,omitempty"`
	Plan        string              `json:"plan,omitempty"`
	AuthMode    string              `json:"auth_mode,omitempty"`
	Disabled    bool                `json:"disabled,omitempty"`
	Fingerprint *checkinFingerprint `json:"checkin_fingerprint,omitempty"`

	rawJSON []byte
}

func (sa *storedAuth) region() account.Region {
	if sa != nil && account.NormalizeRegion(sa.Region) == account.RegionCN {
		return account.RegionCN
	}
	return account.RegionGlobal
}

// authIDFor returns a stable, path-safe auth id for the account.
func authIDFor(sa *storedAuth) string {
	if sa == nil {
		return "qoder-unknown"
	}
	if id := sanitizeID(sa.AccountID); id != "" {
		return "qoder-" + id
	}
	sum := sha1.Sum([]byte(sa.Secret))
	return "qoder-" + hex.EncodeToString(sum[:])[:12]
}

func sanitizeID(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

// parseStored decodes a Qoder credential document. It accepts:
//   - the plugin's own storedAuth JSON,
//   - a bare PAT string,
//   - a JSON doc with secret/pat/personal_token, or device_token/refresh_token.
func parseStored(raw []byte) (*storedAuth, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("empty credential")
	}
	// Bare non-JSON token (PAT).
	if !strings.HasPrefix(trimmed, "{") {
		return &storedAuth{Secret: trimmed, AuthMode: "pat"}, nil
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", err)
	}
	// Provider fields may be nested under "auth" (CPA auth-file convention).
	// Merge the nested object up so both flat and nested files parse.
	if _, hasSecret := doc["secret"]; !hasSecret {
		if nestedRaw, ok := doc["auth"]; ok {
			var nested map[string]json.RawMessage
			if json.Unmarshal(nestedRaw, &nested) == nil {
				for k, v := range nested {
					if _, exists := doc[k]; !exists {
						doc[k] = v
					}
				}
			}
		}
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			v, ok := doc[k]
			if !ok {
				continue
			}
			var s string
			if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}

	secret := get("secret", "pat", "personal_token", "personalToken")
	deviceToken := get("device_token", "deviceToken")
	refreshToken := get("refresh_token", "refreshToken")
	if secret == "" && deviceToken != "" {
		payload, _ := json.Marshal(map[string]string{
			"device_token":  deviceToken,
			"refresh_token": refreshToken,
		})
		secret = string(payload)
	}
	if secret == "" {
		return nil, fmt.Errorf("credential has no secret/pat/device_token")
	}

	sa := &storedAuth{
		Secret:    secret,
		Region:    get("region"),
		AccountID: get("account_id", "accountId", "uid", "id"),
		Name:      get("name", "nickname"),
		Email:     strings.ToLower(get("email")),
		UserType:  get("user_type", "userType"),
		Plan:      get("plan"),
		AuthMode:  get("auth_mode", "authMode"),
		rawJSON:   append([]byte(nil), raw...),
	}
	if v, ok := doc["disabled"]; ok {
		_ = json.Unmarshal(v, &sa.Disabled)
	}
	if fpRaw, ok := doc["checkin_fingerprint"]; ok {
		var fp checkinFingerprint
		if json.Unmarshal(fpRaw, &fp) == nil && fp.MachineID != "" {
			sa.Fingerprint = &fp
		}
	}
	if sa.AuthMode == "" {
		if deviceToken != "" || strings.HasPrefix(secret, "dt-") || strings.HasPrefix(secret, "drt-") {
			sa.AuthMode = "oauth"
		} else {
			sa.AuthMode = "pat"
		}
	}
	return sa, nil
}

// authFileDoc builds the on-disk auth document: provider fields at the top
// level plus the routing metadata the host needs (type/provider/logo).
func authFileDoc(sa *storedAuth) []byte {
	doc := map[string]any{}
	if raw, err := json.Marshal(sa); err == nil {
		_ = json.Unmarshal(raw, &doc)
	}
	doc["type"] = providerName
	doc["provider"] = providerName
	doc["logo"] = pluginLogoURL
	raw, _ := json.Marshal(doc)
	return raw
}

// toAuthData builds the host-facing auth record.
func toAuthData(sa *storedAuth) pluginapi.AuthData {
	id := authIDFor(sa)
	fileName := "qoder-" + strings.TrimPrefix(id, "qoder-") + ".json"
	label := firstNonEmpty(sa.Name, sa.Email, sa.AccountID, id)
	meta := map[string]any{
		"type":     providerName,
		"provider": providerName,
		"logo":     pluginLogoURL,
		"region":   string(sa.region()),
	}
	if sa.Name != "" {
		meta["name"] = sa.Name
	}
	if sa.Email != "" {
		meta["email"] = sa.Email
	}
	return pluginapi.AuthData{
		Provider:    providerName,
		ID:          id,
		FileName:    fileName,
		Label:       label,
		StorageJSON: authFileDoc(sa),
		Metadata:    meta,
	}
}

// resolveAccountInfo fills name/email/account id by building (or reusing) a
// Qoder session. It returns an error when the credential cannot be resolved,
// which callers use to reject obviously invalid imports.
func resolveAccountInfo(sa *storedAuth) error {
	if sa == nil {
		return fmt.Errorf("nil credential")
	}
	if sa.AccountID != "" {
		return nil // already resolved and persisted
	}
	b, err := bridgeForAuth(sa)
	if err != nil {
		return err
	}
	id := b.Identity()
	if sa.Name == "" {
		sa.Name = id.Name
	}
	if sa.AccountID == "" {
		sa.AccountID = firstNonEmpty(id.Uid, id.Aid)
	}
	if sa.UserType == "" {
		sa.UserType = id.UserType
	}
	if sa.AccountID == "" {
		return fmt.Errorf("upstream did not return an account id (credential may be invalid)")
	}
	return nil
}

// -----------------------------------------------------------------------------
// auth.parse
// -----------------------------------------------------------------------------

func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Ownership check: only claim files whose declared type is ours, or
	// type-less files the host routed to us / whose filename carries our prefix.
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(req.RawJSON, &probe)
	declared := strings.ToLower(strings.TrimSpace(probe.Type))
	if declared != "" && declared != providerName && declared != "qoder2api" {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if declared == "" {
		routed := strings.EqualFold(strings.TrimSpace(req.Provider), providerName)
		if !routed && !isQoderAuthName(req.FileName) {
			return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
		}
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	_ = resolveAccountInfo(sa) // best-effort label enrichment
	ad := toAuthData(sa)
	// Leave ID empty so the host derives it from the file path and does not
	// create duplicate records for the same file.
	ad.ID = ""
	if fn := strings.TrimSpace(req.FileName); fn != "" {
		ad.FileName = fn
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: ad})
}

func isQoderAuthName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\\`) {
		return false
	}
	lower := strings.ToLower(name)
	if lower == authFileName {
		return true
	}
	return strings.HasPrefix(lower, providerName+"-") &&
		strings.HasSuffix(lower, ".json") &&
		len(lower) > len(providerName)+len("-.json")
}

// -----------------------------------------------------------------------------
// auth.login.start / poll
// -----------------------------------------------------------------------------

var (
	loginMu    sync.Mutex
	loginState = map[string]string{} // state -> region
)

func handleStartLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	region := strings.TrimSpace(activeRegion(req.Metadata))
	if region == "" {
		region = currentConfig().DefaultRegion
	}
	r := account.NormalizeRegion(region)

	session, err := account.StartLogin(r)
	if err != nil {
		return nil, fmt.Errorf("start login: %w", err)
	}
	loginMu.Lock()
	loginState[session.LoginID] = string(r)
	loginMu.Unlock()

	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       session.LoginURL,
		State:     session.LoginID,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
		Metadata:  map[string]any{"logo": pluginLogoURL, "region": string(r)},
	})
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	acct, secret, done, err := account.PollLoginOnce(state)
	if err != nil {
		loginMu.Lock()
		delete(loginState, state)
		loginMu.Unlock()
		return nil, err
	}
	if !done {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for authorization",
		})
	}

	region := string(acct.Region)
	loginMu.Lock()
	if r, ok := loginState[state]; ok {
		region = r
	}
	delete(loginState, state)
	loginMu.Unlock()

	sa := &storedAuth{
		Secret:    secret,
		Region:    region,
		AccountID: acct.ID,
		Name:      acct.Name,
		Email:     acct.Email,
		UserType:  acct.UserType,
		Plan:      acct.Plan,
		AuthMode:  "oauth",
	}
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa),
	})
}

func activeRegion(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	for _, k := range []string{"region", "Region"} {
		if v, ok := meta[k].(string); ok {
			return v
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// auth.refresh
// -----------------------------------------------------------------------------

// handleRefreshAuth re-validates and returns the credential. Qoder device
// tokens are long-lived and there is no dedicated refresh endpoint in the
// standalone gateway, so the record is returned unchanged with a deferred
// next-refresh so the host does not poll aggressively.
func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	// Keep host metadata (disabled, note, ...) intact.
	ad := toAuthData(sa)
	ad.ID = ""
	ad.FileName = ""
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             ad,
		NextRefreshAfter: time.Now().Add(24 * time.Hour),
	})
}
