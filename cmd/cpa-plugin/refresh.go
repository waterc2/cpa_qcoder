package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"qoder2api/account"
)

// deviceTokenRefresh 是 /api/v1/deviceToken/refresh 的结果。
type deviceTokenRefresh struct {
	DeviceToken           string
	RefreshToken          string
	ExpiresAt             string
	RefreshTokenExpiresAt string
}

// refreshDeviceToken 使用 refresh_token 换取新的设备令牌，并轮换 refresh_token。
// 官方 CLI 的实现（identity.device.refresh）只发送 refresh_token 及可选的机器
// 身份字段，不需要携带旧的（可能已过期的）设备令牌作为 Bearer。
func refreshDeviceToken(region account.Region, refreshToken string, fp *account.CheckinFingerprint) (*deviceTokenRefresh, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("device token refresh: empty refresh_token")
	}
	ep := account.GetEndpoints(region)
	url := strings.TrimSuffix(ep.PollEndpoint, "/poll") + "/refresh"

	body := map[string]any{"refresh_token": refreshToken}
	if fp != nil {
		if fp.MachineID != "" {
			body["machine_id"] = fp.MachineID
		}
		if fp.MachineType != "" {
			body["machine_type"] = fp.MachineType
		}
		if fp.MachineCode != "" {
			body["machine_code"] = fp.MachineCode
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device token refresh: status %d: %s", resp.StatusCode, strings.TrimSpace(string(rawResp)))
	}

	var out struct {
		DeviceToken           string `json:"device_token"`
		Token                 string `json:"token"`
		AccessToken           string `json:"access_token"`
		RefreshToken          string `json:"refresh_token"`
		ExpiresAt             string `json:"expires_at"`
		RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
	}
	if err := json.Unmarshal(rawResp, &out); err != nil {
		return nil, fmt.Errorf("device token refresh: decode: %w", err)
	}

	token := out.DeviceToken
	if token == "" {
		token = out.Token
	}
	if token == "" {
		token = out.AccessToken
	}
	if token == "" {
		return nil, fmt.Errorf("device token refresh: response missing token")
	}
	return &deviceTokenRefresh{
		DeviceToken:           token,
		RefreshToken:          out.RefreshToken,
		ExpiresAt:             out.ExpiresAt,
		RefreshTokenExpiresAt: out.RefreshTokenExpiresAt,
	}, nil
}

// parseSecretTokens 从存储的 secret 中解析设备令牌和刷新令牌。secret 可能是
// JSON 载荷、裸 dt-/drt- 令牌，或 PAT（此时两者均为空）。
func parseSecretTokens(secret string) (deviceToken, refreshToken string) {
	s := strings.TrimSpace(secret)
	if s == "" {
		return "", ""
	}
	if s[0] == '{' {
		var p struct {
			DeviceToken  string `json:"device_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if json.Unmarshal([]byte(s), &p) == nil {
			return p.DeviceToken, p.RefreshToken
		}
		return "", ""
	}
	if strings.HasPrefix(s, "drt-") {
		return "", s
	}
	if strings.HasPrefix(s, "dt-") {
		return s, ""
	}
	return "", ""
}
