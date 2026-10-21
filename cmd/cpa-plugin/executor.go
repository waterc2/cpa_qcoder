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
}

var (
	bridgeMu    sync.Mutex
	bridgeCache = map[string]*cachedBridge{}
)

func bridgeForAuth(sa *storedAuth) (*bridge.Bridge, error) {
	key := authIDFor(sa)
	bridgeMu.Lock()
	if c, ok := bridgeCache[key]; ok && c.secret == sa.Secret {
		bridgeMu.Unlock()
		return c.b, nil
	}
	bridgeMu.Unlock()

	b, err := bridge.NewBridge(sa.Secret, sa.region(), qoderTemplate())
	if err != nil {
		return nil, err
	}
	bridgeMu.Lock()
	bridgeCache[key] = &cachedBridge{b: b, secret: sa.Secret}
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
	model := firstNonEmpty(strings.TrimSpace(modelOverride), clientModel)
	incoming, _ := oreq["messages"].([]interface{})
	tools := oreq["tools"]
	toolsOn := tools != nil

	opts := bridge.CallOpts{}
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
		agent:       bridge.InferAgent(clientModel),
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
	var inTok, outTok int
	ctx, cancel := newExecContext()
	defer cancel()
	err = b.CallQoderWithOpts(ctx, qr.agent, qr.messages, qr.model, qr.tools, qr.opts, func(d bridge.Delta) {
		if d.Err != nil {
			return
		}
		if d.InputTokens > 0 || d.OutputTokens > 0 {
			inTok, outTok = d.InputTokens, d.OutputTokens
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

	// Async: emit chunks through the host stream and return immediately.
	if req.StreamID != "" {
		ctx, cancel := newExecContext()
		go pumpStream(ctx, cancel, req.StreamID, b, qr, frame)
		return okEnvelope(streamResponse{Headers: headers})
	}

	// Sync: collect all chunks and return them in one response.
	ctx, cancel := newExecContext()
	defer cancel()
	chunks, err := collectStream(ctx, b, qr, frame)
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
	var inTok, outTok int

	err := b.CallQoderWithOpts(ctx, qr.agent, qr.messages, qr.model, qr.tools, qr.opts, func(d bridge.Delta) {
		if d.Err != nil {
			return
		}
		if d.InputTokens > 0 || d.OutputTokens > 0 {
			inTok, outTok = d.InputTokens, d.OutputTokens
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
	if inTok > 0 || outTok > 0 {
		done["usage"] = map[string]interface{}{
			"prompt_tokens": inTok, "completion_tokens": outTok, "total_tokens": inTok + outTok,
		}
	}
	if data, err := json.Marshal(done); err == nil {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: frameChunk(data, frame)})
	}
	return chunks, nil
}

// pumpStream emits chunks to the host as they arrive, then closes the stream.
func pumpStream(ctx context.Context, cancel context.CancelFunc, streamID string, b *bridge.Bridge, qr *qoderRequest, frame bool) {
	defer cancel()
	defer streamClose(streamID)

	reqID := "chatcmpl-" + cosy.NewRequestID()
	created := cosy.UnixSec()
	var toolCallBuf []interface{}
	var inTok, outTok int
	emitFailed := false

	err := b.CallQoderWithOpts(ctx, qr.agent, qr.messages, qr.model, qr.tools, qr.opts, func(d bridge.Delta) {
		if emitFailed || d.Err != nil {
			return
		}
		if d.InputTokens > 0 || d.OutputTokens > 0 {
			inTok, outTok = d.InputTokens, d.OutputTokens
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
	if err != nil {
		if !emitFailed {
			msg, _ := bridge.FriendlyError(err)
			streamEmitError(streamID, msg)
		}
		return
	}
	if emitFailed {
		return
	}

	finish := "stop"
	if len(toolCallBuf) > 0 {
		finish = "tool_calls"
	}
	done := bridge.MakeChatChunk(reqID, created, qr.clientModel)
	ch := done["choices"].([]interface{})[0].(map[string]interface{})
	ch["finish_reason"] = finish
	ch["delta"] = map[string]interface{}{}
	if inTok > 0 || outTok > 0 {
		done["usage"] = map[string]interface{}{
			"prompt_tokens": inTok, "completion_tokens": outTok, "total_tokens": inTok + outTok,
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
