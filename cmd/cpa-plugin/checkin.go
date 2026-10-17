package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"qoder2api/account"
	"qoder2api/logger"
)

const checkinHost = "openapi.qoder.com.cn"

func checkinHostFor(region account.Region) string {
	if h := account.GetEndpoints(region).OpenAPIHost; h != "" {
		return h
	}
	return checkinHost
}

// CheckinResult is the per-account check-in outcome.
type CheckinResult struct {
	Account   string `json:"account"`
	AccountID string `json:"account_id"`
	Status    string `json:"status"`
	Amount    int    `json:"amount,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Message   string `json:"message"`

	StreakDays         int `json:"streak_days,omitempty"`
	TotalClaimDays     int `json:"total_claim_days,omitempty"`
	TotalRewardCredits int `json:"total_reward_credits,omitempty"`
	RewardCredits      int `json:"reward_credits,omitempty"`

	WindowDate string `json:"-"`
}

const (
	checkinStatusClaimed        = "claimed"
	checkinStatusAlreadyClaimed = "already_claimed"
	checkinStatusNoCampaign     = "no_campaign"
	checkinStatusNoToken        = "no_token"
	checkinStatusError          = "error"
)

var checkinClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	},
}

func checkinHeaders(deviceToken string, fp *checkinFingerprint) map[string]string {
	h := map[string]string{
		"authorization":   "Bearer " + deviceToken,
		"accept":          "application/json",
		"accept-language": "zh-CN",
		"user-agent":      "Qoder",
		"cosy-clienttype": "10",
	}
	if fp != nil && fp.MachineToken != "" {
		h["cosy-version"] = orDefault(fp.Version, "0.4.4")
		h["cosy-machineid"] = fp.MachineID
		h["cosy-machinetoken"] = fp.MachineToken
		h["cosy-machinetype"] = fp.MachineType
		if fp.MachineCode != "" {
			h["cosy-machinecode"] = fp.MachineCode
		}
		h["cosy-machinehostname"] = orDefault(fp.Hostname, "localhost")
		h["cosy-machineos"] = orDefault(fp.MachineOS, "aarch64_darwin")
	}
	return h
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func doCheckinRequest(method, path, deviceToken string, reqBody interface{}, host string, fp *checkinFingerprint) (int, interface{}, string) {
	if host == "" {
		host = checkinHost
	}
	url := "https://" + host + path

	var body io.Reader
	if reqBody != nil {
		b, _ := json.Marshal(reqBody)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return 0, nil, fmt.Sprintf("build request: %v", err)
	}
	for k, v := range checkinHeaders(deviceToken, fp) {
		req.Header.Set(k, v)
	}
	if method == "POST" {
		req.Header.Set("origin", "https://"+host)
		if reqBody == nil {
			req.ContentLength = 0
		}
	}

	resp, err := checkinClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Sprintf("do request: %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	raw := string(data)
	var parsed interface{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &parsed)
	}
	return resp.StatusCode, parsed, raw
}

type campaignInfo struct {
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ActionType  string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	StartAt     int64  `json:"startAt"`
	EndAt       int64  `json:"endAt"`
	Benefit     *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
}

type claimResponse struct {
	GrantID  string `json:"grantId"`
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
	Benefit  *struct {
		Kind     string `json:"kind"`
		Amount   int    `json:"amount"`
		Validity *struct {
			Mode string `json:"mode"`
			Days int    `json:"days"`
		} `json:"validity"`
	} `json:"benefit"`
	ExpiresAt string `json:"expiresAt"`
}

type dailyCheckinStatus struct {
	CampaignKey        string `json:"campaignKey"`
	Status             string `json:"status"`
	RewardCredits      int    `json:"rewardCredits"`
	CurrentStreakDays  int    `json:"currentStreakDays"`
	TotalClaimDays     int    `json:"totalClaimDays"`
	TotalRewardCredits int    `json:"totalRewardCredits"`
}

// ---------- local check-in history (streak calculation) ----------

type checkinRecord struct {
	Date   string `json:"date"`
	Amount int    `json:"amount"`
}

type checkinHistory struct {
	Accounts map[string][]checkinRecord `json:"accounts"`
}

var historyMu sync.Mutex

func checkinHistoryPath() string {
	return filepath.Join(account.DataRoot(), "checkin_history.json")
}

func loadCheckinHistory() *checkinHistory {
	h := &checkinHistory{Accounts: map[string][]checkinRecord{}}
	data, err := os.ReadFile(checkinHistoryPath())
	if err != nil {
		return h
	}
	_ = json.Unmarshal(data, h)
	if h.Accounts == nil {
		h.Accounts = map[string][]checkinRecord{}
	}
	return h
}

func saveCheckinHistory(h *checkinHistory) {
	data, _ := json.MarshalIndent(h, "", "  ")
	_ = os.WriteFile(checkinHistoryPath(), data, 0o600)
}

func calcLocalStats(recs []checkinRecord) (streak, totalDays, totalCredits int) {
	if len(recs) == 0 {
		return 0, 0, 0
	}
	days := map[string]bool{}
	for _, r := range recs {
		days[r.Date] = true
		totalCredits += r.Amount
	}
	totalDays = len(recs)
	cursor := time.Now()
	if !days[cursor.Format("2006-01-02")] {
		cursor = cursor.AddDate(0, 0, -1)
	}
	for days[cursor.Format("2006-01-02")] {
		streak++
		cursor = cursor.AddDate(0, 0, -1)
	}
	return streak, totalDays, totalCredits
}

func recordCheckinOn(accountID, date string, amount int) (streak, totalDays, totalCredits int) {
	historyMu.Lock()
	defer historyMu.Unlock()

	h := loadCheckinHistory()
	recs := h.Accounts[accountID]
	found := false
	for _, r := range recs {
		if r.Date == date {
			found = true
			break
		}
	}
	if !found {
		recs = append(recs, checkinRecord{Date: date, Amount: amount})
		h.Accounts[accountID] = recs
		saveCheckinHistory(h)
	} else if amount > 0 {
		for i := range recs {
			if recs[i].Date == date && recs[i].Amount == 0 {
				recs[i].Amount = amount
				h.Accounts[accountID] = recs
				saveCheckinHistory(h)
				break
			}
		}
	}
	return calcLocalStats(recs)
}

func localCheckinStats(accountID string) (streak, totalDays, totalCredits int) {
	historyMu.Lock()
	defer historyMu.Unlock()
	return calcLocalStats(loadCheckinHistory().Accounts[accountID])
}

func applyLocalStreak(accountID string, res *CheckinResult, record bool, date string, amount int) {
	var streak, totalDays, totalCredits int
	if record {
		if date == "" {
			date = time.Now().Format("2006-01-02")
		}
		streak, totalDays, totalCredits = recordCheckinOn(accountID, date, amount)
	} else {
		streak, totalDays, totalCredits = localCheckinStats(accountID)
	}
	if totalDays > 0 {
		res.StreakDays = streak
		res.TotalClaimDays = totalDays
		res.TotalRewardCredits = totalCredits
	}
}

// ---------- check-in core ----------

// checkinStored performs a check-in for one Qoder credential.
func checkinStored(sa *storedAuth, id, name string) CheckinResult {
	res := CheckinResult{Account: name, AccountID: id, Status: checkinStatusError}
	if sa == nil || sa.Secret == "" {
		res.Status = checkinStatusNoToken
		res.Message = "无凭证"
		return res
	}
	deviceToken := extractDeviceToken(sa.Secret)
	if deviceToken == "" {
		res.Status = checkinStatusNoToken
		res.Message = "无 device token"
		return res
	}

	host := checkinHostFor(sa.region())
	r := campaignsCheckin(deviceToken, &res, host, sa.Fingerprint)
	readDailyCheckinStats(deviceToken, &r, host, sa.Fingerprint)
	return finalizeCheckin(id, r)
}

func extractDeviceToken(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	if secret[0] == '{' {
		var s struct {
			DeviceToken string `json:"device_token"`
		}
		if json.Unmarshal([]byte(secret), &s) == nil {
			return s.DeviceToken
		}
		return ""
	}
	// Only a bare device token can authenticate the check-in/quota endpoints.
	// A bare PAT (pt-...) is NOT a device token: returning it here made the
	// callers send it as a Bearer token and fail with a confusing 401.
	if strings.HasPrefix(secret, "dt-") {
		return secret
	}
	return ""
}

func finalizeCheckin(accountID string, r CheckinResult) CheckinResult {
	record := r.Status == checkinStatusClaimed || r.Status == checkinStatusAlreadyClaimed
	amount := r.Amount
	if amount == 0 {
		amount = r.RewardCredits
	}
	if amount == 0 {
		amount = 100
	}
	applyLocalStreak(accountID, &r, record, r.WindowDate, amount)
	if record || r.StreakDays > 0 || r.TotalClaimDays > 0 {
		r.Message = r.Message + streakSuffix(&r)
	}
	return r
}

func readDailyCheckinStats(deviceToken string, res *CheckinResult, host string, fp *checkinFingerprint) {
	status, body, raw := doCheckinRequest("GET", "/sash/api/v1/me/daily-check-in/status", deviceToken, nil, host, fp)
	if status != 200 {
		if status != 404 && status != 401 {
			logger.Info("[Checkin] daily-check-in/status HTTP %d: %s", status, truncateStr(raw, 150))
		}
		return
	}
	var st dailyCheckinStatus
	b, _ := json.Marshal(body)
	if json.Unmarshal(b, &st) != nil || st.Status == "" {
		return
	}
	if st.CurrentStreakDays > 0 || st.TotalClaimDays > 0 {
		if st.CurrentStreakDays > res.StreakDays {
			res.StreakDays = st.CurrentStreakDays
		}
		if st.TotalClaimDays > res.TotalClaimDays {
			res.TotalClaimDays = st.TotalClaimDays
		}
		if st.TotalRewardCredits > res.TotalRewardCredits {
			res.TotalRewardCredits = st.TotalRewardCredits
		}
	}
	if res.RewardCredits == 0 && st.RewardCredits > 0 {
		res.RewardCredits = st.RewardCredits
	}
}

func streakSuffix(res *CheckinResult) string {
	if res.StreakDays <= 0 && res.TotalClaimDays <= 0 {
		return ""
	}
	return fmt.Sprintf("（连续 %d 天 · 累计 %d 天 · 共 %d 积分）", res.StreakDays, res.TotalClaimDays, res.TotalRewardCredits)
}

func windowHint(res *CheckinResult, fallback string) string {
	if res.WindowDate == "" {
		return fallback
	}
	today := time.Now().Format("2006-01-02")
	if res.WindowDate == today {
		return fallback
	}
	if wd, err := time.Parse("2006-01-02", res.WindowDate); err == nil {
		next := wd.AddDate(0, 0, 1).Format("01-02")
		return fmt.Sprintf("已领取 %s 窗口额度（新窗口 %s 10:00 开放）", res.WindowDate, next)
	}
	return fallback
}

func campaignsCheckin(deviceToken string, res *CheckinResult, host string, fp *checkinFingerprint) CheckinResult {
	status, body, raw := doCheckinRequest("GET", "/sash/api/v1/me/campaigns", deviceToken, nil, host, fp)
	if status != 200 {
		res.Message = fmt.Sprintf("查询活动失败 HTTP %d: %s", status, truncateStr(raw, 300))
		return *res
	}
	list, ok := body.(map[string]interface{})
	if !ok {
		res.Message = fmt.Sprintf("活动列表格式异常: %s", truncateStr(raw, 300))
		return *res
	}

	campaignsRaw, _ := list["campaigns"].([]interface{})
	var campaigns []campaignInfo
	for _, c := range campaignsRaw {
		b, _ := json.Marshal(c)
		var ci campaignInfo
		if json.Unmarshal(b, &ci) == nil {
			campaigns = append(campaigns, ci)
		}
	}

	var target, benefitCampaign *campaignInfo
	alreadyClaimed := false
	for i := range campaigns {
		c := &campaigns[i]
		if c.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		benefitCampaign = c
		if c.ClaimStatus == "CLAIMABLE" {
			target = c
		} else if c.ClaimStatus == "CLAIMED" {
			alreadyClaimed = true
		}
	}

	windowSrc := target
	if windowSrc == nil {
		windowSrc = benefitCampaign
	}
	if windowSrc != nil && windowSrc.StartAt > 0 {
		res.WindowDate = time.Unix(windowSrc.StartAt, 0).Format("2006-01-02")
	}

	if target == nil {
		if alreadyClaimed {
			res.Status = checkinStatusAlreadyClaimed
			res.Message = windowHint(res, "今日已领取")
		} else {
			res.Status = checkinStatusNoCampaign
			res.Message = "无可用签到活动"
			if fp == nil {
				res.Message += "（若为国际版账号，需在 auth 文件中提供从官方客户端抓包得到的 checkin_fingerprint）"
			}
			actionTypes := make([]string, 0, len(campaigns))
			for i := range campaigns {
				actionTypes = append(actionTypes, campaigns[i].ActionType+"/"+campaigns[i].ClaimStatus)
			}
			logger.Info("[Checkin] no CLAIM_BENEFIT: fingerprint=%v campaigns=%v raw=%s", fp != nil, actionTypes, truncateStr(raw, 400))
		}
		return *res
	}

	claimPath := fmt.Sprintf("/sash/api/v1/me/campaigns/%s/claim", target.CampaignID)
	status, body, raw = doCheckinRequest("POST", claimPath, deviceToken, nil, host, fp)
	if status != 200 {
		res.Message = fmt.Sprintf("领取失败 HTTP %d: %s", status, truncateStr(raw, 300))
		return *res
	}
	var cr claimResponse
	b, _ := json.Marshal(body)
	if json.Unmarshal(b, &cr) != nil {
		res.Message = fmt.Sprintf("领取响应格式异常: %s", truncateStr(raw, 300))
		return *res
	}
	if cr.Status == "CLAIMED" {
		if cr.Replayed {
			res.Status = checkinStatusAlreadyClaimed
			res.Message = windowHint(res, "今日已领取")
		} else {
			res.Status = checkinStatusClaimed
			if cr.Benefit != nil {
				res.Amount = cr.Benefit.Amount
			}
			res.ExpiresAt = cr.ExpiresAt
			res.Message = fmt.Sprintf("领取成功 +%d %s", res.Amount, target.CampaignKey)
		}
		return *res
	}
	res.Message = fmt.Sprintf("未知状态: %s", cr.Status)
	return *res
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------- scheduler ----------

var (
	checkinMu   sync.Mutex
	checkinDone = map[string]bool{}
)

var defaultCheckinTimes = []string{"10:00"}

func checkinTimes() []string {
	cfg := currentConfig()
	if len(cfg.CheckinTimes) > 0 {
		return cfg.CheckinTimes
	}
	return defaultCheckinTimes
}

func parseHHMM(s string) (int, int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

var checkinSchedulerOnce sync.Once

// startCheckinScheduler launches the background auto check-in loop once.
func startCheckinScheduler() {
	checkinSchedulerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				runScheduledCheckin()
			}
		}()
		logger.Info("qoder auto checkin scheduler started (times=%s)", strings.Join(checkinTimes(), ","))
	})
}

func runScheduledCheckin() bool {
	cfg := currentConfig()
	if !cfg.AutoCheckin {
		return false
	}
	now := time.Now()
	date := now.Format("2006-01-02")
	nowMin := now.Hour()*60 + now.Minute()

	ran := false
	for _, t := range checkinTimes() {
		h, m, ok := parseHHMM(t)
		if !ok {
			continue
		}
		slotMin := h*60 + m
		if nowMin < slotMin {
			continue
		}
		key := date + " " + t
		checkinMu.Lock()
		done := checkinDone[key]
		checkinMu.Unlock()
		if done {
			continue
		}

		logger.Info("[Checkin] auto checkin triggered at %s (slot %s)", now.Format("15:04:05"), t)
		results := checkinAllAccounts()
		retryable := 0
		for _, r := range results {
			switch r.Status {
			case checkinStatusError, checkinStatusNoCampaign, checkinStatusNoToken:
				retryable++
			}
		}
		checkinMu.Lock()
		if retryable == 0 || nowMin >= slotMin+120 {
			checkinDone[key] = true
		}
		checkinMu.Unlock()
		ran = true
	}
	return ran
}
