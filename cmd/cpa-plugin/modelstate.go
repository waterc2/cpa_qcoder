package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"qoder2api/account"
)

// modelSelection persists the user's model-exposure choices. It is a denylist:
// every discovered model is exposed by default; unchecking one adds it here, so
// newly discovered upstream models stay visible without an extra opt-in step.
type modelSelection struct {
	Disabled []string `json:"disabled"`
}

var (
	modelSelMu   sync.Mutex
	modelSelData *modelSelection // nil until first load
)

func modelSelectionPath() string {
	return filepath.Join(account.DataRoot(), "models.json")
}

// loadModelSelectionLocked returns the cached selection, loading it from disk on
// first use. Caller must hold modelSelMu.
func loadModelSelectionLocked() *modelSelection {
	if modelSelData != nil {
		return modelSelData
	}
	s := &modelSelection{}
	if raw, err := os.ReadFile(modelSelectionPath()); err == nil {
		_ = json.Unmarshal(raw, s)
	}
	modelSelData = s
	return s
}

func saveModelSelection(s *modelSelection) error {
	modelSelMu.Lock()
	modelSelData = s
	raw, _ := json.MarshalIndent(s, "", "  ")
	modelSelMu.Unlock()
	return os.WriteFile(modelSelectionPath(), raw, 0o600)
}

// modelDisabledSet returns a lower-cased set of disabled model ids.
func modelDisabledSet() map[string]bool {
	modelSelMu.Lock()
	defer modelSelMu.Unlock()
	s := loadModelSelectionLocked()
	out := make(map[string]bool, len(s.Disabled))
	for _, id := range s.Disabled {
		out[strings.ToLower(strings.TrimSpace(id))] = true
	}
	return out
}

// applyModelSelection drops models the user has unchecked.
func applyModelSelection(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	disabled := modelDisabledSet()
	if len(disabled) == 0 {
		return models
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if disabled[strings.ToLower(m.ID)] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// modelConfigView is the payload for GET/POST /models/config: the full catalog
// with an enabled flag per model, plus the raw disabled list.
func modelConfigView() map[string]any {
	models := modelCatalogForUI()
	disabled := modelDisabledSet()
	list := make([]map[string]any, 0, len(models))
	for _, m := range models {
		list = append(list, map[string]any{
			"id":        m.ID,
			"name":      firstNonEmpty(m.DisplayName, m.ID),
			"context":   m.ContextLength,
			"enabled":   !disabled[strings.ToLower(m.ID)],
			"reasoning": m.Thinking != nil,
		})
	}
	dis := make([]string, 0, len(disabled))
	for id := range disabled {
		dis = append(dis, id)
	}
	sort.Strings(dis)
	return map[string]any{"models": list, "disabled": dis}
}

func handleModelConfigUpdate(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Disabled []string `json:"disabled"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return map[string]any{"error": "invalid body: " + err.Error()}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(body.Disabled))
	for _, id := range body.Disabled {
		id = strings.TrimSpace(id)
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		out = append(out, id)
	}
	if err := saveModelSelection(&modelSelection{Disabled: out}); err != nil {
		return map[string]any{"error": err.Error()}
	}
	return modelConfigView()
}

// modelCatalogForUI merges the static catalog with every account's live upstream
// list so the panel can offer the full set of selectable models.
func modelCatalogForUI() []pluginapi.ModelInfo {
	seen := map[string]bool{}
	var out []pluginapi.ModelInfo
	add := func(ms []pluginapi.ModelInfo) {
		for _, m := range ms {
			if m.ID == "" || seen[strings.ToLower(m.ID)] {
				continue
			}
			seen[strings.ToLower(m.ID)] = true
			out = append(out, m)
		}
	}
	add(staticModelsRaw())
	if files, err := hostAuthList(); err == nil {
		for _, f := range files {
			sa, err := resolveAuth(f)
			if err != nil {
				continue
			}
			b, err := bridgeForAuth(sa)
			if err != nil {
				continue
			}
			if list, err := b.ListAvailableModels(); err == nil && len(list) > 0 {
				ms := make([]pluginapi.ModelInfo, 0, len(list))
				for _, m := range list {
					ms = append(ms, modelInfoFor(m))
				}
				add(ms)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
