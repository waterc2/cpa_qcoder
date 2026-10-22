package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func resetSchedulerForTest(t *testing.T, max int) *qoderSchedulerState {
	t.Helper()
	previous := qoderScheduler
	scheduler := newQoderScheduler(max)
	qoderScheduler = scheduler
	t.Cleanup(func() { qoderScheduler = previous })
	return scheduler
}

func resetQueueBreakerForTest(t *testing.T, threshold int, cooldown time.Duration) *qoderQueueBreakerState {
	t.Helper()
	previous := qoderQueueBreaker
	breaker := newQoderQueueBreaker(true, threshold, cooldown, cooldown, time.Minute)
	qoderQueueBreaker = breaker
	t.Cleanup(func() { qoderQueueBreaker = previous })
	return breaker
}

func mixedSchedulerCandidates() []pluginapi.SchedulerAuthCandidate {
	return []pluginapi.SchedulerAuthCandidate{
		{ID: "qoder-a", Provider: providerName, Priority: 1},
		{ID: "qoder-b", Provider: providerName, Priority: 1},
		{ID: "workbuddy-a", Provider: "workbuddy", Priority: 1},
	}
}

func schedulerCandidates(ids ...string) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.SchedulerAuthCandidate{
			ID:       id,
			Provider: providerName,
			Priority: 1,
		})
	}
	return out
}

func schedulerActive(t *testing.T, s *qoderSchedulerState, authID string) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.accounts[authID]; st != nil {
		return st.active
	}
	return 0
}

func TestSchedulerNonQoderIsUnhandled(t *testing.T) {
	s := resetSchedulerForTest(t, 4)
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "codex-a", Provider: "codex", Priority: 1},
		},
	}
	if id, handled := s.pick(req); handled || id != "" {
		t.Fatalf("non-qoder request handled: id=%q handled=%v", id, handled)
	}
}

func TestSchedulerSpreadsAndCapsPerAccount(t *testing.T) {
	s := resetSchedulerForTest(t, 2)
	req := pluginapi.SchedulerPickRequest{
		Provider:   providerName,
		Candidates: schedulerCandidates("qoder-a", "qoder-b"),
	}

	picked := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		id, handled := s.pick(req)
		if !handled || id == "" {
			t.Fatalf("pick %d not handled", i)
		}
		picked = append(picked, id)
		s.trackAttempt("req-"+string(rune('a'+i)), id)
	}
	if picked[0] == picked[1] {
		t.Fatalf("first two picks both used %q, want spread", picked[0])
	}
	if got := schedulerActive(t, s, "qoder-a"); got != 2 {
		t.Fatalf("qoder-a active = %d, want 2", got)
	}
	if got := schedulerActive(t, s, "qoder-b"); got != 2 {
		t.Fatalf("qoder-b active = %d, want 2", got)
	}

	// At the soft cap the scheduler still stays available instead of rejecting.
	if id, handled := s.pick(req); !handled || id == "" {
		t.Fatalf("all-full pick not handled: id=%q handled=%v", id, handled)
	}

	// Completion releases every attempt exactly once.
	s.releaseRequest("req-a")
	s.releaseRequest("req-b")
	s.releaseRequest("req-c")
	s.releaseRequest("req-d")
	if got := schedulerActive(t, s, "qoder-a"); got != 0 {
		t.Fatalf("qoder-a active after release = %d", got)
	}
	if got := schedulerActive(t, s, "qoder-b"); got != 0 {
		t.Fatalf("qoder-b active after release = %d", got)
	}
}

func TestSchedulerSessionStickyAndOverflow(t *testing.T) {
	s := resetSchedulerForTest(t, 4)
	req := pluginapi.SchedulerPickRequest{
		Provider:   providerName,
		Candidates: schedulerCandidates("qoder-a", "qoder-b"),
		Options: pluginapi.SchedulerOptions{
			Metadata: map[string]any{metadataCanonicalSessionID: "session-1"},
		},
	}

	first, handled := s.pick(req)
	if !handled || first == "" {
		t.Fatal("first pick not handled")
	}
	s.trackAttempt("req-1", first)

	second, handled := s.pick(req)
	if !handled || second != first {
		t.Fatalf("same session picked %q after %q, want sticky", second, first)
	}
	s.trackAttempt("req-2", second)

	// Fill the bound account to its limit.
	for i := 0; i < 2; i++ {
		id, handled := s.pick(req)
		if !handled || id != first {
			t.Fatalf("fill pick %d = %q handled=%v, want %q", i, id, handled, first)
		}
		s.trackAttempt("req-fill-"+string(rune('a'+i)), id)
	}
	if got := schedulerActive(t, s, first); got != 4 {
		t.Fatalf("bound account active = %d, want 4", got)
	}

	// The fifth concurrent request for the same session overflows to another
	// account instead of exceeding the cap.
	overflow, handled := s.pick(req)
	if !handled || overflow == "" || overflow == first {
		t.Fatalf("overflow pick = %q handled=%v, want non-bound account", overflow, handled)
	}

	// Releasing one slot returns the session to its original binding.
	s.releaseRequest("req-1")
	if id, handled := s.pick(req); !handled || id != first {
		t.Fatalf("pick after release = %q handled=%v, want %q", id, handled, first)
	}
}

func TestSchedulerReleasesAllRetryAttempts(t *testing.T) {
	s := resetSchedulerForTest(t, 4)
	s.trackAttempt("req-retry", "qoder-a")
	s.trackAttempt("req-retry", "qoder-b")
	if got := schedulerActive(t, s, "qoder-a"); got != 1 {
		t.Fatalf("qoder-a active = %d", got)
	}
	if got := schedulerActive(t, s, "qoder-b"); got != 1 {
		t.Fatalf("qoder-b active = %d", got)
	}
	s.releaseRequest("req-retry")
	if got := schedulerActive(t, s, "qoder-a"); got != 0 {
		t.Fatalf("qoder-a active after release = %d", got)
	}
	if got := schedulerActive(t, s, "qoder-b"); got != 0 {
		t.Fatalf("qoder-b active after release = %d", got)
	}
}

func TestSchedulerLifecycleDispatch(t *testing.T) {
	s := resetSchedulerForTest(t, 4)

	intercept := pluginapi.RequestInterceptRequest{
		RequestID: "req-lifecycle",
		Metadata: map[string]any{
			metadataSelectedAuthID: "qoder-a",
		},
	}
	raw, _ := json.Marshal(intercept)
	if _, err := handleMethod(pluginabi.MethodRequestInterceptAfter, raw); err != nil {
		t.Fatalf("intercept after: %v", err)
	}
	if got := schedulerActive(t, s, "qoder-a"); got != 1 {
		t.Fatalf("active after intercept = %d, want 1", got)
	}

	completion := pluginapi.RequestCompletion{RequestID: "req-lifecycle"}
	raw, _ = json.Marshal(completion)
	if _, err := handleMethod(pluginabi.MethodRequestComplete, raw); err != nil {
		t.Fatalf("request complete: %v", err)
	}
	if got := schedulerActive(t, s, "qoder-a"); got != 0 {
		t.Fatalf("active after completion = %d, want 0", got)
	}
}

func TestSchedulerSessionKeyFromHeaders(t *testing.T) {
	opts := pluginapi.SchedulerOptions{
		Headers: map[string][]string{
			"X-Session-ID": {"header-session"},
		},
	}
	if got := schedulerSessionKey(opts); got != "header-session" {
		t.Fatalf("session key = %q", got)
	}
	opts.Metadata = map[string]any{metadataCallerScope: "caller-a"}
	if got := schedulerSessionKey(opts); got != "caller-a::header-session" {
		t.Fatalf("scoped session key = %q", got)
	}
}

func TestSchedulerHalfOpenKeepsExistingSessionAndDivertsNewSession(t *testing.T) {
	s := resetSchedulerForTest(t, 4)
	breaker := resetQueueBreakerForTest(t, 2, time.Minute)
	candidates := mixedSchedulerCandidates()
	model := "deepseek-v4.1-flash"
	models := []string{"dfmodel", model}

	req := pluginapi.SchedulerPickRequest{
		Provider:   "mixed",
		Providers:  []string{providerName, "workbuddy"},
		Model:      model,
		Candidates: candidates,
		Options: pluginapi.SchedulerOptions{
			Metadata: map[string]any{metadataCanonicalSessionID: "session-existing"},
		},
	}
	first, handled := s.pick(req)
	if !handled || first != "qoder-a" {
		t.Fatalf("initial pick = %q handled=%v, want qoder-a", first, handled)
	}
	s.trackAttempt("req-existing-1", first)

	breaker.recordQueue(models, "session-existing")
	breaker.recordQueue(models, "session-waiting")
	if !breaker.shouldSkipQoder(models) {
		t.Fatal("breaker did not open")
	}

	// Existing Qoder session stays on Qoder even while the breaker is open.
	same, handled := s.pick(req)
	if !handled || same != first {
		t.Fatalf("existing session pick = %q handled=%v, want %q", same, handled, first)
	}

	// A new session is diverted to the first non-Qoder candidate.
	newReq := req
	newReq.Options = pluginapi.SchedulerOptions{
		Metadata: map[string]any{metadataCanonicalSessionID: "session-new"},
	}
	diverted, handled := s.pick(newReq)
	if !handled || diverted != "workbuddy-a" {
		t.Fatalf("new session pick = %q handled=%v, want workbuddy-a", diverted, handled)
	}

	// The diverted session remains bound to that provider.
	again, handled := s.pick(newReq)
	if !handled || again != "workbuddy-a" {
		t.Fatalf("new session repeat pick = %q handled=%v, want workbuddy-a", again, handled)
	}
}

func TestSchedulerHalfOpenFallsBackToQoderWithoutAlternative(t *testing.T) {
	s := resetSchedulerForTest(t, 4)
	breaker := resetQueueBreakerForTest(t, 1, time.Minute)
	models := []string{"dfmodel", "deepseek-v4.1-flash"}
	breaker.recordQueue(models, "session-waiting")

	req := pluginapi.SchedulerPickRequest{
		Provider:   providerName,
		Model:      "deepseek-v4.1-flash",
		Candidates: schedulerCandidates("qoder-a", "qoder-b"),
		Options: pluginapi.SchedulerOptions{
			Metadata: map[string]any{metadataCanonicalSessionID: "session-new"},
		},
	}
	id, handled := s.pick(req)
	if !handled || id != "qoder-a" {
		t.Fatalf("pick = %q handled=%v, want qoder fallback", id, handled)
	}
}

func TestSchedulerHalfOpenDoesNotAffectOtherModels(t *testing.T) {
	s := resetSchedulerForTest(t, 4)
	breaker := resetQueueBreakerForTest(t, 1, time.Minute)
	breaker.recordQueue([]string{"dfmodel", "deepseek-v4.1-flash"}, "session-waiting")

	req := pluginapi.SchedulerPickRequest{
		Provider:   "mixed",
		Providers:  []string{providerName, "workbuddy"},
		Model:      "some-other-model",
		Candidates: mixedSchedulerCandidates(),
		Options: pluginapi.SchedulerOptions{
			Metadata: map[string]any{metadataCanonicalSessionID: "session-new"},
		},
	}
	id, handled := s.pick(req)
	if !handled || id != "qoder-a" {
		t.Fatalf("pick = %q handled=%v, want qoder-a", id, handled)
	}
}
