package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"qoder2api/logger"
)

// OAuth 认证流程
// 1. StartLogin: 生成 PKCE 参数，返回登录 URL
// 2. WaitLogin: 轮询 deviceToken/poll 端点，等待用户授权
// 3. 获取 device token (dt-xxx)，用于后续 API 调用

const oauthClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"

// oauthSessionTTL 是待授权 OAuth 会话的有效期。默认 10 分钟（用户需在
// 此时间内完成浏览器授权），可用 QODER2API_OAUTH_TIMEOUT 覆盖（如 "30m"）。
func oauthSessionTTL() time.Duration {
	if v := strings.TrimSpace(os.Getenv("QODER2API_OAUTH_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		logger.Error("invalid QODER2API_OAUTH_TIMEOUT=%q, falling back to 10m", v)
	}
	return 10 * time.Minute
}

type pendingOAuth struct {
	loginID  string
	nonce    string
	verifier string
	region   Region
	ctx      context.Context
	cancel   context.CancelFunc
	deadline time.Time
}

var (
	pendingMu sync.Mutex
	// pending holds every in-flight OAuth login, keyed by loginID, so multiple
	// concurrent logins (e.g. several operators or repeated Add-auth dialogs) do
	// not cancel each other.
	pending = map[string]*pendingOAuth{}
)

// pruneExpiredPendingLocked cancels and drops timed-out sessions. Caller must
// hold pendingMu.
func pruneExpiredPendingLocked(now time.Time) {
	for id, p := range pending {
		if p == nil || now.After(p.deadline) {
			if p != nil {
				p.cancel()
			}
			delete(pending, id)
		}
	}
}

// StartLogin 启动 OAuth 登录流程
func StartLogin(region Region) (*OAuthSession, error) {
	verifier, challenge, err := pkce()
	if err != nil {
		return nil, err
	}
	nonce := newSimpleID()
	loginID := newSimpleID()

	ep := GetEndpoints(region)
	params := url.Values{}
	params.Set("nonce", nonce)
	params.Set("challenge", challenge)
	params.Set("challenge_method", "S256")
	params.Set("client_id", oauthClientID)
	loginURL := ep.DeviceLoginBase + "?" + params.Encode()

	logger.Info("OAuth: StartLogin loginID=%s region=%s", loginID, region)

	pendingMu.Lock()
	pruneExpiredPendingLocked(time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	pending[loginID] = &pendingOAuth{
		loginID:  loginID,
		nonce:    nonce,
		verifier: verifier,
		region:   region,
		ctx:      ctx,
		cancel:   cancel,
		deadline: time.Now().Add(oauthSessionTTL()),
	}
	pendingMu.Unlock()

	return &OAuthSession{LoginID: loginID, LoginURL: loginURL}, nil
}

// WaitLogin 等待用户完成 OAuth 授权
func WaitLogin(loginID string) (*Account, error) {
	logger.Debug("OAuth: WaitLogin called loginID=%s", loginID)

	pendingMu.Lock()
	p := pending[loginID]
	pendingMu.Unlock()

	if p == nil {
		logger.Error("OAuth: no pending login for id %s", loginID)
		return nil, fmt.Errorf("no pending login for id %s", loginID)
	}
	defer func() {
		pendingMu.Lock()
		if pending[loginID] == p {
			delete(pending, loginID)
		}
		pendingMu.Unlock()
	}()

	ep := GetEndpoints(p.region)

	logger.Info("OAuth: Starting poll loop")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if time.Now().After(p.deadline) {
			logger.Error("OAuth: Timeout reached")
			return nil, fmt.Errorf("oauth login timed out")
		}
		select {
		case <-p.ctx.Done():
			logger.Info("OAuth: Cancelled")
			return nil, fmt.Errorf("oauth login cancelled")
		case <-ticker.C:
			deviceToken, refreshToken, err := pollToken(p.nonce, p.verifier, ep)
			if err != nil {
				continue
			}
			logger.Info("OAuth: Got token, building account")
			acct, err := buildAccountFromToken(deviceToken, refreshToken, p.region, ep)
			if err != nil {
				logger.Error("OAuth: Build account error: %v", err)
				return nil, err
			}
			logger.Info("OAuth: Success! Account: %s", acct.Name)
			return acct, nil
		}
	}
}

// PollLoginOnce performs a single, non-blocking poll of a pending OAuth
// session. It returns (nil, "", false, nil) while the user has not finished
// authorizing, and (acct, secret, true, nil) on success, where secret is the
// JSON payload {"device_token":...,"refresh_token":...}. This is the shape the
// CPA plugin needs because the host drives the poll cadence itself.
func PollLoginOnce(loginID string) (*Account, string, bool, error) {
	pendingMu.Lock()
	p := pending[loginID]
	pendingMu.Unlock()

	if p == nil {
		return nil, "", false, fmt.Errorf("no pending login for id %s", loginID)
	}
	if time.Now().After(p.deadline) {
		return nil, "", false, fmt.Errorf("oauth login timed out")
	}
	select {
	case <-p.ctx.Done():
		return nil, "", false, fmt.Errorf("oauth login cancelled")
	default:
	}

	ep := GetEndpoints(p.region)
	deviceToken, refreshToken, err := pollToken(p.nonce, p.verifier, ep)
	if err != nil {
		return nil, "", false, nil // still pending
	}
	acct, err := BuildAccountFromToken(deviceToken, refreshToken, p.region)
	if err != nil {
		return nil, "", false, err
	}

	pendingMu.Lock()
	if pending[loginID] == p {
		delete(pending, loginID)
	}
	pendingMu.Unlock()

	secretPayload, _ := json.Marshal(map[string]string{
		"device_token":  deviceToken,
		"refresh_token": refreshToken,
	})
	return acct, string(secretPayload), true, nil
}

func CancelLogin(loginID string) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if p, ok := pending[loginID]; ok {
		p.cancel()
		delete(pending, loginID)
	}
}

// pollToken 轮询 deviceToken/poll 端点
func pollToken(nonce, verifier string, ep Endpoints) (string, string, error) {
	reqURL := fmt.Sprintf("%s?nonce=%s&verifier=%s&challenge_method=S256",
		ep.PollEndpoint,
		url.QueryEscape(nonce),
		url.QueryEscape(verifier))

	logger.Debug("OAuth: Polling %s", ep.PollEndpoint)

	resp, err := http.Get(reqURL)
	if err != nil {
		logger.Error("OAuth: Poll error: %v", err)
		return "", "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	logger.Debug("OAuth: Response [%d]: %s", resp.StatusCode, string(raw))

	// 404 表示还没授权，继续等待
	if resp.StatusCode == 404 {
		return "", "", fmt.Errorf("not authorized yet")
	}

	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("poll: HTTP %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", err
	}

	deviceToken, _ := result["token"].(string)
	refreshToken, _ := result["refresh_token"].(string)

	if deviceToken == "" {
		return "", "", fmt.Errorf("no device token in response")
	}
	if refreshToken != "" {
		logger.Info("OAuth: Got device token and refresh token")
	} else {
		logger.Info("OAuth: Got device token")
	}
	return deviceToken, refreshToken, nil
}

// buildAccountFromToken 使用 device token 构建账号信息并落盘 secret（独立服务用）。
func buildAccountFromToken(deviceToken, refreshToken string, region Region, ep Endpoints) (*Account, error) {
	acct, err := BuildAccountFromToken(deviceToken, refreshToken, region)
	if err != nil {
		return nil, err
	}
	secretPayload, err := json.Marshal(map[string]string{
		"device_token":  deviceToken,
		"refresh_token": refreshToken,
	})
	if err != nil {
		return nil, err
	}
	if err := SaveSecret(acct.ID, string(secretPayload)); err != nil {
		return nil, err
	}
	return acct, nil
}

// BuildAccountFromToken 使用 device token 构建账号信息，但不持久化 secret。
// CPA 插件把凭证存进 host auth store，不需要独立服务的文件存储。
func BuildAccountFromToken(deviceToken, refreshToken string, region Region) (*Account, error) {
	ep := GetEndpoints(region)
	info, err := fetchUserInfo(deviceToken, ep)
	if err != nil {
		return nil, fmt.Errorf("fetch user info: %w", err)
	}
	plan := fetchPlan(deviceToken, ep)

	id := SanitizeID(strGet(info, "userId") + strGet(info, "email"))
	if id == "" {
		id = newSimpleID()
	}
	now := time.Now()
	return &Account{
		ID:        id,
		Name:      strGet(info, "name"),
		Email:     strings.ToLower(strGet(info, "email")),
		UserType:  strGet(info, "userType"),
		Plan:      plan,
		Region:    region,
		AuthMode:  "oauth",
		APIMode:   "openai",
		Tags:      []string{},
		CreatedAt: now,
	}, nil
}

func fetchUserInfo(token string, ep Endpoints) (map[string]interface{}, error) {
	req, _ := http.NewRequest("GET", ep.UserinfoBase, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &result)
	return result, nil
}

func fetchPlan(token string, ep Endpoints) string {
	req, _ := http.NewRequest("GET", ep.PlanEndpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := quotaClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &result)
	return strGet(result, "plan_tier_name")
}

func pkce() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return
}

func newSimpleID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func strGet(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// quotaClient bounds quota/plan HTTP calls so a hung upstream cannot stall the
// management panel's batch credits refresh.
var quotaClient = &http.Client{Timeout: 15 * time.Second}

func FetchQuota(token string, region Region) (*QuotaInfo, error) {
	ep := GetEndpoints(region)
	req, _ := http.NewRequest("GET", ep.QuotaEndpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := quotaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}

	return ParseQuotaPayload(result, fetchPlan(token, ep)), nil
}

// ParseQuotaPayload turns a /api/v2/quota/usage (or unwrapped
// /sash/api/v2/me/usage) response into a QuotaInfo. It is exported so the
// available-credits aggregation can be unit tested without network access.
func ParseQuotaPayload(result map[string]interface{}, plan string) *QuotaInfo {
	// /sash/api/v2/me/usage wraps the same quota payload under "qoderUsage".
	// Unwrap it so both endpoints share one parser.
	if inner, ok := result["qoderUsage"].(map[string]interface{}); ok {
		result = inner
	}

	info := &QuotaInfo{
		Plan:            plan,
		IsQuotaExceeded: result["isQuotaExceeded"] == true,
		ExpiresAt:       int64(toFloat(result, "expiresAt")),
	}

	info.UserQuota = extractBucketAny(result, "userQuota", "user_quota")
	info.AddonQuota = extractBucketAny(result, "addOnQuota", "add_on_quota", "addonQuota")
	info.OrgResourcePackage = extractBucketAny(result, "orgResourcePackage", "org_resource_package", "sharedQuota", "shared_quota")

	// Mirror the official CLI: available = sum of every bucket's remaining.
	for _, b := range []*QuotaBucket{info.UserQuota, info.AddonQuota, info.OrgResourcePackage} {
		if b != nil {
			info.AvailableCredits += b.Remaining
		}
	}

	return info
}

// extractBucketAny is extractBucket with multiple accepted key spellings
// (the API mixes camelCase and snake_case across regions/endpoints).
func extractBucketAny(data map[string]interface{}, keys ...string) *QuotaBucket {
	for _, key := range keys {
		if b := extractBucket(data, key); b != nil {
			return b
		}
	}
	return nil
}

func extractBucket(data map[string]interface{}, key string) *QuotaBucket {
	obj, ok := data[key].(map[string]interface{})
	if !ok {
		return nil
	}
	used := toFloat(obj, "used")
	total := toFloat(obj, "total")
	remaining := toFloat(obj, "remaining")
	if total == 0 && used == 0 && remaining == 0 {
		return nil
	}
	resetTime := ""
	if v, ok := obj["resetTime"].(string); ok {
		resetTime = v
	} else if v, ok := obj["reset_time"].(string); ok {
		resetTime = v
	}
	unit, _ := obj["unit"].(string)
	return &QuotaBucket{Used: used, Total: total, Remaining: remaining, ResetTime: resetTime, Unit: unit}
}

func toFloat(m map[string]interface{}, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}
