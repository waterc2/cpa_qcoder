package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"qoder2api/logger"
)

// These metadata keys are part of the host/plugin contract. They are kept as
// literals so this plugin does not pull the full sdk/cliproxy/executor package
// (and its translator dependencies) into the c-shared build.
const (
	metadataCallerScope             = "caller_scope"
	metadataCanonicalSessionID      = "canonical_session_id"
	metadataExecutionSessionID      = "execution_session_id"
	metadataDerivedSessionID        = "derived_session_id"
	metadataSelectedAuthID          = "selected_auth_id"
	metadataSessionAffinityProvider = "session_affinity_provider"
)

// The Qoder scheduler spreads concurrent requests across accounts while keeping
// each account below a small in-flight limit (default 4). It also keeps a
// best-effort session binding so a conversation normally stays on one account
// and keeps its prompt cache warm.
//
// Why a plugin scheduler instead of the built-in selector?
//   - The built-in round-robin has no per-account in-flight awareness.
//   - The built-in session-affinity selector cannot be combined with a custom
//     load-aware selector through the plugin API.
//
// The host calls Scheduler.Pick before it selects an auth. We record a short
// "reservation" for every pick so a burst of simultaneous requests cannot all
// choose the same account before the first request reaches the executor. Exact
// in-flight accounting is then done in RequestInterceptor.AfterAuth (which
// knows both RequestID and selected_auth_id) and released by
// RequestLifecyclePlugin.HandleRequestComplete.
const (
	defaultMaxConcurrentPerAuth = 4
	maxConcurrentPerAuthLimit   = 64

	// schedulerReservationTTL covers the short window between Scheduler.Pick
	// and the after-auth interceptor. It only prevents burst stampedes; it is
	// not the real in-flight count.
	schedulerReservationTTL = 15 * time.Second

	// schedulerBindingTTL mirrors CPA's default session-affinity TTL.
	schedulerBindingTTL = time.Hour
)

type schedulerAccountState struct {
	// active is the exact number of attempts currently between after-auth
	// interception and request completion. Retries increment it once per
	// attempted auth.
	active int
	// recent holds scheduler reservations that have not necessarily reached
	// the executor yet. They expire quickly.
	recent []time.Time
}

type schedulerBinding struct {
	authID  string
	expires time.Time
}

type qoderSchedulerState struct {
	mu sync.Mutex

	maxPerAuth int
	accounts   map[string]*schedulerAccountState
	// requestAuths maps one host request to every auth attempted for it. A
	// retry can produce more than one auth, so completion releases all of them.
	requestAuths map[string]map[string]int
	bindings     map[string]schedulerBinding
	lastPicked   string
}

var qoderScheduler = newQoderScheduler(defaultMaxConcurrentPerAuth)

func newQoderScheduler(maxPerAuth int) *qoderSchedulerState {
	if maxPerAuth <= 0 {
		maxPerAuth = defaultMaxConcurrentPerAuth
	}
	return &qoderSchedulerState{
		maxPerAuth:   maxPerAuth,
		accounts:     make(map[string]*schedulerAccountState),
		requestAuths: make(map[string]map[string]int),
		bindings:     make(map[string]schedulerBinding),
	}
}

func (s *qoderSchedulerState) setMax(n int) {
	if s == nil {
		return
	}
	if n <= 0 {
		n = defaultMaxConcurrentPerAuth
	}
	if n > maxConcurrentPerAuthLimit {
		n = maxConcurrentPerAuthLimit
	}
	s.mu.Lock()
	s.maxPerAuth = n
	s.mu.Unlock()
}

func (s *qoderSchedulerState) accountLocked(id string) *schedulerAccountState {
	st := s.accounts[id]
	if st == nil {
		st = &schedulerAccountState{}
		s.accounts[id] = st
	}
	return st
}

func (s *qoderSchedulerState) pruneLocked(now time.Time) {
	cutoff := now.Add(-schedulerReservationTTL)
	for _, st := range s.accounts {
		if len(st.recent) == 0 {
			continue
		}
		keep := st.recent[:0]
		for _, at := range st.recent {
			if at.After(cutoff) {
				keep = append(keep, at)
			}
		}
		st.recent = keep
	}
	for key, binding := range s.bindings {
		if !binding.expires.After(now) {
			delete(s.bindings, key)
		}
	}
}

// loadLocked reports the effective load used for scheduling. max(active,
// recent) avoids double-counting the same request after the after-auth
// interceptor has confirmed a reservation.
func (s *qoderSchedulerState) loadLocked(id string) int {
	st := s.accountLocked(id)
	if len(st.recent) > st.active {
		return len(st.recent)
	}
	return st.active
}

func (s *qoderSchedulerState) reserveLocked(id string, now time.Time) {
	st := s.accountLocked(id)
	st.recent = append(st.recent, now)
	s.lastPicked = id
}

// trackAttempt records an exact in-flight attempt. It is called by the
// after-auth interceptor, which is the first point where both the host
// RequestID and the selected auth ID are available.
func (s *qoderSchedulerState) trackAttempt(requestID, authID string) {
	requestID = strings.TrimSpace(requestID)
	authID = strings.TrimSpace(authID)
	if s == nil || requestID == "" || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempts := s.requestAuths[requestID]
	if attempts == nil {
		attempts = make(map[string]int)
		s.requestAuths[requestID] = attempts
	}
	attempts[authID]++
	s.accountLocked(authID).active++
}

// releaseRequest releases every auth attempt belonging to one completed host
// request. It is called from the request lifecycle completion hook.
func (s *qoderSchedulerState) releaseRequest(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if s == nil || requestID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempts := s.requestAuths[requestID]
	if attempts == nil {
		return
	}
	delete(s.requestAuths, requestID)
	for authID, count := range attempts {
		st := s.accountLocked(authID)
		st.active -= count
		if st.active < 0 {
			st.active = 0
		}
		// Drop the matching reservations too, otherwise a just-completed
		// request keeps inflating the account load until the reservation TTL.
		if count >= len(st.recent) {
			st.recent = nil
		} else if count > 0 {
			st.recent = st.recent[count:]
		}
	}
}

// pick returns (authID, handled). handled=false means "not a Qoder request" or
// "no Qoder candidate", and lets the host fall back to its normal selector.
//
// Session-aware half-open behavior:
//   - a session already bound to Qoder keeps using Qoder, even while the model
//     queue is congested;
//   - a new session is routed to the first non-Qoder candidate while the queue
//     breaker is open;
//   - if no non-Qoder candidate exists, Qoder remains the fallback.
func (s *qoderSchedulerState) pick(req pluginapi.SchedulerPickRequest) (string, bool) {
	if s == nil || !schedulerRequestIsQoder(req) {
		return "", false
	}
	allCandidates := validSchedulerCandidates(req.Candidates)
	candidates := qoderSchedulerCandidates(req.Candidates)
	if len(candidates) == 0 {
		return "", false
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)

	sessionKey := schedulerSessionKey(req.Options)
	boundQoderFull := false
	if sessionKey != "" {
		if binding, ok := s.bindings[sessionKey]; ok && binding.expires.After(now) {
			for _, candidate := range allCandidates {
				if candidate.ID != binding.authID {
					continue
				}
				if !isQoderSchedulerCandidate(candidate) {
					// The session already belongs to another provider; keep it
					// there instead of pulling it back to Qoder.
					s.bindings[sessionKey] = schedulerBinding{authID: candidate.ID, expires: now.Add(schedulerBindingTTL)}
					return candidate.ID, true
				}
				if s.loadLocked(candidate.ID) < s.maxPerAuth {
					s.bindings[sessionKey] = schedulerBinding{authID: candidate.ID, expires: now.Add(schedulerBindingTTL)}
					s.reserveLocked(candidate.ID, now)
					return candidate.ID, true
				}
				boundQoderFull = true
			}
			if boundQoderFull {
				// Keep the binding alive while the bound account is full. This
				// request overflows, but the next one can return to it.
				s.bindings[sessionKey] = schedulerBinding{authID: binding.authID, expires: now.Add(schedulerBindingTTL)}
			}
		}
	}

	// Only a genuinely new session is diverted by the half-open breaker.
	// Existing Qoder sessions keep their binding above, even when the bound
	// account is full and this request has to overflow to another Qoder account.
	if !boundQoderFull && qoderQueueBreaker.shouldSkipQoder(queueBreakerModelsForRequest(req)) {
		if candidate, ok := firstNonQoderCandidate(allCandidates); ok {
			if sessionKey != "" {
				s.bindings[sessionKey] = schedulerBinding{authID: candidate.ID, expires: now.Add(schedulerBindingTTL)}
			}
			logger.Info("qoder queue breaker: diverting new session model=%s session=%s auth=%s", req.Model, sessionKey, candidate.ID)
			return candidate.ID, true
		}
	}

	// Prefer the highest priority tier that still has capacity. Priority is
	// respected even when SchedulerAcrossPriorities exposes lower tiers.
	highest := 0
	haveCapacity := false
	for _, candidate := range candidates {
		if s.loadLocked(candidate.ID) >= s.maxPerAuth {
			continue
		}
		if !haveCapacity || candidate.Priority > highest {
			highest = candidate.Priority
			haveCapacity = true
		}
	}
	if !haveCapacity {
		// Every account is at the soft limit. Stay available instead of
		// rejecting the request: choose the least-loaded highest-priority
		// account. With 9 accounts x 4 slots this path should be rare.
		for _, candidate := range candidates {
			if candidate.Priority > highest {
				highest = candidate.Priority
			}
		}
	}

	tier := make([]pluginapi.SchedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Priority == highest {
			tier = append(tier, candidate)
		}
	}
	chosen := s.chooseLocked(tier)
	if chosen == "" {
		return "", false
	}
	underLimit := s.loadLocked(chosen) < s.maxPerAuth
	s.reserveLocked(chosen, now)
	if sessionKey != "" && underLimit && !boundQoderFull {
		s.bindings[sessionKey] = schedulerBinding{
			authID:  chosen,
			expires: now.Add(schedulerBindingTTL),
		}
	}
	return chosen, true
}

// chooseLocked picks the least-loaded candidate in the tier, using a stable
// round-robin tie break so equal accounts are not always chosen in ID order.
func (s *qoderSchedulerState) chooseLocked(candidates []pluginapi.SchedulerAuthCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].ID < candidates[j].ID
	})
	minLoad := -1
	for _, candidate := range candidates {
		load := s.loadLocked(candidate.ID)
		if minLoad < 0 || load < minLoad {
			minLoad = load
		}
	}
	start := 0
	for i, candidate := range candidates {
		if candidate.ID > s.lastPicked {
			start = i
			break
		}
	}
	for offset := 0; offset < len(candidates); offset++ {
		candidate := candidates[(start+offset)%len(candidates)]
		if s.loadLocked(candidate.ID) == minLoad {
			return candidate.ID
		}
	}
	return candidates[0].ID
}

func (s *qoderSchedulerState) snapshot() map[string]int {
	if s == nil {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	out := make(map[string]int, len(s.accounts))
	for id := range s.accounts {
		out[id] = s.loadLocked(id)
	}
	return out
}

func schedulerRequestIsQoder(req pluginapi.SchedulerPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(req.Provider), providerName) {
		return true
	}
	for _, provider := range req.Providers {
		if strings.EqualFold(strings.TrimSpace(provider), providerName) {
			return true
		}
	}
	for _, candidate := range req.Candidates {
		if isQoderSchedulerCandidate(candidate) {
			return true
		}
	}
	return false
}

func validSchedulerCandidates(in []pluginapi.SchedulerAuthCandidate) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(in))
	for _, candidate := range in {
		if strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func isQoderSchedulerCandidate(candidate pluginapi.SchedulerAuthCandidate) bool {
	if strings.EqualFold(strings.TrimSpace(candidate.Provider), providerName) {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(candidate.ID)), providerName+"-")
}

func firstNonQoderCandidate(in []pluginapi.SchedulerAuthCandidate) (pluginapi.SchedulerAuthCandidate, bool) {
	for _, candidate := range in {
		if strings.TrimSpace(candidate.ID) == "" || isQoderSchedulerCandidate(candidate) {
			continue
		}
		return candidate, true
	}
	return pluginapi.SchedulerAuthCandidate{}, false
}

func queueBreakerModelsForRequest(req pluginapi.SchedulerPickRequest) []string {
	models := []string{req.Model}
	if upstream, ok := resolveModelAlias(req.Model); ok && !strings.EqualFold(upstream, req.Model) {
		models = append(models, upstream)
	}
	return models
}

func qoderSchedulerCandidates(in []pluginapi.SchedulerAuthCandidate) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(in))
	for _, candidate := range in {
		if strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		if candidate.Provider != "" && !strings.EqualFold(strings.TrimSpace(candidate.Provider), providerName) {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func schedulerSessionKey(opts pluginapi.SchedulerOptions) string {
	scope, _ := opts.Metadata[metadataCallerScope].(string)
	scope = strings.TrimSpace(scope)
	withScope := func(id string) string {
		id = strings.TrimSpace(id)
		if id == "" {
			return ""
		}
		if scope != "" {
			return scope + "::" + id
		}
		return id
	}

	// Mirror the explicit session headers recognized by CPA's session-affinity
	// selector. The scheduler API does not carry the request body, so body-only
	// identities such as prompt_cache_key/conversation.id are not available.
	for _, key := range []string{
		"X-Claude-Code-Session-Id",
		"Session-Id",
		"Session_id",
		"X-Http-Session-Id",
		"X-Session-ID",
		"X-Session-Affinity",
		"X-Slot-Session-Id",
		"X-Conversation-Id",
		"X-Thread-Id",
		"X-Client-Request-Id",
	} {
		if id := headerValue(opts.Headers, key); id != "" {
			return withScope(id)
		}
	}

	for _, key := range []string{
		metadataCanonicalSessionID,
		metadataExecutionSessionID,
		metadataDerivedSessionID,
		"session_id",
		"conversation_id",
		"prompt_cache_key",
	} {
		if value, ok := opts.Metadata[key].(string); ok {
			if id := withScope(value); id != "" {
				return id
			}
		}
	}
	return ""
}

func headerValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if authID, handled := qoderScheduler.pick(req); handled {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: authID})
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
}

func handleRequestInterceptBefore(raw []byte) ([]byte, error) {
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

func handleRequestInterceptAfter(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if isQoderIntercept(req) {
		authID, _ := req.Metadata[metadataSelectedAuthID].(string)
		qoderScheduler.trackAttempt(req.RequestID, authID)
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

func handleRequestComplete(raw []byte) ([]byte, error) {
	var completion pluginapi.RequestCompletion
	if err := json.Unmarshal(raw, &completion); err != nil {
		return nil, err
	}
	qoderScheduler.releaseRequest(completion.RequestID)
	return okEnvelope(map[string]any{})
}

func isQoderIntercept(req pluginapi.RequestInterceptRequest) bool {
	if provider, _ := req.Metadata[metadataSessionAffinityProvider].(string); strings.EqualFold(strings.TrimSpace(provider), providerName) {
		return true
	}
	if authID, _ := req.Metadata[metadataSelectedAuthID].(string); strings.HasPrefix(strings.ToLower(strings.TrimSpace(authID)), providerName+"-") {
		return true
	}
	return false
}

func handleSchedulerStatus() map[string]any {
	cfg := currentConfig()
	return map[string]any{
		"max_concurrency": cfg.MaxConcurrentPerAuth,
		"accounts":        qoderScheduler.snapshot(),
		"queue_breaker":   qoderQueueBreaker.snapshot(),
	}
}
