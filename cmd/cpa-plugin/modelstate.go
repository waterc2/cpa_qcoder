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
	// Aliases maps an upstream model key to the name exposed to CPA clients.
	// The original key is used when an entry is absent/empty.
	Aliases map[string]string `json:"aliases,omitempty"`
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

// modelAliasSnapshot returns a copy of the configured alias map.
func modelAliasSnapshot() map[string]string {
	modelSelMu.Lock()
	defer modelSelMu.Unlock()
	s := loadModelSelectionLocked()
	if len(s.Aliases) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.Aliases))
	for id, alias := range s.Aliases {
		if alias != "" {
			out[id] = alias
		}
	}
	return out
}

// modelAliasFor returns the exposed name for an upstream model id (the id
// itself when no alias is configured).
func modelAliasFor(id string) string {
	aliases := modelAliasSnapshot()
	if len(aliases) == 0 {
		return id
	}
	if alias, ok := aliases[id]; ok && alias != "" {
		return alias
	}
	for key, alias := range aliases {
		if strings.EqualFold(key, id) && alias != "" {
			return alias
		}
	}
	return id
}

// resolveModelAlias maps a client-facing alias back to the upstream model key.
// The bool is false when the name is not a configured alias.
func resolveModelAlias(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	aliases := modelAliasSnapshot()
	for id, alias := range aliases {
		if alias != "" && strings.EqualFold(alias, name) {
			return id, true
		}
	}
	return "", false
}

// applyModelAliases rewrites the exposed model ID/name after the denylist has
// been applied. Filtering must always happen against the original upstream ID.
func applyModelAliases(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	aliases := modelAliasSnapshot()
	if len(aliases) == 0 {
		return models
	}
	out := make([]pluginapi.ModelInfo, len(models))
	copy(out, models)
	for i := range out {
		alias := ""
		if a, ok := aliases[out[i].ID]; ok {
			alias = a
		} else {
			for key, a := range aliases {
				if strings.EqualFold(key, out[i].ID) {
					alias = a
					break
				}
			}
		}
		if alias == "" || strings.EqualFold(alias, out[i].ID) {
			continue
		}
		out[i].ID = alias
		out[i].Name = alias
		out[i].DisplayName = alias
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
	return modelConfigViewFor(modelCatalogForUI())
}

func modelConfigViewFor(models []catalogModel) map[string]any {
	disabled := modelDisabledSet()
	list := make([]map[string]any, 0, len(models))
	for _, m := range models {
		list = append(list, map[string]any{
			"id":           m.Info.ID,
			"name":         firstNonEmpty(m.Info.DisplayName, m.Info.ID),
			"alias":        modelAliasFor(m.Info.ID),
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
		Disabled []string          `json:"disabled"`
		Aliases  map[string]string `json:"aliases"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return map[string]any{"error": "invalid body: " + err.Error()}
	}
	modelSelMu.Lock()
	existing := loadModelSelectionLocked()
	existingDisabled := append([]string(nil), existing.Disabled...)
	existingAliases := map[string]string{}
	for id, alias := range existing.Aliases {
		existingAliases[id] = alias
	}
	modelSelMu.Unlock()

	disabled := body.Disabled
	if disabled == nil {
		disabled = existingDisabled
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(disabled))
	for _, id := range disabled {
		id = strings.TrimSpace(id)
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		out = append(out, id)
	}

	aliases := existingAliases
	var catalog []catalogModel
	if body.Aliases != nil {
		catalog = modelCatalogForUI()
		cleaned, err := normalizeModelAliases(catalog, body.Aliases)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		aliases = cleaned
	}
	if err := saveModelSelection(&modelSelection{Disabled: out, Aliases: aliases}); err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Ask the host to re-register models so the change is reflected in
	// /v1/models without a CPA restart (best effort; executor enforces anyway).
	refreshHostAuthFiles()
	if catalog != nil {
		return modelConfigViewFor(catalog)
	}
	return modelConfigView()
}

// normalizeModelAliases validates the UI-supplied alias map against the current
// catalog and returns only non-default entries keyed by canonical model id.
func normalizeModelAliases(catalog []catalogModel, requested map[string]string) (map[string]string, error) {
	canonical := make(map[string]string, len(catalog))
	for _, m := range catalog {
		canonical[strings.ToLower(m.Info.ID)] = m.Info.ID
	}
	cleaned := make(map[string]string, len(requested))
	for rawID, rawAlias := range requested {
		id := strings.TrimSpace(rawID)
		alias := strings.TrimSpace(rawAlias)
		if id == "" {
			continue
		}
		canonicalID, ok := canonical[strings.ToLower(id)]
		if !ok {
			continue // model no longer in the catalog
		}
		if alias == "" || strings.EqualFold(alias, canonicalID) {
			continue
		}
		if strings.ContainsAny(alias, " \t\r\n") {
			return nil, fmt.Errorf("模型 %s 的映射名称不能包含空白字符", canonicalID)
		}
		cleaned[canonicalID] = alias
	}
	// Exposed names (aliases plus untouched original ids) must be unique.
	seen := make(map[string]string, len(catalog))
	for _, m := range catalog {
		id := m.Info.ID
		exposed := id
		if alias := cleaned[id]; alias != "" {
			exposed = alias
		}
		key := strings.ToLower(exposed)
		if prev, ok := seen[key]; ok && !strings.EqualFold(prev, id) {
			return nil, fmt.Errorf("映射名称 %s 与模型 %s 冲突", exposed, prev)
		}
		seen[key] = id
	}
	return cleaned, nil
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
