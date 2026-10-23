package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"qoder2api/account"
	"qoder2api/logger"
)

// QueueStatus is the parsed upstream model-queue status, mirroring the fields
// the official Qoder CLI reads from GET /api/v2/service/ask/queue/status.
type QueueStatus struct {
	IsQueued          bool   `json:"isQueued"`
	ModelKey          string `json:"modelKey"`
	QueueCount        int    `json:"queueCount"`
	QueueType         string `json:"queueType"`
	ServiceAvailable  bool   `json:"serviceAvailable"`
	RetryAfterSeconds int    `json:"retryAfterSeconds"`
	WaitTime          int    `json:"waitTime"`
}

// QueueError is the parsed "model is queued / resource limited" signal carried
// inside an upstream 403 (code 10605 / subCode RESOURCE_LIMIT). The official CLI
// reacts to this by polling the queue status and waiting instead of failing.
type QueueError struct {
	IsQueued          bool
	ModelKey          string
	QueueType         string
	QueueCount        int
	ServiceAvailable  bool
	RetryAfterSeconds int
	WaitTime          int
}

// QueueObservation describes one request entering the upstream model queue.
// QueueObserver is set by the plugin so the scheduler can implement a
// session-aware half-open breaker. It is nil for the standalone CLI.
type QueueObservation struct {
	ModelKey     string
	RequestSetID string
	SessionKey   string
	ClientModel  string
}

var QueueObserver func(QueueObservation)

type queueContextKey struct{}

type queueContext struct {
	sessionKey  string
	clientModel string
}

// WithQueueContext attaches the session/client-model identity to a request so
// queue observations can be attributed to the right session.
func WithQueueContext(ctx context.Context, sessionKey, clientModel string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, queueContextKey{}, queueContext{
		sessionKey:  strings.TrimSpace(sessionKey),
		clientModel: strings.TrimSpace(clientModel),
	})
}

func queueContextFrom(ctx context.Context) queueContext {
	if ctx == nil {
		return queueContext{}
	}
	value, _ := ctx.Value(queueContextKey{}).(queueContext)
	return value
}

func notifyQueueObserver(ctx context.Context, modelKey, requestSetID string) {
	if QueueObserver == nil {
		return
	}
	qctx := queueContextFrom(ctx)
	sessionKey := qctx.sessionKey
	if sessionKey == "" {
		sessionKey = requestSetID
	}
	QueueObserver(QueueObservation{
		ModelKey:     modelKey,
		RequestSetID: requestSetID,
		SessionKey:   sessionKey,
		ClientModel:  qctx.clientModel,
	})
}

// MaxQueueWait bounds the total time a single client request will wait for the
// model queue to clear. The official CLI defaults to 1h
// (QODER_MODEL_QUEUE_MAX_WAIT_MS); we default lower so a stalled queue does not
// pin a client request forever.
//
// Overridable:
//   - QODER2API_QUEUE_MAX_WAIT (Go duration, e.g. "2m")
//   - QODER_MODEL_QUEUE_MAX_WAIT_MS (integer milliseconds, official CLI name)
var MaxQueueWait = envQueueDuration("QODER2API_QUEUE_MAX_WAIT", "QODER_MODEL_QUEUE_MAX_WAIT_MS", 5*time.Minute)

// PerAccountQueueWait bounds how long a single account waits in the queue
// before the request fails over to the next account. A per-account cap avoids
// burning the whole MaxQueueWait on one queued account when another account (or
// a fresh queue slot) might be free. Set <= 0 to disable per-account failover
// and wait on one account up to MaxQueueWait.
//
// Overridable via QODER2API_QUEUE_PER_ACCOUNT_WAIT (Go duration, e.g. "45s").
var PerAccountQueueWait = envQueueDuration("QODER2API_QUEUE_PER_ACCOUNT_WAIT", "", 60*time.Second)

// envQueueDuration parses a Go duration from primary, or an integer number of
// milliseconds from msEnv (used for the official CLI's env name).
func envQueueDuration(primary, msEnv string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(primary)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		logger.Error("invalid %s=%q, falling back to default %s", primary, v, def)
	}
	if msEnv != "" {
		if v := strings.TrimSpace(os.Getenv(msEnv)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return time.Duration(n) * time.Millisecond
			}
			logger.Error("invalid %s=%q, falling back to default %s", msEnv, v, def)
		}
	}
	return def
}

// QueuePollInterval is the fallback wait between queue retries when upstream
// does not return an explicit retryAfterSeconds/waitTime.
const QueuePollInterval = 15 * time.Second

// queueSignalKeys are the JSON keys the official CLI walks (data/result/message/
// body) to find the queue signal. We do the same recursive descent because the
// payload is often double-encoded as a JSON string inside a JSON string.
func parseQueueError(err error) *QueueError {
	if err == nil {
		return nil
	}
	var ue *UpstreamError
	detail := err.Error()
	if ok := asUpstreamError(err, &ue); ok {
		detail = ue.Detail
	}
	if !strings.Contains(detail, "10605") && !strings.Contains(detail, "isQueued") &&
		!strings.Contains(detail, "RESOURCE_LIMIT") {
		return nil
	}
	found := findQueueSignal(detail)
	if found == nil {
		return nil
	}
	return found
}

func asUpstreamError(err error, out **UpstreamError) bool {
	for err != nil {
		if ue, ok := err.(*UpstreamError); ok {
			*out = ue
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// findQueueSignal recursively walks nested JSON (including JSON encoded inside
// strings) looking for a map that carries isQueued/serviceAvailable.
func findQueueSignal(raw string) *QueueError {
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	return walkQueue(v, 0)
}

func walkQueue(v interface{}, depth int) *QueueError {
	if depth > 12 {
		return nil
	}
	switch x := v.(type) {
	case map[string]interface{}:
		if qe := queueFromMap(x); qe != nil {
			return qe
		}
		for _, key := range []string{"data", "result", "message", "body", "error"} {
			if child, ok := x[key]; ok {
				if qe := walkQueue(child, depth+1); qe != nil {
					return qe
				}
			}
		}
	case []interface{}:
		for _, it := range x {
			if qe := walkQueue(it, depth+1); qe != nil {
				return qe
			}
		}
	case string:
		if !strings.Contains(x, "isQueued") && !strings.Contains(x, "serviceAvailable") {
			return nil
		}
		var inner interface{}
		if json.Unmarshal([]byte(x), &inner) == nil {
			return walkQueue(inner, depth+1)
		}
	}
	return nil
}

func queueFromMap(m map[string]interface{}) *QueueError {
	_, hasQueued := m["isQueued"]
	_, hasAvail := m["serviceAvailable"]
	if !hasQueued && !hasAvail {
		return nil
	}
	qe := &QueueError{
		IsQueued:          boolVal(m, "isQueued"),
		ServiceAvailable:  boolVal(m, "serviceAvailable"),
		ModelKey:          strVal(m, "modelKey"),
		QueueType:         strVal(m, "queueType"),
		QueueCount:        intVal(m, "queueCount"),
		RetryAfterSeconds: intVal(m, "retryAfterSeconds"),
		WaitTime:          intVal(m, "waitTime"),
	}
	return qe
}

func boolVal(m map[string]interface{}, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func strVal(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func intVal(m map[string]interface{}, key string) int {
	switch x := m[key].(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

// GetQueueStatus queries the upstream model queue for one requestSet/model.
func (b *Bridge) GetQueueStatus(ctx context.Context, slot *Slot, requestSetID, modelKey, queueType string) (*QueueStatus, error) {
	base := account.GetEndpoints(slot.Region).InferenceBase
	if base == "" {
		return nil, fmt.Errorf("no inference base for region %s", slot.Region)
	}
	q := url.Values{}
	q.Set("requestSetId", requestSetID)
	q.Set("modelKey", modelKey)
	if queueType != "" {
		q.Set("queueType", queueType)
	}
	full := base + "/api/v2/service/ask/queue/status?" + q.Encode()

	resp, err := slot.client.callGetCtx(ctx, full)
	if err != nil {
		return nil, err
	}
	// The payload may be nested; reuse the recursive walker.
	raw, _ := json.Marshal(resp)
	qe := walkQueue(resp, 0)
	if qe == nil {
		// Fall back to a flat decode.
		var flat QueueStatus
		_ = json.Unmarshal(raw, &flat)
		return &flat, nil
	}
	return &QueueStatus{
		IsQueued:          qe.IsQueued,
		ModelKey:          qe.ModelKey,
		QueueCount:        qe.QueueCount,
		QueueType:         qe.QueueType,
		ServiceAvailable:  qe.ServiceAvailable,
		RetryAfterSeconds: qe.RetryAfterSeconds,
		WaitTime:          qe.WaitTime,
	}, nil
}

// FinishQueue reports queue completion to upstream (best effort), mirroring
// POST /api/v2/service/ask/finish.
func (b *Bridge) FinishQueue(slot *Slot, requestSetID, modelKey, userID string, timeConsumedMs int64) error {
	base := account.GetEndpoints(slot.Region).InferenceBase
	if base == "" {
		return fmt.Errorf("no inference base for region %s", slot.Region)
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"model_key":      modelKey,
		"request_set_id": requestSetID,
		"user_id":        userID,
		"time_consumed":  timeConsumedMs,
	})
	body := map[string]interface{}{
		"payload":       string(payload),
		"encodeVersion": "1",
	}
	_, err := slot.client.callPost(base+"/api/v2/service/ask/finish?Encode=1", body)
	return err
}

// queueWorthSameAccountRetry reports whether a queued signal looks like real
// upstream congestion (queueCount>0 or the service reports itself available),
// which is worth the bounded same-account retry inside streamWithSlot.
// Fake/empty queue signals (queueCount=0 && serviceAvailable=false — an
// upstream glitch) return false so the error bubbles straight to
// streamWithQueueRecovery, which fails over to another account immediately
// instead of burning the same-account retry budget on the same 403.
func queueWorthSameAccountRetry(err error) bool {
	qe := parseQueueError(err)
	if qe == nil {
		return true
	}
	return qe.QueueCount > 0 || qe.ServiceAvailable
}

// queueShouldFailoverImmediately decides whether a queue signal should be
// returned to the caller instead of waiting. Qoder sometimes reports
// queueCount=0 / serviceAvailable=false on submit while the status endpoint
// simultaneously says the model is available; waiting 30s and retrying just
// repeats the same 403. In that case, fail over to the next account/provider
// immediately. The same applies when a retry after a successful status poll is
// queued again.
func queueShouldFailoverImmediately(qe *QueueError, retriedAfterAvailable bool) bool {
	if qe == nil {
		return false
	}
	if retriedAfterAvailable {
		return true
	}
	return qe.QueueCount <= 0 && !qe.ServiceAvailable
}

// waitForQueue drains a queued model: it polls the queue status until the model
// becomes available or the deadline passes. It returns available=true when the
// status endpoint reported the model as available (the caller should retry the
// chat request), or an error when the wait timed out/cancelled.
func (b *Bridge) waitForQueue(ctx context.Context, slot *Slot, qe *QueueError, requestSetID, modelKey string, deadline time.Time) (bool, error) {
	queueType := qe.QueueType
	for {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("model queue wait exceeded (model=%s queueType=%s deadline=%s)",
				modelKey, queueType, deadline.Format(time.RFC3339))
		}

		// Poll before sleeping. The status endpoint often reports the model as
		// available immediately, so sleeping retryAfterSeconds first only adds
		// latency before the next chat attempt.
		st, err := b.GetQueueStatus(ctx, slot, requestSetID, modelKey, queueType)
		if err != nil {
			logger.Info("[ModelQueue] status poll failed: %v", err)
			// Poll failure: fall through and retry the chat anyway after the
			// wait, so a broken status endpoint does not strand the request.
			return false, nil
		}
		logger.Info("[ModelQueue] status model=%s isQueued=%v serviceAvailable=%v queueCount=%d",
			st.ModelKey, st.IsQueued, st.ServiceAvailable, st.QueueCount)
		if st.ServiceAvailable && !st.IsQueued {
			return true, nil
		}
		qe = &QueueError{
			IsQueued:          st.IsQueued,
			ServiceAvailable:  st.ServiceAvailable,
			QueueType:         st.QueueType,
			QueueCount:        st.QueueCount,
			RetryAfterSeconds: st.RetryAfterSeconds,
			WaitTime:          st.WaitTime,
		}

		wait := time.Duration(qe.RetryAfterSeconds) * time.Second
		if wait <= 0 {
			wait = time.Duration(qe.WaitTime) * time.Second
		}
		if wait <= 0 {
			wait = QueuePollInterval
		}
		// Never sleep past the deadline.
		if remain := time.Until(deadline); remain < wait {
			wait = remain
		}
		logger.Info("[ModelQueue] queued model=%s queueType=%s queueCount=%d retry=%s (requestSet=%s)",
			modelKey, queueType, qe.QueueCount, wait, requestSetID)
		if wait > 0 {
			if err := sleepCtx(ctx, wait); err != nil {
				return false, err
			}
		}
	}
}

// streamWithQueueRecovery wraps streamWithSlot with model-queue waiting. When
// upstream signals the model is queued, it polls the queue and retries the same
// slot until the model frees up or deadline elapses. Empty/fake queue signals
// are returned immediately so the caller can fail over to another account or
// provider instead of hanging for the full wait budget.
func (b *Bridge) streamWithQueueRecovery(ctx context.Context, slot *Slot, qurl string, body map[string]interface{}, extra map[string]string, deadline time.Time, onDelta func(Delta)) (bool, error) {
	emittedAny := false
	queuedOnce := false
	retriedAfterAvailable := false
	startedAt := time.Now()
	for {
		emitted, err := b.streamWithSlot(ctx, slot, qurl, body, extra, onDelta)
		emittedAny = emittedAny || emitted
		if err == nil {
			if queuedOnce {
				b.finishQueueAsync(slot, body, startedAt)
			}
			return emittedAny, nil
		}
		if emitted {
			return emittedAny, err
		}
		qe := parseQueueError(err)
		if qe == nil {
			return emittedAny, err
		}
		requestSetID := StrVal(body, "request_set_id")
		modelKey := ""
		if mc, ok := body["model_config"].(map[string]interface{}); ok {
			modelKey = StrVal(mc, "key")
		}
		if !queuedOnce {
			notifyQueueObserver(ctx, modelKey, requestSetID)
		}
		queuedOnce = true
		if queueShouldFailoverImmediately(qe, retriedAfterAvailable) {
			logger.Info("[ModelQueue] queueCount=%d serviceAvailable=%v -> failover instead of waiting (requestSet=%s)",
				qe.QueueCount, qe.ServiceAvailable, requestSetID)
			return emittedAny, err
		}
		available, werr := b.waitForQueue(ctx, slot, qe, requestSetID, modelKey, deadline)
		if werr != nil {
			return emittedAny, err
		}
		retriedAfterAvailable = available
	}
}

// finishQueueAsync reports queue completion to upstream (best effort).
func (b *Bridge) finishQueueAsync(slot *Slot, body map[string]interface{}, startedAt time.Time) {
	requestSetID := StrVal(body, "request_set_id")
	modelKey := ""
	if mc, ok := body["model_config"].(map[string]interface{}); ok {
		modelKey = StrVal(mc, "key")
	}
	userID := slot.sess.Identity.Uid
	timeConsumed := time.Since(startedAt).Milliseconds()
	go func() {
		defer func() { _ = recover() }()
		if err := b.FinishQueue(slot, requestSetID, modelKey, userID, timeConsumed); err != nil {
			logger.Debug("[ModelQueue] finish failed: %v", err)
		}
	}()
}
