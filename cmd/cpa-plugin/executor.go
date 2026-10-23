package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"qoder2api/internal/bridge"
	"qoder2api/internal/cosy"
)

//go:embed baseprompt.json
var basePromptRaw []byte

var (
	templateOnce sync.Once
	templateData map[string]interface{}
)

// qoderTemplate returns the upstream request template with UUID/time
// placeholders substituted once per process.
func qoderTemplate() map[string]interface{} {
	templateOnce.Do(func() {
		tmpl := string(basePromptRaw)
		for _, ukey := range []string{"{UUID1}", "{UUID2}", "{UUID3}", "{UUID4}", "{UUID5}"} {
			tmpl = strings.ReplaceAll(tmpl, ukey, cosy.NewUUID())
		}
		tmpl = strings.ReplaceAll(tmpl, "{TIME1}", fmt.Sprintf("%d", cosy.UnixMs()))
		var m map[string]interface{}
		if json.Unmarshal([]byte(tmpl), &m) != nil {
			m = map[string]interface{}{}
		}
		templateData = m
	})
	return templateData
}

func templateMessages() []interface{} {
	if msgs, ok := qoderTemplate()["messages"].([]interface{}); ok {
		return msgs
	}
	return nil
}

// --- bridge session cache ---------------------------------------------------
// Building a bridge for a PAT performs a jobToken exchange; for OAuth it calls
// userinfo. Cache per auth id so each account pays that cost once.

type cachedBridge struct {
	b      *bridge.Bridge
	secret string
	fpKey  string
}

var (
	bridgeMu    sync.Mutex
	bridgeCache = map[string]*cachedBridge{}
)

func bridgeForAuth(sa *storedAuth) (*bridge.Bridge, error) {
	key := authIDFor(sa) + "|" + string(sa.region())
	fp := sa.Fingerprint.toAccountFingerprint()
	fpKey := fingerprintCacheKey(fp)
	bridgeMu.Lock()
	if c, ok := bridgeCache[key]; ok && c.secret == sa.Secret && c.fpKey == fpKey {
		bridgeMu.Unlock()
		return c.b, nil
	}
	bridgeMu.Unlock()

	// A real device fingerprint switches this account to the modern inference
	// protocol, which is what makes model/list return Sonus/Cantus/Fast and
	// lets those models pass the upstream device check. Accounts without one
	// keep the legacy protocol unchanged.
	b, err := bridge.NewBridgeWithFP(sa.Secret, sa.region(), qoderTemplate(), fp)
	if err != nil {
		return nil, err
	}
	bridgeMu.Lock()
	bridgeCache[key] = &cachedBridge{b: b, secret: sa.Secret, fpKey: fpKey}
	bridgeMu.Unlock()
	return b, nil
}

// --- request parsing --------------------------------------------------------

type qoderRequest struct {
	body        map[string]interface{}
	model       string // upstream model key passed to Qoder
	clientModel string // client-requested name, echoed back in responses
	agent       string
	messages    []interface{}
	tools       interface{}
	toolsOn     bool
	opts        bridge.CallOpts
	stream      bool
}

// buildQoderRequest decodes the translated client payload. modelOverride is the
// host-resolved upstream model (pluginapi.ExecutorRequest.Model): CPA resolves
// oauth-model-alias entries before calling the executor, so req.Model is the
// real Qoder key (e.g. "auto") while the payload still carries the client name
// (e.g. "qoder-auto"). Prefer the override so aliases do not leak upstream.
func buildQoderRequest(raw []byte, modelOverride string, wantStream bool) (*qoderRequest, error) {
	body := raw
	var oreq map[string]interface{}
	if err := json.Unmarshal(body, &oreq); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	clientModel := bridge.StrValDefault(oreq, "model", "auto")
	requestedModel := firstNonEmpty(strings.TrimSpace(modelOverride), clientModel)
	model := requestedModel
	agent := bridge.InferAgent(clientModel)
	skipModelMap := false
	// Plugin-level model aliases are authoritative: resolve them directly to the
	// upstream model.key and skip bridge.MapModel so user/default mappings cannot
	// rewrite an explicit alias. If the host already resolved the alias to the
	// upstream key, still infer the agent from the upstream key.
	if upstream, ok := resolveModelAlias(requestedModel); ok {
		model = upstream
		agent = bridge.InferAgent(upstream)
		skipModelMap = true
	} else if upstream, ok := resolveModelAlias(clientModel); ok {
		override := strings.TrimSpace(modelOverride)
		if override == "" || strings.EqualFold(override, upstream) || strings.EqualFold(override, clientModel) {
			model = upstream
			agent = bridge.InferAgent(upstream)
			skipModelMap = true
		}
	}
	incoming, _ := oreq["messages"].([]interface{})
	tools := oreq["tools"]
	toolsOn := tools != nil

	opts := bridge.CallOpts{SkipModelMap: skipModelMap}
	if mt, ok := oreq["max_tokens"].(float64); ok && int(mt) > 0 {
		opts.MaxTokens = int(mt)
	}
	// OpenAI 标准 reasoning_effort（low/medium/high/...）与 Claude 风格的
	// thinking.budget_tokens 都透传给上游的思考强度参数。
	if re, ok := oreq["reasoning_effort"].(string); ok {
		opts.ReasoningEffort = strings.TrimSpace(re)
	}
	if thinking, ok := oreq["thinking"].(map[string]interface{}); ok {
		if tp, _ := thinking["type"].(string); tp == "enabled" {
			opts.IsReasoning = true
		}
		if bt, ok := thinking["budget_tokens"].(float64); ok && int(bt) > 0 {
			opts.ThinkingBudget = int(bt)
		}
	}

	prompt := bridge.ExtractLatestUserPrompt(incoming)
	messages := bridge.BuildQoderMessages(templateMessages(), incoming, prompt, toolsOn)

	return &qoderRequest{
		body:        oreq,
		model:       model,
		clientModel: clientModel,
		agent:       agent,
		messages:    messages,
		tools:       tools,
		toolsOn:     toolsOn,
		opts:        opts,
		stream:      wantStream,
	}, nil
}

// --- non-stream execute -----------------------------------------------------

// execTimeout bounds a single upstream generation so a stalled connection
// cannot pin a goroutine forever. The host does not pass a cancelable context
// through the plugin RPC, so this is the only backstop.
const execTimeout = 30 * time.Minute

func newExecContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), execTimeout)
}

// executorSessionKey extracts the same session identity the scheduler uses.
// When the client does not provide one, a unique key keeps queue observations
// paired with the completion callback instead of leaking into the TTL bucket.
func executorSessionKey(req pluginapi.ExecutorRequest) string {
	key := schedulerSessionKey(pluginapi.SchedulerOptions{
		Headers:  req.Headers,
		Metadata: req.Metadata,
	})
	if key != "" {
		return key
	}
	return "request:" + cosy.NewUUID()
}

func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	b, err := bridgeForAuth(sa)
	if err != nil {
		return nil, err
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	qr, err := buildQoderRequest(body, req.Model, false)
	if err != nil {
		return nil, err
	}
	if err := ensureModelEnabled(qr); err != nil {
		return errorEnvelopeStatus("model_not_found", err.Error(), 404), nil
	}

	var full strings.Builder
	var reasoning strings.Builder
	var toolCalls []interface{}
	var inTok, outTok, cachedTok int
	sessionKey := executorSessionKey(req)
	ctx, cancel := newExecContext()
	ctx = bridge.WithQueueContext(ctx, sessionKey, qr.clientModel)
	defer cancel()
	err = b.CallQoderWithOpts(ctx, qr.agent, qr.messages, qr.model, qr.tools, qr.opts, func(d bridge.Delta) {
		if d.Err != nil {
			return
		}
		if d.InputTokens > 0 || d.OutputTokens > 0 || d.CachedTokens > 0 {
			inTok, outTok, cachedTok = d.InputTokens, d.OutputTokens, d.CachedTokens
		}
		if d.Content != "" {
			full.WriteString(d.Content)
		}
		if d.Reasoning != "" {
			reasoning.WriteString(d.Reasoning)
		}
		if d.ToolCalls != nil {
			toolCalls = append(toolCalls, d.ToolCalls...)
		}
	})
	qoderQueueBreaker.recordDone([]string{qr.model, qr.clientModel}, sessionKey, err == nil)
	if err != nil {
		msg, et := bridge.FriendlyError(err)
		if et == "" {
			et = "qoder_error"
		}
		return errorEnvelopeStatus(et, msg, bridge.ErrorStatus(err)), nil
	}

	reqID := "chatcmpl-" + cosy.NewRequestID()
	created := cosy.UnixSec()
	finish := "stop"
	message := map[string]interface{}{"role": "assistant", "content": full.String()}
	if reasoning.Len() > 0 {
		// OpenAI-compatible reasoning passthrough (DeepSeek/Qwen convention).
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		finish = "tool_calls"
		message["tool_calls"] = toolCalls
		if full.Len() == 0 {
			message["content"] = nil
		}
	}
	resp := map[string]interface{}{
		"id": reqID, "object": "chat.completion",
		"created": created, "model": qr.clientModel,
		"choices": []interface{}{
			map[string]interface{}{"index": 0, "message": message, "finish_reason": finish},
		},
		"usage": map[string]interface{}{
			"prompt_tokens": inTok, "completion_tokens": outTok, "total_tokens": inTok + outTok,
			"prompt_tokens_details": map[string]interface{}{"cached_tokens": cachedTok},
		},
	}
	payload, _ := json.Marshal(resp)
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload})
}

// --- stream execute ---------------------------------------------------------

type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	b, err := bridgeForAuth(sa)
	if err != nil {
		return nil, err
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	qr, err := buildQoderRequest(body, req.Model, true)
	if err != nil {
		return nil, err
	}
	if err := ensureModelEnabled(qr); err != nil {
		return errorEnvelopeStatus("model_not_found", err.Error(), 404), nil
	}
	frame := clientNeedsSSEFrame(req.Metadata)
	headers := streamHeaders()
	sessionKey := executorSessionKey(req.ExecutorRequest)

	// Async: preflight the first upstream event before opening the host stream.
	// If Qoder answers with a queue/resource-limit error before any content,
	// return an executor error so the host can retry another auth/provider in
	// the same request. Without this, the host has already sent HTTP 200 and
	// the client sees the upstream 403 as a stream error.
	if req.StreamID != "" {
		ctx, cancel := newExecContext()
		ctx = bridge.WithQueueContext(ctx, sessionKey, qr.clientModel)
		preflight := newStreamPreflight()
		go pumpStream(ctx, cancel, req.StreamID, b, qr, frame, sessionKey, preflight)
		select {
		case <-preflight.ready:
			preflight.releaseNow()
			return okEnvelope(streamResponse{Headers: headers})
		case err := <-preflight.failed:
			cancel()
			msg, et := bridge.FriendlyError(err)
			if et == "" {
				et = "qoder_error"
			}
			return errorEnvelopeStatus(et, msg, bridge.ErrorStatus(err)), nil
		case <-time.After(10 * time.Second):
			// No first event yet: open the stream and let it continue. A later
			// error is emitted as a normal stream error.
			preflight.abandon()
			return okEnvelope(streamResponse{Headers: headers})
		}
	}

	// Sync: collect all chunks and return them in one response.
	ctx, cancel := newExecContext()
	ctx = bridge.WithQueueContext(ctx, sessionKey, qr.clientModel)
	defer cancel()
	chunks, err := collectStream(ctx, b, qr, frame)
	qoderQueueBreaker.recordDone([]string{qr.model, qr.clientModel}, sessionKey, err == nil)
	if err != nil {
		msg, et := bridge.FriendlyError(err)
		if et == "" {
			et = "qoder_error"
		}
		return errorEnvelopeStatus(et, msg, bridge.ErrorStatus(err)), nil
	}
	return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
}

// collectStream runs one streaming request and returns the framed chunks.
func collectStream(ctx context.Context, b *bridge.Bridge, qr *qoderRequest, frame bool) ([]pluginapi.ExecutorStreamChunk, error) {
	reqID := "chatcmpl-" + cosy.NewRequestID()
	created := cosy.UnixSec()
	var chunks []pluginapi.ExecutorStreamChunk
	var toolCallBuf []interface{}
	var inTok, outTok, cachedTok int

	err := b.CallQoderWithOpts(ctx, qr.agent, qr.messages, qr.model, qr.tools, qr.opts, func(d bridge.Delta) {
		if d.Err != nil {
			return
		}
		if d.InputTokens > 0 || d.OutputTokens > 0 || d.CachedTokens > 0 {
			inTok, outTok, cachedTok = d.InputTokens, d.OutputTokens, d.CachedTokens
		}
		chunk := bridge.MakeChatChunk(reqID, created, qr.clientModel)
		choices := chunk["choices"].([]interface{})
		delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
		if d.Content != "" {
			delta["role"] = "assistant"
			delta["content"] = d.Content
		}
		if d.Reasoning != "" {
			delta["role"] = "assistant"
			delta["reasoning_content"] = d.Reasoning
		}
		if d.ToolCalls != nil {
			delta["tool_calls"] = d.ToolCalls
			toolCallBuf = append(toolCallBuf, d.ToolCalls...)
		}
		if data, err := json.Marshal(chunk); err == nil {
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: frameChunk(data, frame)})
		}
	})
	if err != nil {
		return chunks, err
	}

	finish := "stop"
	if len(toolCallBuf) > 0 {
		finish = "tool_calls"
	}
	done := bridge.MakeChatChunk(reqID, created, qr.clientModel)
	ch := done["choices"].([]interface{})[0].(map[string]interface{})
	ch["finish_reason"] = finish
	ch["delta"] = map[string]interface{}{}
	if inTok > 0 || outTok > 0 || cachedTok > 0 {
		done["usage"] = map[string]interface{}{
			"prompt_tokens": inTok, "completion_tokens": outTok, "total_tokens": inTok + outTok,
			"prompt_tokens_details": map[string]interface{}{"cached_tokens": cachedTok},
		}
	}
	if data, err := json.Marshal(done); err == nil {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: frameChunk(data, frame)})
	}
	return chunks, nil
}

// streamPreflight gates the async stream until the first upstream event is
// known. The handler waits on ready/failed; the pump waits on release before
// emitting the first chunk.
type streamPreflight struct {
	mu          sync.Mutex
	decided     bool
	ready       chan struct{}
	failed      chan error
	release     chan struct{}
	releaseOnce sync.Once
}

func newStreamPreflight() *streamPreflight {
	return &streamPreflight{
		ready:   make(chan struct{}),
		failed:  make(chan error, 1),
		release: make(chan struct{}),
	}
}

func (p *streamPreflight) markReady() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	if p.decided {
		p.mu.Unlock()
		return false
	}
	p.decided = true
	p.mu.Unlock()
	close(p.ready)
	return true
}

func (p *streamPreflight) fail(err error) bool {
	if p == nil || err == nil {
		return false
	}
	p.mu.Lock()
	if p.decided {
		p.mu.Unlock()
		return false
	}
	p.decided = true
	p.mu.Unlock()
	p.failed <- err
	return true
}

func (p *streamPreflight) abandon() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.decided = true
	p.mu.Unlock()
	p.releaseNow()
}

func (p *streamPreflight) releaseNow() {
	if p == nil {
		return
	}
	p.releaseOnce.Do(func() { close(p.release) })
}

// pumpStream emits chunks to the host as they arrive, then closes the stream.
func pumpStream(ctx context.Context, cancel context.CancelFunc, streamID string, b *bridge.Bridge, qr *qoderRequest, frame bool, sessionKey string, preflight *streamPreflight) {
	defer cancel()
	closeStream := true
	defer func() {
		if closeStream {
			streamClose(streamID)
		}
	}()

	reqID := "chatcmpl-" + cosy.NewRequestID()
	created := cosy.UnixSec()
	var toolCallBuf []interface{}
	var inTok, outTok, cachedTok int
	emitFailed := false
	preflightStarted := false

	err := b.CallQoderWithOpts(ctx, qr.agent, qr.messages, qr.model, qr.tools, qr.opts, func(d bridge.Delta) {
		if emitFailed || d.Err != nil {
			return
		}
		if d.InputTokens > 0 || d.OutputTokens > 0 || d.CachedTokens > 0 {
			inTok, outTok, cachedTok = d.InputTokens, d.OutputTokens, d.CachedTokens
		}
		if preflight != nil && !preflightStarted && (d.Content != "" || d.Reasoning != "" || d.ToolCalls != nil) {
			preflightStarted = true
			if preflight.markReady() {
				select {
				case <-preflight.release:
				case <-ctx.Done():
					emitFailed = true
					return
				}
			}
		}
		chunk := bridge.MakeChatChunk(reqID, created, qr.clientModel)
		choices := chunk["choices"].([]interface{})
		delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
		if d.Content != "" {
			delta["role"] = "assistant"
			delta["content"] = d.Content
		}
		if d.Reasoning != "" {
			delta["role"] = "assistant"
			delta["reasoning_content"] = d.Reasoning
		}
		if d.ToolCalls != nil {
			delta["tool_calls"] = d.ToolCalls
			toolCallBuf = append(toolCallBuf, d.ToolCalls...)
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			return
		}
		if err := streamEmit(streamID, frameChunk(data, frame)); err != nil {
			emitFailed = true
			cancel()
		}
	})
	qoderQueueBreaker.recordDone([]string{qr.model, qr.clientModel}, sessionKey, err == nil && !emitFailed)
	if err != nil {
		if preflight != nil && !preflightStarted {
			if preflight.fail(err) {
				closeStream = false
				return
			}
		}
		if !emitFailed {
			msg, _ := bridge.FriendlyError(err)
			streamEmitError(streamID, msg)
		}
		return
	}
	if emitFailed {
		return
	}
	if preflight != nil && !preflightStarted {
		preflightStarted = true
		if preflight.markReady() {
			select {
			case <-preflight.release:
			case <-ctx.Done():
				return
			}
		}
	}

	finish := "stop"
	if len(toolCallBuf) > 0 {
		finish = "tool_calls"
	}
	done := bridge.MakeChatChunk(reqID, created, qr.clientModel)
	ch := done["choices"].([]interface{})[0].(map[string]interface{})
	ch["finish_reason"] = finish
	ch["delta"] = map[string]interface{}{}
	if inTok > 0 || outTok > 0 || cachedTok > 0 {
		done["usage"] = map[string]interface{}{
			"prompt_tokens": inTok, "completion_tokens": outTok, "total_tokens": inTok + outTok,
			"prompt_tokens_details": map[string]interface{}{"cached_tokens": cachedTok},
		}
	}
	if data, err := json.Marshal(done); err == nil {
		_ = streamEmit(streamID, frameChunk(data, frame))
	}
}

// frameChunk optionally prefixes a "data: " SSE frame. CPA's native
// chat-completions writer adds the prefix itself; every cross-format response
// translator consumes already-framed payloads.
func frameChunk(payload []byte, frame bool) []byte {
	if !frame {
		return payload
	}
	out := make([]byte, 0, len(payload)+6)
	out = append(out, []byte("data: ")...)
	out = append(out, payload...)
	return out
}

func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// --- host stream helpers ----------------------------------------------------

func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	errJSON, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
	_ = streamEmit(streamID, errJSON)
}

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID})
	_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}
