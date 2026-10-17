package bridge

import "testing"

func TestNormalizeRoleDeveloper(t *testing.T) {
	if got := normalizeRole("developer"); got != "system" {
		t.Fatalf("normalizeRole(developer) = %q, want system", got)
	}
	for _, role := range []string{"system", "user", "assistant", "tool", "function"} {
		if got := normalizeRole(role); got != role {
			t.Fatalf("normalizeRole(%q) = %q, want unchanged", role, got)
		}
	}
}

func TestBuildQoderMessagesMapsDeveloperToSystem(t *testing.T) {
	incoming := []interface{}{
		map[string]interface{}{"role": "developer", "content": "be nice"},
		map[string]interface{}{"role": "user", "content": "hi"},
	}
	msgs := BuildQoderMessages(nil, incoming, "", false)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	first, _ := msgs[0].(map[string]interface{})
	if first == nil || first["role"] != "system" {
		t.Fatalf("developer role was not mapped to system: %+v", msgs[0])
	}
	if first["content"] != "be nice" {
		t.Fatalf("unexpected system content: %+v", first)
	}
	second, _ := msgs[1].(map[string]interface{})
	if second == nil || second["role"] != "user" {
		t.Fatalf("unexpected second message: %+v", msgs[1])
	}
}
