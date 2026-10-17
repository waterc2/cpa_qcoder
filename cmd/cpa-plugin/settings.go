package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"qoder2api/account"
)

// persistedConfig stores the plugin-owned config fields the operator changed via
// the management panel. The host config (plugins.configs.qoder.*) is applied
// first on every plugin.register/reconfigure; this file is then layered on top
// so panel changes survive a CPA restart. It lives in the plugin data dir next
// to models.json and checkin_history.json.
type persistedConfig struct {
	CheckinAuto    *bool    `json:"checkin_auto,omitempty"`
	CheckinTimes   []string `json:"checkin_times,omitempty"`
	CheckinOnStart *bool    `json:"checkin_on_start,omitempty"`
	DefaultRegion  string   `json:"default_region,omitempty"`
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
	configMu.Unlock()
}

// savePersistedConfig writes the current panel-owned config to disk.
func savePersistedConfig() {
	configMu.RLock()
	pc := persistedConfig{
		CheckinAuto:    boolPtr(activeConfig.AutoCheckin),
		CheckinTimes:   append([]string(nil), activeConfig.CheckinTimes...),
		CheckinOnStart: boolPtr(activeConfig.CheckinOnStart),
		DefaultRegion:  activeConfig.DefaultRegion,
	}
	configMu.RUnlock()

	raw, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return
	}
	path := persistedConfigPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, raw, 0o600)
}

func boolPtr(b bool) *bool { return &b }
