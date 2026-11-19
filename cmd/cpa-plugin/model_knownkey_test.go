package main

import (
	"testing"

	"qoder2api/internal/bridge"
)

// TestAdvertisedModelsAreKnownKeys locks every model this plugin advertises to
// the host against bridge.IsKnownUpstreamKey.
//
// Upstream issue #13 ("request succeeds but Qoder shows no usage") turned out to
// be a model-key problem: the upstream falls back for a key it does not know,
// returns content and does NOT count the call against quota — and because that
// traffic has no real quota behind it, it also gets rate-limited far more
// often. The bridge now WARNs when the key it is about to send is unknown.
//
// That warning is only useful if it stays quiet for keys that really exist.
// Upstream's knownUpstreamKeys is a single snapshot and already misses two keys
// this catalog advertises (performance, qmodel), so this test fails whenever the
// catalog grows past the known-key set: either the key is real and belongs in
// knownUpstreamKeys, or it is not and we must stop advertising it.
func TestAdvertisedModelsAreKnownKeys(t *testing.T) {
	advertised := append(append([]bridge.QoderModel{}, fallbackModels...), modernFallbackModels...)
	for _, m := range advertised {
		if !bridge.IsKnownUpstreamKey(m.Key) {
			t.Errorf("plugin advertises model key %q (%s) but bridge.IsKnownUpstreamKey says it is unknown — requests for it would log the issue #13 no-quota-counted warning", m.Key, m.DisplayName)
		}
	}
}

// TestAdvertisedDisplayNamesResolveToOwnKey verifies that a client echoing back
// the display name this plugin publishes lands on that model's own upstream key.
//
// The host exposes our models by key, but the panel and the Qoder app show the
// display name, and users copy what they see into ModelMappings and client
// configs. Before issue #13 was fixed such a name was lowercased and sent up as
// a key that does not exist, so the request was served outside of quota. If one
// of these now lands on a *different* model's key, we spend that model's credits
// instead — worse than the original bug, so every row is asserted.
func TestAdvertisedDisplayNamesResolveToOwnKey(t *testing.T) {
	advertised := append(append([]bridge.QoderModel{}, fallbackModels...), modernFallbackModels...)
	for _, m := range advertised {
		if m.DisplayName == "" || m.DisplayName == m.Key {
			continue
		}
		for _, agent := range []string{"codex", "claude", "gemini", ""} {
			if got := bridge.MapModel(agent, m.DisplayName); got != m.Key {
				t.Errorf("MapModel(%q, %q) = %q, want %q", agent, m.DisplayName, got, m.Key)
			}
		}
	}
}
