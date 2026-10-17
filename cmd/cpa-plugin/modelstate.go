package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"qoder2api/account"
	"qoder2api/internal/bridge"
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

// modelIsDisabled reports whether a model id is hidden by the user's denylist.
func modelIsDisabled(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	return modelDisabledSet()[id]
}

// ensureModelEnabled rejects requests for models the user unchecked. CPA keeps
// serving its previously registered model registry after a selection change, so
// this is the enforcement point that makes the denylist effective immediately
// (and it also covers host-registered aliases such as qoder-auto).
func ensureModelEnabled(qr *qoderRequest) error {
	if qr == nil {
		return nil
	}
	if modelIsDisabled(qr.model) || modelIsDisabled(qr.clientModel) {
		return fmt.Errorf("model %s is disabled by the Qoder model selection", firstNonEmpty(qr.clientModel, qr.model))
	}
	return nil
}

// refreshHostAuthFiles re-saves every Qoder auth file with a bumped revision so
// the host file watcher re-parses it and re-registers its models. The host has
// no explicit "re-register models" RPC, so this is how a model selection change
// propagates to /v1/models without restarting CPA. Best effort: failures are
// ignored because the executor still enforces the denylist.
func refreshHostAuthFiles() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	rev := time.Now().UnixNano()
	for _, f := range files {
		raw, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			continue
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		doc["model_revision"] = rev
		out, err := json.Marshal(doc)
		if err != nil {
			continue
		}
		_ = hostAuthSave(f.Name, out)
	}
}

// catalogModel is a UI-facing model entry: the host ModelInfo plus the upstream
// credit multiplier and reasoning flag the panel wants to display.
type catalogModel struct {
	Info        pluginapi.ModelInfo
	PriceFactor float64
	Reasoning   bool
}

// modelConfigView is the payload for GET/POST /models/config: the full catalog
// with an enabled flag per model, plus the raw disabled list.
func modelConfigView() map[string]any {
	models := modelCatalogForUI()
	disabled := modelDisabledSet()
	list := make([]map[string]any, 0, len(models))
	for _, m := range models {
		list = append(list, map[string]any{
			"id":           m.Info.ID,
			"name":         firstNonEmpty(m.Info.DisplayName, m.Info.ID),
			"context":      m.Info.ContextLength,
			"enabled":      !disabled[strings.ToLower(m.Info.ID)],
			"reasoning":    m.Reasoning,
			"price_factor": m.PriceFactor,
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
	// Ask the host to re-register models so the change is reflected in
	// /v1/models without a CPA restart (best effort; executor enforces anyway).
	refreshHostAuthFiles()
	return modelConfigView()
}

// modelCatalogForUI merges every account's live upstream list with the static
// fallback so the panel can offer the full set of selectable models.
//
// The live upstream list is collected FIRST so its real display names (e.g.
// "Qwen3.8-Max") win over the fallback catalog; the fallback only fills in
// models no account reported. This fixes the panel showing raw internal keys
// such as "qmodel_38max" because the static list used to shadow the live one.
func modelCatalogForUI() []catalogModel {
	var live []bridge.QoderModel
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
			if list, err := b.ListAvailableModels(); err == nil {
				live = append(live, list...)
			}
		}
	}
	return mergeCatalog(live, fallbackModels)
}

// mergeCatalog deduplicates models by id, giving the live upstream list
// priority over the static fallback. Exported behavior is covered by
// TestMergeCatalogPrefersLiveDisplayName.
func mergeCatalog(live, fallback []bridge.QoderModel) []catalogModel {
	byID := map[string]catalogModel{}
	order := []string{}
	add := func(m bridge.QoderModel) {
		info := modelInfoFor(m)
		if info.ID == "" {
			return
		}
		key := strings.ToLower(info.ID)
		if _, ok := byID[key]; ok {
			return
		}
		byID[key] = catalogModel{Info: info, PriceFactor: m.PriceFactor, Reasoning: m.IsReasoning}
		order = append(order, key)
	}
	for _, m := range live {
		add(m)
	}
	for _, m := range fallback {
		add(m)
	}

	out := make([]catalogModel, 0, len(order))
	for _, k := range order {
		out = append(out, byID[k])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Info.ID < out[j].Info.ID })
	return out
}
