// request_usage.go implements the per-request Qoder usage log shown at the
// bottom of the panel. The host calls usage.handle after every request with a
// canonical pluginapi.UsageRecord; we persist account/model/token/latency data
// as append-only JSONL under the plugin data dir.
//
// Source of truth is the JSONL file. An in-memory ring (recent records) serves
// fast panel renders and a day-bucket cache backs the summary totals. Rotation
// splits the file at ~50MB into request-usage-<YYYY-MM>.jsonl; old data is
// retained, never deleted. No credentials or prompt content are stored.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"qoder2api/account"
)

type requestUsageRecord struct {
	Timestamp       string `json:"timestamp"`
	AuthIndex       string `json:"auth_index"`
	AuthID          string `json:"auth_id,omitempty"`
	Nickname        string `json:"nickname,omitempty"`
	Email           string `json:"email,omitempty"`
	Region          string `json:"region,omitempty"`
	Model           string `json:"model"` // upstream model key
	Alias           string `json:"alias,omitempty"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens,omitempty"`
	CachedTokens    int64  `json:"cached_tokens,omitempty"`
	TotalTokens     int64  `json:"total_tokens"`
	Failed          bool   `json:"failed"`
	StatusCode      int    `json:"status_code,omitempty"`
	Error           string `json:"error,omitempty"`
	LatencyMS       int64  `json:"latency_ms"`
}

const (
	usageLogBaseName = "request-usage.jsonl"
	usageLogMaxBytes = 50 * 1024 * 1024
	usageRingSize    = 200
	usageLimitMax    = 1000
)

type usageDayAgg struct {
	Requests     int64
	Failed       int64
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

type requestUsageStore struct {
	mu sync.Mutex

	dir  string
	path string

	ring   []requestUsageRecord
	perDay map[string]map[string]*usageDayAgg // local date -> auth -> agg
	loaded bool
}

var usageStore = &requestUsageStore{perDay: map[string]map[string]*usageDayAgg{}}

func usageLogDir() string {
	return filepath.Join(account.DataRoot(), "usage")
}

func (s *requestUsageStore) ensureLogDir() string {
	if s.path != "" {
		return s.path
	}
	dir := usageLogDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	s.dir = dir
	s.path = filepath.Join(dir, usageLogBaseName)
	return s.path
}

func (s *requestUsageStore) jsonlFiles() []string {
	matches, _ := filepath.Glob(filepath.Join(s.dir, "request-usage-*.jsonl"))
	files := make([]string, 0, len(matches)+1)
	if s.path != "" {
		files = append(files, s.path)
	}
	for _, m := range matches {
		if m != s.path {
			files = append(files, m)
		}
	}
	sort.Strings(files)
	return files
}

func usageBucketKey(rec requestUsageRecord) string {
	if key := firstNonEmpty(rec.AuthIndex, rec.AuthID); key != "" {
		return key
	}
	return "_unknown"
}

// bucketAdd folds one record into the per-day x per-account cache.
// Caller must hold s.mu.
func (s *requestUsageStore) bucketAdd(rec requestUsageRecord) {
	ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return
	}
	key := ts.Local().Format("2006-01-02")
	auth := usageBucketKey(rec)
	m := s.perDay[key]
	if m == nil {
		m = map[string]*usageDayAgg{}
		s.perDay[key] = m
	}
	a := m[auth]
	if a == nil {
		a = &usageDayAgg{}
		m[auth] = a
	}
	a.Requests++
	if rec.Failed {
		a.Failed++
	}
	a.InputTokens += rec.InputTokens
	a.OutputTokens += rec.OutputTokens
	a.TotalTokens += rec.TotalTokens
}

func (s *requestUsageStore) loadFromDisk() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return
	}
	if s.dir == "" {
		s.ensureLogDir()
	}
	s.perDay = map[string]map[string]*usageDayAgg{}
	var newest []requestUsageRecord
	for _, f := range s.jsonlFiles() {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec requestUsageRecord
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue // tolerate a corrupt trailing line
			}
			s.bucketAdd(rec)
			newest = append(newest, rec)
		}
	}
	if len(newest) > usageRingSize {
		newest = newest[len(newest)-usageRingSize:]
	}
	s.ring = newest
	s.loaded = true
}

func (s *requestUsageStore) record(rec requestUsageRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bucketAdd(rec)
	s.ring = append(s.ring, rec)
	if len(s.ring) > usageRingSize {
		s.ring = s.ring[len(s.ring)-usageRingSize:]
	}
	path := s.ensureLogDir()
	if path == "" {
		return
	}
	ts, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if fi, err := os.Stat(path); err == nil && fi.Size() > usageLogMaxBytes {
		rot := filepath.Join(s.dir, "request-usage-"+ts.Local().Format("2006-01")+".jsonl")
		if _, err := os.Stat(rot); err == nil {
			rot = filepath.Join(s.dir, "request-usage-"+ts.Local().Format("2006-01")+".part-"+ts.UTC().Format("20060102T150405.000000000")+".jsonl")
		}
		if rot != path {
			_ = os.Rename(path, rot)
		}
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// queryRecords filters by auth_index/from/to, newest first, up to limit.
func (s *requestUsageStore) queryRecords(authIndex, from, to string, limit int) []requestUsageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	fromT, toT := parseUsageRange(from, to)
	if authIndex == "" && fromT.IsZero() && toT.IsZero() && len(s.ring) >= limit {
		out := make([]requestUsageRecord, 0, limit)
		for i := len(s.ring) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, s.ring[i])
		}
		return out
	}
	var out []requestUsageRecord
	files := s.jsonlFiles()
scan:
	for fi := len(files) - 1; fi >= 0 && len(out) < limit; fi-- {
		raw, err := os.ReadFile(files[fi])
		if err != nil {
			continue
		}
		lines := strings.Split(string(raw), "\n")
		for li := len(lines) - 1; li >= 0 && len(out) < limit; li-- {
			line := strings.TrimSpace(lines[li])
			if line == "" {
				continue
			}
			var rec requestUsageRecord
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			if authIndex != "" && rec.AuthIndex != authIndex && rec.AuthID != authIndex {
				continue
			}
			ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
			if err != nil {
				continue
			}
			local := ts.Local()
			if !toT.IsZero() && local.After(toT) {
				continue
			}
			if !fromT.IsZero() && local.Before(fromT) {
				if li == len(lines)-1 {
					break scan
				}
				continue
			}
			out = append(out, rec)
		}
	}
	return out
}

func parseUsageRange(from, to string) (fromT, toT time.Time) {
	if f := strings.TrimSpace(from); f != "" {
		if t, err := parseUsageFlexTime(f); err == nil {
			fromT = t
		}
	}
	if t := strings.TrimSpace(to); t != "" {
		if v, err := parseUsageFlexTime(t); err == nil {
			toT = v
			if !strings.Contains(t, "T") {
				toT = v.Add(24*time.Hour - time.Second)
			}
		}
	}
	return
}

func parseUsageFlexTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, errors.New("empty time")
	}
	if strings.Contains(v, "T") {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, v); err == nil {
				return t, nil
			}
		}
		return time.Time{}, errors.New("bad time")
	}
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("bad time")
}

func (s *requestUsageStore) totalsFor(fromT, toT time.Time, authIndex string) (requests, failed, totalTokens int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for day, m := range s.perDay {
		t, err := time.Parse("2006-01-02", day)
		if err != nil {
			continue
		}
		if !fromT.IsZero() && t.Before(fromT) {
			continue
		}
		if !toT.IsZero() && t.After(toT) {
			continue
		}
		for auth, a := range m {
			if authIndex != "" && auth != authIndex {
				continue
			}
			requests += a.Requests
			failed += a.Failed
			totalTokens += a.TotalTokens
		}
	}
	return
}

func (s *requestUsageStore) dayTotals(day string) (requests, failed, totalTokens int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.perDay[day] {
		requests += a.Requests
		failed += a.Failed
		totalTokens += a.TotalTokens
	}
	return
}

// usageAuthMeta is cached per-account display metadata.
type usageAuthMeta struct {
	AuthIndex string
	Nickname  string
	Email     string
	Region    string
}

const (
	usageMetaTTL        = 60 * time.Second
	usageMetaRetryAfter = 5 * time.Second
)

type usageMetaResolver struct {
	mu        sync.Mutex
	byAuthID  map[string]usageAuthMeta
	byIndex   map[string]usageAuthMeta
	refreshed time.Time
	lastErr   bool
}

var usageMetaCache = &usageMetaResolver{}

func (r *usageMetaResolver) refreshLocked() {
	now := time.Now()
	if r.byAuthID != nil && now.Sub(r.refreshed) < r.retryAfterLocked() {
		return
	}
	byAuthID := map[string]usageAuthMeta{}
	byIndex := map[string]usageAuthMeta{}
	files, err := hostAuthList()
	r.lastErr = err != nil
	if err == nil {
		for _, f := range files {
			meta := usageAuthMeta{
				AuthIndex: f.AuthIndex,
				Nickname:  f.Label,
				Email:     f.Email,
			}
			if sa, err := resolveAuth(f); err == nil {
				meta.Nickname = firstNonEmpty(sa.Name, meta.Nickname)
				meta.Email = firstNonEmpty(sa.Email, meta.Email)
				meta.Region = string(sa.region())
				if id := authIDFor(sa); id != "" {
					byAuthID[id] = meta
				}
			}
			if f.ID != "" {
				byAuthID[f.ID] = meta
			}
			if meta.AuthIndex != "" {
				byIndex[meta.AuthIndex] = meta
			}
		}
	}
	r.byAuthID, r.byIndex = byAuthID, byIndex
	r.refreshed = now
}

func (r *usageMetaResolver) retryAfterLocked() time.Duration {
	if r.lastErr || (len(r.byAuthID) == 0 && len(r.byIndex) == 0) {
		return usageMetaRetryAfter
	}
	return usageMetaTTL
}

func (r *usageMetaResolver) resolve(authID, authIndex string) usageAuthMeta {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked()
	if authID != "" {
		if m, ok := r.byAuthID[authID]; ok {
			return m
		}
	}
	if authIndex != "" {
		if m, ok := r.byIndex[authIndex]; ok {
			return m
		}
	}
	return usageAuthMeta{}
}

func buildUsageRecord(rec pluginapi.UsageRecord) requestUsageRecord {
	model := strings.TrimSpace(rec.Model)
	alias := strings.TrimSpace(rec.Alias)
	if upstream, ok := resolveModelAlias(model); ok {
		alias = model
		model = upstream
	}
	if alias == "" || strings.EqualFold(alias, model) {
		alias = ""
	}
	meta := usageMetaCache.resolve(rec.AuthID, rec.AuthIndex)
	authIndex := firstNonEmpty(rec.AuthIndex, meta.AuthIndex, rec.AuthID)
	region := meta.Region
	if region == "" {
		region = "global"
	}
	ts := rec.RequestedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	latency := rec.Latency.Milliseconds()
	if latency < 0 {
		latency = 0
	}
	total := rec.Detail.TotalTokens
	if total == 0 {
		total = rec.Detail.InputTokens + rec.Detail.OutputTokens + rec.Detail.ReasoningTokens
	}
	errText := strings.TrimSpace(rec.Failure.Body)
	if len(errText) > 512 {
		errText = errText[:512]
	}
	return requestUsageRecord{
		Timestamp:       ts.UTC().Format(time.RFC3339Nano),
		AuthIndex:       authIndex,
		AuthID:          rec.AuthID,
		Nickname:        meta.Nickname,
		Email:           meta.Email,
		Region:          region,
		Model:           model,
		Alias:           alias,
		InputTokens:     rec.Detail.InputTokens,
		OutputTokens:    rec.Detail.OutputTokens,
		ReasoningTokens: rec.Detail.ReasoningTokens,
		CachedTokens:    rec.Detail.CachedTokens,
		TotalTokens:     total,
		Failed:          rec.Failed,
		StatusCode:      rec.Failure.StatusCode,
		Error:           errText,
		LatencyMS:       latency,
	}
}

// handleUsage is the UsagePlugin entry point. It must never block the chat
// path: all errors are swallowed and returned as recorded=false.
func handleUsage(raw []byte) ([]byte, error) {
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return okEnvelope(map[string]any{"recorded": false, "error": err.Error()})
	}
	if rec.Provider != "" && rec.Provider != providerName && rec.Provider != "qoder2api" {
		return okEnvelope(map[string]any{"recorded": false})
	}
	usageStore.record(buildUsageRecord(rec))
	return okEnvelope(map[string]any{"recorded": true})
}

func handleUsageQuery(req pluginapi.ManagementRequest) map[string]any {
	limit := 100
	if vals := req.Query["limit"]; len(vals) > 0 {
		if n, err := fmt.Sscanf(strings.TrimSpace(vals[0]), "%d", &limit); err == nil && n == 1 {
			if limit < 1 {
				limit = 1
			}
			if limit > usageLimitMax {
				limit = usageLimitMax
			}
		} else {
			limit = 100
		}
	}
	authIndex := ""
	if vals := req.Query["auth_index"]; len(vals) > 0 {
		authIndex = strings.TrimSpace(vals[0])
	}
	from, to := "", ""
	if vals := req.Query["from"]; len(vals) > 0 {
		from = strings.TrimSpace(vals[0])
	}
	if vals := req.Query["to"]; len(vals) > 0 {
		to = strings.TrimSpace(vals[0])
	}
	usageStore.loadFromDisk()
	records := usageStore.queryRecords(authIndex, from, to, limit)
	for i := range records {
		if records[i].Nickname == "" || records[i].Email == "" || records[i].Region == "" {
			meta := usageMetaCache.resolve(records[i].AuthID, records[i].AuthIndex)
			if records[i].Nickname == "" {
				records[i].Nickname = meta.Nickname
			}
			if records[i].Email == "" {
				records[i].Email = meta.Email
			}
			if records[i].Region == "" {
				records[i].Region = meta.Region
			}
		}
	}
	if records == nil {
		records = []requestUsageRecord{}
	}
	fromT, toT := parseUsageRange(from, to)
	todayReq, todayFailed, todayTokens := usageStore.dayTotals(time.Now().Format("2006-01-02"))
	allReq, allFailed, allTokens := usageStore.totalsFor(time.Time{}, time.Time{}, "")
	rangeReq, rangeFailed, rangeTokens := usageStore.totalsFor(fromT, toT, authIndex)
	return map[string]any{
		"records":       records,
		"total_records": len(records),
		"totals": map[string]any{
			"today_requests": todayReq,
			"today_failed":   todayFailed,
			"today_tokens":   todayTokens,
			"total_requests": allReq,
			"total_failed":   allFailed,
			"total_tokens":   allTokens,
			"range_requests": rangeReq,
			"range_failed":   rangeFailed,
			"range_tokens":   rangeTokens,
		},
	}
}
