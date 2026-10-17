package account

import "time"

type Account struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Email     string     `json:"email,omitempty"`
	UserType  string     `json:"user_type,omitempty"`
	Plan      string     `json:"plan,omitempty"`
	Region    Region     `json:"region,omitempty"` // "global" | "cn"，空值视为 global
	AuthMode  string     `json:"auth_mode"`        // "pat" | "oauth"
	APIMode   string     `json:"api_mode"`         // "openai" | "anthropic" | "gemini" | "claude-code"
	Tags      []string   `json:"tags"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	Active    bool       `json:"active"`
	SortOrder int        `json:"sort_order"`

	// CheckinFingerprint 签到专用设备指纹（可选）。
	// 国际版（qoder.com）的签到活动**按真实设备身份下发**：请求必须携带与登录
	// 时协商一致的 cosy 指纹，否则服务端不下发 CLAIM_BENEFIT 活动（表现为
	// no_campaign）。自造指纹无效，需从官方客户端抓包取得。
	// 国内版（cn）反之——发送自造 machinetoken 会被拒，留空即可。
	CheckinFingerprint *CheckinFingerprint `json:"checkin_fingerprint,omitempty"`
}

// CheckinFingerprint 官方客户端签到请求携带的 cosy 设备指纹。
type CheckinFingerprint struct {
	MachineID    string `json:"machine_id"`
	MachineToken string `json:"machine_token"`
	MachineType  string `json:"machine_type"`
	MachineCode  string `json:"machine_code,omitempty"`
	Hostname     string `json:"hostname,omitempty"`
	MachineOS    string `json:"machine_os,omitempty"`
	Version      string `json:"version,omitempty"`
}

type OAuthSession struct {
	LoginID  string `json:"login_id"`
	LoginURL string `json:"login_url"`
}

type Status struct {
	Running       bool   `json:"running"`
	Port          int    `json:"port"`
	ActiveAccount string `json:"active_account"`
	APIMode       string `json:"api_mode"`
}

type Settings struct {
	Port                 int                          `json:"port"`
	AutoStart            bool                         `json:"auto_start"`
	LogLevel             string                       `json:"log_level"`                    // "info" | "debug" | "error"
	QuotaRefreshInterval int                          `json:"quota_refresh_interval"`       // 秒，0=不自动刷新
	BridgeToken          string                       `json:"bridge_token,omitempty"`       // 自定义鉴权 token，空则使用默认值 "qccg"
	ConsolePassword      string                       `json:"console_password,omitempty"`   // Web 控制台登录密码（类似 CPA management key）
	ModelMapping         map[string]string            `json:"model_mapping,omitempty"`      // [DEPRECATED] 旧扁平映射（向后兼容，MapModel 中作为兜底回退使用）
	ModelMappings        map[string]map[string]string `json:"model_mappings,omitempty"`     // agent (claude/codex/gemini) → 客户端模型名 → Qoder model.key
	AutoCheckin          bool                         `json:"auto_checkin"`                 // 自动签到开关（默认关闭）
	AutoCheckinTimes     []string                     `json:"auto_checkin_times,omitempty"` // 自动签到时间点，HH:MM 列表，如 ["09:15","17:00"]；空=10:00
	MachineSalt          string                       `json:"machine_salt,omitempty"`       // 本机设备指纹盐（首次启动自动生成；勿修改，变更即指纹漂移）
}

type QuotaInfo struct {
	Plan               string       `json:"plan"`
	UserQuota          *QuotaBucket `json:"user_quota,omitempty"`
	AddonQuota         *QuotaBucket `json:"addon_quota,omitempty"`
	OrgResourcePackage *QuotaBucket `json:"org_resource_package,omitempty"`
	// AvailableCredits is the sum of remaining credits across every bucket
	// (user + add-on + org). This is what the official CLI shows as
	// "Available Credits" and is more meaningful than any single bucket:
	// a Pro Trial account can have userQuota.remaining=0 while addOnQuota still
	// holds 83 credits.
	AvailableCredits float64 `json:"available_credits"`
	IsQuotaExceeded  bool    `json:"is_quota_exceeded"`
	ExpiresAt        int64   `json:"expires_at,omitempty"`
}

type QuotaBucket struct {
	Used      float64 `json:"used"`
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining"`
	ResetTime string  `json:"reset_time,omitempty"`
	Unit      string  `json:"unit,omitempty"`
}
