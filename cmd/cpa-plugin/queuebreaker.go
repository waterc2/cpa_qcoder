package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"qoder2api/internal/bridge"
	"qoder2api/logger"
)

// Session-aware queue breaker (half-open).
//
// Existing sessions keep their Qoder binding even while the model queue is
// congested. New sessions are routed to another provider when enough distinct
// Qoder sessions are currently waiting on the same model. If no other provider
// is available, the request still falls back to Qoder so the channel is never
// hard-closed.
const (
	defaultQueueBreakerSessions    = 3
	defaultQueueBreakerCooldown    = 10 * time.Minute
	defaultQueueBreakerMaxCooldown = 60 * time.Minute
)

type queueBreakerModelState struct {
	// waiting maps a session key to the time it first entered the upstream
	// model queue. Entries are removed on request completion or after the TTL.
	waiting map[string]time.Time
	until   time.Time
	level   int
}

type qoderQueueBreakerState struct {
	mu sync.Mutex

	enabled     bool
	threshold   int
	cooldown    time.Duration
	maxCooldown time.Duration
	waitingTTL  time.Duration
	models      map[string]*queueBreakerModelState
}

var qoderQueueBreaker = newQoderQueueBreakerFromEnv()

func newQoderQueueBreakerFromEnv() *qoderQueueBreakerState {
	threshold := envQueueBreakerInt("QODER2API_QUEUE_BREAKER_SESSIONS", defaultQueueBreakerSessions)
	cooldown := envQueueBreakerDuration("QODER2API_QUEUE_BREAKER_COOLDOWN", defaultQueueBreakerCooldown)
	maxCooldown := envQueueBreakerDuration("QODER2API_QUEUE_BREAKER_MAX_COOLDOWN", defaultQueueBreakerMaxCooldown)
	if maxCooldown < cooldown {
		maxCooldown = cooldown
	}
	waitingTTL := envQueueBreakerDuration("QODER2API_QUEUE_BREAKER_WAIT_TTL", bridge.MaxQueueWait+10*time.Minute)
	if waitingTTL < time.Minute {
		waitingTTL = time.Minute
	}
	return newQoderQueueBreaker(
		envQueueBreakerEnabled("QODER2API_QUEUE_BREAKER", true),
		threshold,
		cooldown,
		maxCooldown,
		waitingTTL,
	)
}

func newQoderQueueBreaker(enabled bool, threshold int, cooldown, maxCooldown, waitingTTL time.Duration) *qoderQueueBreakerState {
	if threshold <= 0 {
		threshold = defaultQueueBreakerSessions
	}
	if cooldown <= 0 {
		cooldown = defaultQueueBreakerCooldown
	}
	if maxCooldown < cooldown {
		maxCooldown = cooldown
	}
	if waitingTTL <= 0 {
		waitingTTL = cooldown
	}
	return &qoderQueueBreakerState{
		enabled:     enabled,
		threshold:   threshold,
		cooldown:    cooldown,
		maxCooldown: maxCooldown,
		waitingTTL:  waitingTTL,
		models:      make(map[string]*queueBreakerModelState),
	}
}

func init() {
	bridge.QueueObserver = func(obs bridge.QueueObservation) {
		qoderQueueBreaker.recordQueue([]string{obs.ModelKey, obs.ClientModel}, obs.SessionKey)
	}
}

func (b *qoderQueueBreakerState) modelLocked(key string) *queueBreakerModelState {
	st := b.models[key]
	if st == nil {
		st = &queueBreakerModelState{waiting: make(map[string]time.Time)}
		b.models[key] = st
	}
	return st
}

func (b *qoderQueueBreakerState) pruneLocked(now time.Time) {
	cutoff := now.Add(-b.waitingTTL)
	for _, st := range b.models {
		for session, at := range st.waiting {
			if !at.After(cutoff) {
				delete(st.waiting, session)
			}
		}
	}
}

func (b *qoderQueueBreakerState) cooldownForLocked(level int) time.Duration {
	d := b.cooldown
	for i := 1; i < level && d < b.maxCooldown; i++ {
		if d > b.maxCooldown/2 {
			return b.maxCooldown
		}
		d *= 2
	}
	if d > b.maxCooldown {
		d = b.maxCooldown
	}
	return d
}

// recordQueue marks one session as waiting on the model queue. The same session
// can be recorded repeatedly; it only counts once while it is waiting.
func (b *qoderQueueBreakerState) recordQueue(models []string, sessionKey string) {
	if b == nil || !b.enabled {
		return
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		sessionKey = "unknown"
	}
	keys := normalizeQueueBreakerModels(models)
	if len(keys) == 0 {
		return
	}
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now)

	for _, key := range keys {
		st := b.modelLocked(key)
		before := len(st.waiting)
		st.waiting[sessionKey] = now
		if len(st.waiting) < b.threshold {
			continue
		}

		newEpisode := st.until.IsZero() || !st.until.After(now)
		if newEpisode {
			st.level++
		}
		next := now.Add(b.cooldownForLocked(st.level))
		if next.After(st.until) {
			st.until = next
		}
		if newEpisode || len(st.waiting) > before {
			logger.Info("qoder queue breaker: model=%s waiting_sessions=%d level=%d cooldown_until=%s",
				key, len(st.waiting), st.level, st.until.Format(time.RFC3339))
		}
	}
}

// recordDone removes one session from the waiting set on success. A failed or
// canceled request keeps the session marked as queued so the next attempt for
// that same session can be diverted even when the global threshold has not been
// reached yet. Non-queue failures are not added because recordQueue is the only
// writer of the waiting map.
func (b *qoderQueueBreakerState) recordDone(models []string, sessionKey string, success bool) {
	if b == nil || !b.enabled {
		return
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return
	}
	keys := normalizeQueueBreakerModels(models)
	if len(keys) == 0 {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.pruneLocked(now)
	for _, key := range keys {
		st := b.models[key]
		if st == nil {
			continue
		}
		if !success {
			// Keep the entry: the session itself has experienced the queue and
			// should fail over on its next attempt.
			continue
		}
		delete(st.waiting, sessionKey)
		if len(st.waiting) == 0 {
			st.until = time.Time{}
			st.level = 0
		}
	}
}

// shouldSkipQoder reports whether new sessions for any of the given model keys
// should temporarily avoid Qoder.
func (b *qoderQueueBreakerState) shouldSkipQoder(models []string) bool {
	if b == nil || !b.enabled {
		return false
	}
	keys := normalizeQueueBreakerModels(models)
	if len(keys) == 0 {
		return false
	}
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now)
	for _, key := range keys {
		st := b.models[key]
		if st == nil {
			continue
		}
		if st.until.After(now) || len(st.waiting) >= b.threshold {
			return true
		}
	}
	return false
}

// shouldSkipQoderForSession reports whether this specific session has itself
// been queued for any of the given model keys. This is intentionally separate
// from the global threshold: a client that keeps one long-lived session should
// still fail over after its own request hits the queue.
func (b *qoderQueueBreakerState) shouldSkipQoderForSession(models []string, sessionKey string) bool {
	if b == nil || !b.enabled {
		return false
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return false
	}
	keys := normalizeQueueBreakerModels(models)
	if len(keys) == 0 {
		return false
	}
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now)
	for _, key := range keys {
		st := b.models[key]
		if st == nil {
			continue
		}
		if _, ok := st.waiting[sessionKey]; ok {
			return true
		}
	}
	return false
}

func (b *qoderQueueBreakerState) snapshot() map[string]any {
	if b == nil {
		return nil
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now)

	models := make(map[string]any, len(b.models))
	for key, st := range b.models {
		if len(st.waiting) == 0 && !st.until.After(now) {
			continue
		}
		entry := map[string]any{
			"waiting_sessions": len(st.waiting),
			"level":            st.level,
		}
		if st.until.After(now) {
			entry["cooldown_until"] = st.until.Format(time.RFC3339)
		}
		models[key] = entry
	}
	return map[string]any{
		"enabled":   b.enabled,
		"threshold": b.threshold,
		"cooldown":  b.cooldown.String(),
		"models":    models,
	}
}

func (b *qoderQueueBreakerState) reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.models = make(map[string]*queueBreakerModelState)
	b.mu.Unlock()
}

// applyRuntimeThreshold changes the divert threshold without restarting the
// proxy process. The threshold is otherwise read once from the environment at
// package init, so ops used to need a full restart to retune it. A non-positive
// value is rejected (use the env default instead). Existing waiting sets and
// cooldowns are preserved so an in-flight episode keeps its state.
func (b *qoderQueueBreakerState) applyRuntimeThreshold(threshold int) (int, bool) {
	if b == nil || threshold <= 0 {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.threshold = threshold
	return b.threshold, true
}

// applyRuntimeCooldown changes the divert cooldown base. Takes effect for the
// next episode; an already-open cooldown keeps its absolute deadline.
func (b *qoderQueueBreakerState) applyRuntimeCooldown(cooldown, maxCooldown time.Duration) (time.Duration, time.Duration, bool) {
	if b == nil || cooldown <= 0 {
		return 0, 0, false
	}
	if maxCooldown < cooldown {
		maxCooldown = cooldown
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cooldown = cooldown
	b.maxCooldown = maxCooldown
	return b.cooldown, b.maxCooldown, true
}

func normalizeQueueBreakerModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(model))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func envQueueBreakerEnabled(name string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		return def
	}
	switch raw {
	case "0", "false", "no", "off", "disable", "disabled":
		return false
	case "1", "true", "yes", "on", "enable", "enabled":
		return true
	default:
		logger.Error("invalid %s=%q, falling back to default %v", name, raw, def)
		return def
	}
}

func envQueueBreakerInt(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		logger.Error("invalid %s=%q, falling back to default %d", name, raw, def)
		return def
	}
	return n
}

func envQueueBreakerDuration(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		logger.Error("invalid %s=%q, falling back to default %s", name, raw, def)
		return def
	}
	return d
}

func (b *qoderQueueBreakerState) String() string {
	if b == nil {
		return "<nil>"
	}
	return fmt.Sprintf("queueBreaker(enabled=%v threshold=%d)", b.enabled, b.threshold)
}
