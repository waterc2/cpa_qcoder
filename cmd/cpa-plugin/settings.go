package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qoder2api/account"
)

// persistedConfig stores the plugin-owned config fields the operator changed via
// the management panel. The host config (plugins.configs.qoder.*) is applied
// first on every plugin.register/reconfigure; this file is then layered on top
// so panel changes survive a CPA restart. It lives in the plugin data dir next
// to models.json and checkin_history.json.
type persistedConfig struct {
	CheckinAuto          *bool    `json:"checkin_auto,omitempty"`
	CheckinTimes         []string `json:"checkin_times,omitempty"`
	CheckinOnStart       *bool    `json:"checkin_on_start,omitempty"`
	DefaultRegion        string   `json:"default_region,omitempty"`
	MaxConcurrentPerAuth *int     `json:"max_concurrency,omitempty"`
	// QueueBreakerSessions overrides QODER2API_QUEUE_BREAKER_SESSIONS so the
	// divert threshold can be retuned without restarting the proxy process.
	QueueBreakerSessions *int   `json:"queue_breaker_sessions,omitempty"`
	QueueBreakerCooldown string `json:"queue_breaker_cooldown,omitempty"`
}

func persistedConfigPath() string {
	return filepath.Join(account.DataRoot(), "plugin_settings.json")
}

// loadPersistedConfig overlays the persisted panel config onto activeConfig.
func loadPersistedConfig() {
	raw, err := os.ReadFile(persistedConfigPath())
	if err != nil {
		return
	}
	var pc persistedConfig
	if json.Unmarshal(raw, &pc) != nil {
		return
	}
	configMu.Lock()
	if pc.CheckinAuto != nil {
		activeConfig.AutoCheckin = *pc.CheckinAuto
	}
	if len(pc.CheckinTimes) > 0 {
		activeConfig.CheckinTimes = pc.CheckinTimes
	}
	if pc.CheckinOnStart != nil {
		activeConfig.CheckinOnStart = *pc.CheckinOnStart
	}
	if pc.DefaultRegion != "" {
		activeConfig.DefaultRegion = pc.DefaultRegion
	}
	if pc.MaxConcurrentPerAuth != nil && *pc.MaxConcurrentPerAuth > 0 {
		activeConfig.MaxConcurrentPerAuth = *pc.MaxConcurrentPerAuth
	}
	configMu.Unlock()
	qoderScheduler.setMax(currentConfig().MaxConcurrentPerAuth)

	// Queue-breaker overrides are process-local state, not part of pluginConfig:
	// apply them straight onto the live breaker so a reconfigure picks them up.
	if pc.QueueBreakerSessions != nil {
		qoderQueueBreaker.applyRuntimeThreshold(*pc.QueueBreakerSessions)
	}
	if strings.TrimSpace(pc.QueueBreakerCooldown) != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(pc.QueueBreakerCooldown)); err == nil {
			qoderQueueBreaker.applyRuntimeCooldown(d, qoderQueueBreaker.maxCooldown)
		}
	}
}

// savePersistedConfig writes the current panel-owned config to disk.
func savePersistedConfig() {
	configMu.RLock()
	pc := persistedConfig{
		CheckinAuto:          boolPtr(activeConfig.AutoCheckin),
		CheckinTimes:         append([]string(nil), activeConfig.CheckinTimes...),
		CheckinOnStart:       boolPtr(activeConfig.CheckinOnStart),
		DefaultRegion:        activeConfig.DefaultRegion,
		MaxConcurrentPerAuth: intPtr(activeConfig.MaxConcurrentPerAuth),
	}
	configMu.RUnlock()

	// Preserve breaker overrides that were set through the runtime endpoint.
	if qoderQueueBreaker != nil {
		pc.QueueBreakerSessions = intPtr(qoderQueueBreaker.threshold)
		pc.QueueBreakerCooldown = qoderQueueBreaker.cooldown.String()
	}

	raw, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return
	}
	path := persistedConfigPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, raw, 0o600)
}

func boolPtr(b bool) *bool { return &b }

func intPtr(n int) *int { return &n }
