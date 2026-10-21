package bridge

import "testing"

func reasoningBody() map[string]interface{} {
	return map[string]interface{}{
		"model_config": map[string]interface{}{"key": "ultimate", "is_reasoning": false},
		"parameters":   map[string]interface{}{"max_tokens": float64(32768)},
	}
}

func TestApplyReasoningParams(t *testing.T) {
	// Explicit effort enables thinking and records the effort.
	b := reasoningBody()
	applyReasoningParams(b, CallOpts{ReasoningEffort: "high"})
	p := b["parameters"].(map[string]interface{})
	mc := b["model_config"].(map[string]interface{})
	if p["reasoning_effort"] != "high" || p["enable_thinking"] != true || mc["is_reasoning"] != true {
		t.Fatalf("effort=high: params=%+v model_config=%+v", p, mc)
	}

	// effort=none disables thinking and drops any budget.
	b = reasoningBody()
	applyReasoningParams(b, CallOpts{ReasoningEffort: "none", ThinkingBudget: 1000})
	p = b["parameters"].(map[string]interface{})
	mc = b["model_config"].(map[string]interface{})
	if p["enable_thinking"] != false || mc["is_reasoning"] != false {
		t.Fatalf("effort=none: params=%+v model_config=%+v", p, mc)
	}
	if _, ok := p["reasoning_budget_tokens"]; ok {
		t.Fatalf("effort=none should drop reasoning_budget_tokens: %+v", p)
	}

	// Budget alone enables thinking and is forwarded.
	b = reasoningBody()
	applyReasoningParams(b, CallOpts{ThinkingBudget: 4096})
	p = b["parameters"].(map[string]interface{})
	if p["reasoning_budget_tokens"] != 4096 || p["enable_thinking"] != true {
		t.Fatalf("budget only: %+v", p)
	}

	// Unknown effort is ignored rather than sent upstream.
	b = reasoningBody()
	applyReasoningParams(b, CallOpts{ReasoningEffort: "bogus"})
	p = b["parameters"].(map[string]interface{})
	if _, ok := p["reasoning_effort"]; ok {
		t.Fatalf("invalid effort should be ignored: %+v", p)
	}

	// No reasoning options: body untouched.
	b = reasoningBody()
	applyReasoningParams(b, CallOpts{})
	p = b["parameters"].(map[string]interface{})
	mc = b["model_config"].(map[string]interface{})
	if _, ok := p["enable_thinking"]; ok || mc["is_reasoning"] != false {
		t.Fatalf("no-op should not change body: params=%+v model_config=%+v", p, mc)
	}
}
