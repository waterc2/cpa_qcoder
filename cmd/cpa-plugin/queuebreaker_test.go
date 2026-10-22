package main

import (
	"testing"
	"time"
)

func TestQueueBreakerCountsDistinctSessions(t *testing.T) {
	b := newQoderQueueBreaker(true, 2, time.Minute, time.Minute, time.Minute)
	models := []string{"dfmodel", "deepseek-v4.1-flash"}

	b.recordQueue(models, "session-a")
	if b.shouldSkipQoder(models) {
		t.Fatal("breaker opened after one session")
	}
	b.recordQueue(models, "session-a")
	if b.shouldSkipQoder(models) {
		t.Fatal("same session counted twice")
	}
	b.recordQueue(models, "session-b")
	if !b.shouldSkipQoder(models) {
		t.Fatal("breaker did not open after two distinct sessions")
	}
}

func TestQueueBreakerSuccessRecoversWhenQueueEmpty(t *testing.T) {
	b := newQoderQueueBreaker(true, 2, time.Minute, time.Minute, time.Minute)
	models := []string{"dfmodel"}

	b.recordQueue(models, "session-a")
	b.recordQueue(models, "session-b")
	if !b.shouldSkipQoder(models) {
		t.Fatal("breaker not open")
	}

	b.recordDone(models, "session-a", true)
	if !b.shouldSkipQoder(models) {
		t.Fatal("breaker recovered while another session was still waiting")
	}
	b.recordDone(models, "session-b", true)
	if b.shouldSkipQoder(models) {
		t.Fatal("breaker did not recover after the queue emptied")
	}
}

func TestQueueBreakerFailureKeepsCooldown(t *testing.T) {
	b := newQoderQueueBreaker(true, 1, time.Minute, time.Minute, time.Minute)
	models := []string{"dfmodel"}

	b.recordQueue(models, "session-a")
	b.recordDone(models, "session-a", false)
	if !b.shouldSkipQoder(models) {
		t.Fatal("failed request cleared the cooldown")
	}
}

func TestQueueBreakerDisabled(t *testing.T) {
	b := newQoderQueueBreaker(false, 1, time.Minute, time.Minute, time.Minute)
	b.recordQueue([]string{"dfmodel"}, "session-a")
	if b.shouldSkipQoder([]string{"dfmodel"}) {
		t.Fatal("disabled breaker opened")
	}
}

func TestQueueBreakerSnapshot(t *testing.T) {
	b := newQoderQueueBreaker(true, 2, time.Minute, time.Minute, time.Minute)
	b.recordQueue([]string{"dfmodel"}, "session-a")
	b.recordQueue([]string{"dfmodel"}, "session-b")

	snap := b.snapshot()
	models, _ := snap["models"].(map[string]any)
	entry, _ := models["dfmodel"].(map[string]any)
	if entry == nil {
		t.Fatalf("snapshot missing model: %#v", snap)
	}
	if got := entry["waiting_sessions"]; got != 2 {
		t.Fatalf("waiting_sessions = %v, want 2", got)
	}
	if _, ok := entry["cooldown_until"]; !ok {
		t.Fatalf("snapshot missing cooldown_until: %#v", entry)
	}
}
