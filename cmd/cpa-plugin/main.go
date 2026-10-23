// Package main implements the Qoder CLIProxyAPI dynamic plugin.
//
// qoder wraps Qoder (qoder.com / qoder.com.cn) as a cliproxy provider. It is a
// thin CPA-native adapter over the qoder2api bridge core: the plugin owns the
// OAuth/PAT login flow, exposes Qoder models, and forwards OpenAI-compatible
// chat completion requests to the Qoder upstream using the existing cosy
// signature/fingerprint implementation.
//
// Accounts are stored as ordinary CPA auth records (one Qoder account per auth
// file), so CPA's built-in scheduler, auth UI, aliasing and usage accounting all
// apply. The plugin only adds: login, account listing/quota, daily check-in and
// the executor itself.
//
// Built with -buildmode=c-shared and exports the cliproxy C ABI entry points.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

static int qd_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}
static void qd_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"qoder2api/account"
	"qoder2api/logger"
)

const (
	providerName = "qoder"
	authFileName = "qoder.json"
	// pluginLogoURL is an inline SVG data URI. The previous value pointed at
	// raw.githubusercontent.com/.../webconsole/favicon.ico, which does not
	// exist (404), so the panel/auth UI showed a broken image.
	pluginLogoURL = "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSI2NCIgaGVpZ2h0PSI2NCIgdmlld0JveD0iMCAwIDY0IDY0Ij48cmVjdCB3aWR0aD0iNjQiIGhlaWdodD0iNjQiIHJ4PSIxNCIgZmlsbD0iIzRmOGNmZiIvPjxwYXRoIGQ9Ik0zMiAxNGM5LjkgMCAxOCA4LjEgMTggMThzLTguMSAxOC0xOCAxOC0xOC04LjEtMTgtMTggOC4xLTE4IDE4LTE4em0wIDhhMTAgMTAgMCAxIDAgMCAyMCAxMCAxMCAwIDAgMCAwLTIweiIgZmlsbD0iI2ZmZiIvPjxyZWN0IHg9IjM4IiB5PSIzOCIgd2lkdGg9IjE0IiBoZWlnaHQ9IjciIHJ4PSIzLjUiIHRyYW5zZm9ybT0icm90YXRlKDQ1IDM4IDM4KSIgZmlsbD0iI2ZmZiIvPjwvc3ZnPg=="
)

// pluginVersion is the release version. Overridable with -ldflags -X.
var pluginVersion = "0.1.1"

// hostAPI is the host function-pointer table captured at init. It is used for
// async host calls (host.stream.emit / host.stream.close / host.auth.*).
var hostAPI *C.cliproxy_host_api

// pluginConfig holds the per-plugin config delivered via plugin.register /
// plugin.reconfigure (plugins.configs.qoder.*).
type pluginConfig struct {
	AutoCheckin    bool
	CheckinTimes   []string
	CheckinOnStart bool
	DefaultRegion  string
	// MaxConcurrentPerAuth caps in-flight requests per Qoder account. The
	// scheduler spreads requests across accounts above this limit.
	MaxConcurrentPerAuth int
}

var (
	configMu     sync.RWMutex
	activeConfig = pluginConfig{
		CheckinTimes:         []string{"10:00"},
		DefaultRegion:        "global",
		MaxConcurrentPerAuth: defaultMaxConcurrentPerAuth,
	}
)

// -----------------------------------------------------------------------------
// C ABI exports
// -----------------------------------------------------------------------------

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// Intentionally a no-op. The host calls this on its own exit path and
	// dlclose()es the library immediately afterwards. Touching Go runtime state
	// here (mutexes, channels) races the c-shared teardown and SIGSEGVs.
}

// -----------------------------------------------------------------------------
// Host bridge
// -----------------------------------------------------------------------------

func hostCall(method string, request []byte) ([]byte, error) {
	if hostAPI == nil || hostAPI.call == nil {
		return nil, fmt.Errorf("host API unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq unsafe.Pointer
	var reqLen C.size_t
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
		reqLen = C.size_t(len(request))
	}
	var resp C.cliproxy_buffer
	rc := C.qd_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil && hostAPI.free_buffer != nil {
		C.qd_free_host_buffer(hostAPI, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, int(rc))
	}
	return out, nil
}

func hostBridgeUnwrap(raw []byte, method string) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: decode envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: host error %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("%s: host returned not-ok", method)
	}
	return env.Result, nil
}

// -----------------------------------------------------------------------------
// Method dispatch
// -----------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		configure(request)
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelStatic:
		return handleModelStatic(request)
	case pluginabi.MethodModelForAuth:
		return handleModelForAuth(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return handleStartLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return handlePollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return handleRefreshAuth(request)
	case pluginabi.MethodSchedulerPick:
		return handleSchedulerPick(request)
	case pluginabi.MethodRequestInterceptBefore:
		return handleRequestInterceptBefore(request)
	case pluginabi.MethodRequestInterceptAfter:
		return handleRequestInterceptAfter(request)
	case pluginabi.MethodRequestComplete:
		return handleRequestComplete(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodExecutorExecute:
		return handleExecExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodManagementRegister:
		var regReq pluginapi.ManagementRegistrationRequest
		if err := json.Unmarshal(request, &regReq); err == nil {
			setManagementBasePath(regReq.BasePath)
		}
		startCheckinScheduler()
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// -----------------------------------------------------------------------------
// Envelopes & registration
// -----------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider             bool                         `json:"model_provider"`
	AuthProvider              bool                         `json:"auth_provider"`
	Scheduler                 bool                         `json:"scheduler"`
	SchedulerAcrossPriorities bool                         `json:"scheduler_across_priorities,omitempty"`
	RequestInterceptor        bool                         `json:"request_interceptor"`
	RequestLifecyclePlugin    bool                         `json:"request_lifecycle_plugin"`
	Executor                  bool                         `json:"executor"`
	ExecutorModelScope        pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats      []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats     []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI             bool                         `json:"management_api"`
	UsagePlugin               bool                         `json:"usage_plugin"`
}

type streamResponse struct {
	Headers map[string][]string             `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Qoder",
			Version:          pluginVersion,
			Author:           "Zhengyuuuui (based on qoder2api)",
			GitHubRepository: "https://github.com/Zhengyuuuui/qoder2api",
			Logo:             pluginLogoURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "checkin_auto", Type: pluginapi.ConfigFieldTypeBoolean, Default: false, Description: "Enable the daily automatic check-in scheduler (default false)."},
				{Name: "checkin_times", Type: pluginapi.ConfigFieldTypeArray, Description: "Daily auto check-in times, HH:MM list (default [\"10:00\"])."},
				{Name: "checkin_on_start", Type: pluginapi.ConfigFieldTypeBoolean, Default: false, Description: "Run a full check-in once when the plugin loads."},
				{Name: "default_region", Type: pluginapi.ConfigFieldTypeEnum, Default: "global", EnumValues: []string{"global", "cn"}, Description: "Region used when a credential does not specify one."},
				{Name: "max_concurrency", Type: pluginapi.ConfigFieldTypeInteger, Default: defaultMaxConcurrentPerAuth, Description: "Maximum concurrent in-flight requests per Qoder account (default 4)."},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:             true,
			AuthProvider:              true,
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			RequestInterceptor:        true,
			RequestLifecyclePlugin:    true,
			Executor:                  true,
			ExecutorModelScope:        pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:      []string{"chat-completions"},
			ExecutorOutputFormats:     []string{"chat-completions"},
			ManagementAPI:             true,
			UsagePlugin:               true,
		},
	}
}

// -----------------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------------

// configure parses the plugin config bag. The host sends the whole
// plugin.register request; the plugin-owned values live under config (or
// configs.qoder) depending on host version.
func configure(raw []byte) {
	initDataDir()
	if len(raw) == 0 {
		return
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return
	}
	cfgRaw, ok := doc["config"]
	if !ok {
		cfgRaw, ok = doc["configs"]
	}
	if !ok {
		return
	}
	// configs may be nested as {"qoder": {...}}.
	var bag map[string]json.RawMessage
	if err := json.Unmarshal(cfgRaw, &bag); err != nil {
		return
	}
	if inner, ok := bag[providerName]; ok {
		cfgRaw = inner
	} else if inner, ok := bag["qoder2api"]; ok {
		cfgRaw = inner
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(cfgRaw, &fields); err != nil {
		return
	}
	getBool := func(key string) (bool, bool) {
		v, ok := fields[key]
		if !ok {
			return false, false
		}
		var b bool
		if json.Unmarshal(v, &b) != nil {
			return false, false
		}
		return b, true
	}
	getStr := func(key string) (string, bool) {
		v, ok := fields[key]
		if !ok {
			return "", false
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			return "", false
		}
		return strings.TrimSpace(s), true
	}
	getStrs := func(key string) ([]string, bool) {
		v, ok := fields[key]
		if !ok {
			return nil, false
		}
		var list []string
		if json.Unmarshal(v, &list) != nil {
			return nil, false
		}
		return list, true
	}
	getInt := func(key string) (int, bool) {
		v, ok := fields[key]
		if !ok {
			return 0, false
		}
		var n int
		if json.Unmarshal(v, &n) != nil {
			return 0, false
		}
		return n, true
	}

	configMu.Lock()
	if b, ok := getBool("checkin_auto"); ok {
		activeConfig.AutoCheckin = b
	}
	if b, ok := getBool("checkin_on_start"); ok {
		activeConfig.CheckinOnStart = b
	}
	if list, ok := getStrs("checkin_times"); ok && len(list) > 0 {
		activeConfig.CheckinTimes = list
	}
	if s, ok := getStr("default_region"); ok && s != "" {
		activeConfig.DefaultRegion = s
	}
	if n, ok := getInt("max_concurrency"); ok && n > 0 {
		activeConfig.MaxConcurrentPerAuth = n
	}
	configMu.Unlock()

	// Values saved from the management panel override the host config so they
	// survive a CPA restart.
	loadPersistedConfig()
	qoderScheduler.setMax(currentConfig().MaxConcurrentPerAuth)

	if currentConfig().CheckinOnStart {
		go runScheduledCheckin()
	}
}

func currentConfig() pluginConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return activeConfig
}

// -----------------------------------------------------------------------------
// Data dir (checkin history + logs + model settings fallback)
// -----------------------------------------------------------------------------

var dataDirOnce sync.Once

// initDataDir points the qoder2api account package at a plugin-owned directory.
// The plugin does not use the account file store for credentials (CPA's auth
// store is authoritative), but bridge.MapModel reads model-mapping settings and
// the check-in history is kept here.
func initDataDir() {
	dataDirOnce.Do(func() {
		dir := strings.TrimSpace(os.Getenv("QODER2API_HOME"))
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				home = "."
			}
			dir = filepath.Join(home, ".qoder2api-cpa")
		}
		_ = os.MkdirAll(dir, 0o700)
		account.SetDataRoot(dir)
		_ = logger.InitFile(filepath.Join(dir, "logs"))
		logger.Info("qoder plugin data dir: %s", dir)
	})
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func okEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func errorEnvelopeStatus(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: httpStatus}})
	return raw
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func main() {}
