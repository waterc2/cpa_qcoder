package bridge

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseQueueErrorNested(t *testing.T) {
	// Real upstream 403 shape: the queue signal is JSON encoded inside a
	// message string inside another message string.
	inner := map[string]interface{}{
		"isQueued": true, "modelKey": "performance", "queueCount": 0,
		"queueType": "p3", "retryAfterSeconds": 30, "serviceAvailable": false,
		"subCode": "RESOURCE_LIMIT", "waitTime": 30,
	}
	innerJSON, _ := json.Marshal(inner)
	mid := map[string]interface{}{"code": "10605", "message": string(innerJSON), "subCode": "RESOURCE_LIMIT"}
	midJSON, _ := json.Marshal(mid)
	outer := map[string]interface{}{"code": "403", "message": string(midJSON)}
	outerJSON, _ := json.Marshal(outer)

	ue := NewUpstreamError(403, string(outerJSON))
	qe := parseQueueError(ue)
	if qe == nil {
		t.Fatalf("expected queue error, got nil (detail=%s)", ue.Detail)
	}
	if !qe.IsQueued || qe.ModelKey != "performance" || qe.QueueType != "p3" ||
		qe.RetryAfterSeconds != 30 || qe.ServiceAvailable {
		t.Fatalf("unexpected queue error: %+v", qe)
	}
}

func TestParseQueueErrorNegative(t *testing.T) {
	if qe := parseQueueError(NewUpstreamError(500, `{"error":"boom"}`)); qe != nil {
		t.Fatalf("non-queue error parsed as queue: %+v", qe)
	}
	if qe := parseQueueError(ErrEmptyStream); qe != nil {
		t.Fatalf("plain error parsed as queue: %+v", qe)
	}
}

func TestParseQueueErrorAvailable(t *testing.T) {
	detail := `{"isQueued":false,"modelKey":"auto","queueCount":0,"queueType":"p3","retryAfterSeconds":2,"serviceAvailable":true,"waitTime":0}`
	qe := parseQueueError(NewUpstreamError(403, detail))
	if qe == nil {
		t.Fatal("expected queue signal")
	}
	if qe.IsQueued || !qe.ServiceAvailable || qe.RetryAfterSeconds != 2 {
		t.Fatalf("unexpected: %+v", qe)
	}
}

func TestEnvQueueDuration(t *testing.T) {
	const primary, msEnv = "QODER2API_QUEUE_MAX_WAIT", "QODER_MODEL_QUEUE_MAX_WAIT_MS"
	def := 3 * time.Minute

	t.Setenv(primary, "")
	t.Setenv(msEnv, "")
	if got := envQueueDuration(primary, msEnv, def); got != def {
		t.Fatalf("default = %s, want %s", got, def)
	}

	t.Setenv(primary, "90s")
	if got := envQueueDuration(primary, msEnv, def); got != 90*time.Second {
		t.Fatalf("duration = %s, want 90s", got)
	}

	t.Setenv(primary, "")
	t.Setenv(msEnv, "45000")
	if got := envQueueDuration(primary, msEnv, def); got != 45*time.Second {
		t.Fatalf("ms = %s, want 45s", got)
	}

	t.Setenv(msEnv, "bogus")
	if got := envQueueDuration(primary, msEnv, def); got != def {
		t.Fatalf("bogus ms fallback = %s, want %s", got, def)
	}
}

func TestQueueShouldFailoverImmediately(t *testing.T) {
	tests := []struct {
		name                  string
		qe                    *QueueError
		retriedAfterAvailable bool
		want                  bool
	}{
		{name: "nil", qe: nil, want: false},
		{name: "empty fake queue", qe: &QueueError{QueueCount: 0, ServiceAvailable: false}, want: true},
		{name: "real queue", qe: &QueueError{QueueCount: 3, ServiceAvailable: false}, want: false},
		{name: "available", qe: &QueueError{QueueCount: 0, ServiceAvailable: true}, want: false},
		{name: "queued again after available", qe: &QueueError{QueueCount: 3, ServiceAvailable: false}, retriedAfterAvailable: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := queueShouldFailoverImmediately(tt.qe, tt.retriedAfterAvailable); got != tt.want {
				t.Fatalf("queueShouldFailoverImmediately() = %v, want %v", got, tt.want)
			}
		})
	}
}
