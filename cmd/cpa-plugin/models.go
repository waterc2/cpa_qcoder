package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"qoder2api/internal/bridge"
)

// fallbackModels is the static model catalog used before an account has been
// queried (and as a fallback when the upstream model list is unavailable).
// Display names, prices and reasoning flags mirror the 2026-10-20 Global
// /algo/api/v2/model/list snapshot (see docs/upstream-notes.md). CN-only models
// (q37fmodel / gm51model) are omitted here because the fallback has no region
// context; a CN auth gets the live list after its first query.
var fallbackModels = []bridge.QoderModel{
	{Key: "auto", DisplayName: "Auto", PriceFactor: 0.5, ContextWindow: 200000, MaxOutputTokens: 16384},
	{Key: "ultimate", DisplayName: "Ultimate", PriceFactor: 0.4, IsReasoning: true, ContextWindow: 200000, MaxOutputTokens: 32768},
	{Key: "performance", DisplayName: "Performance", PriceFactor: 1.1, ContextWindow: 200000, MaxOutputTokens: 16384},
	{Key: "efficient", DisplayName: "Efficient", PriceFactor: 0.3, ContextWindow: 200000, MaxOutputTokens: 16384},
	{Key: "qmodel_38max", DisplayName: "Qwen3.8-Max", PriceFactor: 0.2, IsReasoning: true, ContextWindow: 200000, MaxOutputTokens: 32768},
	{Key: "qfmodel", DisplayName: "Qwen3.8-Flash", PriceFactor: 0, IsReasoning: true, ContextWindow: 200000, MaxOutputTokens: 32768},
	{Key: "qmodel_latest", DisplayName: "Qwen3.7-Max", PriceFactor: 0.5, ContextWindow: 200000, MaxOutputTokens: 16384},
	{Key: "qmodel", DisplayName: "Qwen3.7-Plus", PriceFactor: 0.1, ContextWindow: 200000, MaxOutputTokens: 16384},
	{Key: "gmodel", DisplayName: "GLM-5.3", PriceFactor: 0.8, IsReasoning: true, ContextWindow: 200000, MaxOutputTokens: 32768},
	{Key: "gfmodel", DisplayName: "GLM-5.3-Flash", PriceFactor: 0.1, IsReasoning: true, ContextWindow: 200000, MaxOutputTokens: 32768},
	{Key: "kmodel_latest", DisplayName: "Kimi-K3", PriceFactor: 1.4, ContextWindow: 200000, MaxOutputTokens: 16384},
	{Key: "kmodel", DisplayName: "Kimi-K2.8-Preview", PriceFactor: 0.8, ContextWindow: 200000, MaxOutputTokens: 16384},
	{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro", PriceFactor: 0.5, IsReasoning: true, ContextWindow: 200000, MaxOutputTokens: 32768},
	{Key: "dfmodel", DisplayName: "DeepSeek-Flash", PriceFactor: 0, IsReasoning: true, ContextWindow: 200000, MaxOutputTokens: 32768},
	{Key: "mmodel", DisplayName: "MiniMax-M3.1-Flash", PriceFactor: 0.1, ContextWindow: 200000, MaxOutputTokens: 16384},
}

func staticModelsRaw() []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(fallbackModels))
	for _, m := range fallbackModels {
		if m.ContextWindow == 0 {
			m.ContextWindow = 180000
		}
		if m.MaxOutputTokens == 0 {
			m.MaxOutputTokens = 16384
		}
		m.Enable = true
		out = append(out, modelInfoFor(m))
	}
	return out
}

// staticModels is the fallback catalog with the user's model selection applied.
func staticModels() []pluginapi.ModelInfo {
	return applyModelSelection(staticModelsRaw())
}

func modelInfoFor(m bridge.QoderModel) pluginapi.ModelInfo {
	ctx := m.ContextWindow
	if ctx == 0 {
		ctx = m.MaxInputTokens
	}
	if ctx == 0 {
		ctx = 180000
	}
	maxOut := m.MaxOutputTokens
	if maxOut == 0 {
		maxOut = 16384
	}
	return pluginapi.ModelInfo{
		ID:                         m.Key,
		Object:                     "model",
		Created:                    1687864820,
		OwnedBy:                    "qoder",
		Type:                       "chat",
		DisplayName:                firstNonEmpty(m.DisplayName, m.Key),
		Name:                       m.Key,
		InputTokenLimit:            int64(ctx),
		ContextLength:              int64(ctx),
		MaxCompletionTokens:        int64(maxOut),
		OutputTokenLimit:           int64(maxOut),
		SupportedInputModalities:   []string{"text", "image"},
		SupportedOutputModalities:  []string{"text"},
		SupportedGenerationMethods: []string{"chat.completions"},
	}
}

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	models := filterExcludedModels(staticModels(), req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: filterExcludedModels(staticModels(), req.Host)})
	}
	models := staticModels()
	if b, err := bridgeForAuth(sa); err == nil {
		if list, err := b.ListAvailableModels(); err == nil && len(list) > 0 {
			models = make([]pluginapi.ModelInfo, 0, len(list))
			for _, m := range list {
				if !m.Enable {
					continue
				}
				models = append(models, modelInfoFor(m))
			}
			if len(models) == 0 {
				models = staticModels()
			}
		}
	}
	models = applyModelSelection(models)
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	excluded := host.ExcludedModels[providerName]
	if len(excluded) == 0 {
		for channel, list := range host.ExcludedModels {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				excluded = list
				break
			}
		}
	}
	if len(excluded) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}
