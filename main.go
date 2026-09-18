package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"gopkg.in/yaml.v3"
)

const pluginName = "cpa-devin-compat"
const pluginVersion = "0.2.0"

type config struct {
	Enabled bool     `yaml:"enabled"`
	Models  []string `yaml:"models"`
	// SessionPin 为没有显式会话标识的请求补一个按对话稳定的会话头。
	// Devin 的 prompt cache 以 cascade_id（即会话 ID）为键，会话 ID 一变缓存就全部失效。
	SessionPin    bool   `yaml:"session-pin"`
	SessionHeader string `yaml:"session-header"`
	// FlattenNamespaceTools 把 Responses 请求里的 namespace 工具展开成普通 function 工具，
	// 否则 devin 执行器会发出无名工具，上游返回 invalid_argument。
	FlattenNamespaceTools bool `yaml:"flatten-namespace-tools"`
	// SanitizeToolSchemas 内联 function parameters 中的本地 JSON Schema 引用，
	// 避免 Devin 对复杂 $defs/$ref 图返回 invalid_argument。
	SanitizeToolSchemas bool `yaml:"sanitize-tool-schemas"`
	// FixResponses 补齐 Interactions→Responses 转换缺失的必填字段（AI SDK 会做严格校验）。
	FixResponses bool `yaml:"fix-responses"`
	// NormalizeChatToolIndex 把 chat 流里按步骤编号的 tool_calls.index 改成从 0 连续编号。
	NormalizeChatToolIndex bool `yaml:"normalize-chat-tool-index"`
	Log                    bool `yaml:"log"`
}

func defaultConfig() config {
	return config{
		Enabled:                true,
		Models:                 []string{"devin/*"},
		SessionPin:             true,
		SessionHeader:          "X-Session-Id",
		FlattenNamespaceTools:  true,
		SanitizeToolSchemas:    true,
		FixResponses:           true,
		NormalizeChatToolIndex: true,
		Log:                    true,
	}
}

var settings atomic.Pointer[config]

func init() {
	c := defaultConfig()
	settings.Store(&c)
}

func main() { fmt.Println(pluginName, pluginVersion) }

func logf(format string, args ...any) {
	if c := settings.Load(); c != nil && c.Log {
		fmt.Fprintf(os.Stderr, "["+pluginName+"] "+format+"\n", args...)
	}
}

// matchModel 判断请求模型是否命中配置的通配符（path.Match 语义，"*" 不跨越 "/"）。
func matchModel(c *config, models ...string) bool {
	for _, model := range models {
		if model == "" {
			continue
		}
		for _, p := range c.Models {
			if p == "" {
				continue
			}
			if ok, err := path.Match(p, model); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// handleMethod 是 C ABI 的入口。插件以动态库形式跑在 CPA 进程里，这里的 panic 会直接拖垮宿主，
// 所以统一兜底：拦截类调用出错时返回空结果（宿主按"不修改"处理），注册类调用返回错误。
func handleMethod(method string, raw []byte) (out []byte, err error) {
	return withRecover(method, func() ([]byte, error) { return dispatch(method, raw) })
}

func withRecover(method string, fn func() ([]byte, error)) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[%s] recovered panic method=%s: %v\n", pluginName, method, r)
			if method == pluginabi.MethodPluginRegister || method == pluginabi.MethodPluginReconfigure {
				out, err = nil, fmt.Errorf("plugin panic during %s", method)
				return
			}
			out, err = okEnvelope(struct{}{})
		}
	}()
	return fn()
}

func dispatch(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return register(raw)
	case pluginabi.MethodRequestInterceptBefore:
		var req pluginapi.RequestInterceptRequest
		if json.Unmarshal(raw, &req) != nil {
			return okEnvelope(pluginapi.RequestInterceptResponse{})
		}
		return okEnvelope(interceptRequest(settings.Load(), req))
	case pluginabi.MethodResponseInterceptAfter:
		var req pluginapi.ResponseInterceptRequest
		if json.Unmarshal(raw, &req) != nil {
			return okEnvelope(pluginapi.ResponseInterceptResponse{})
		}
		return okEnvelope(interceptResponse(settings.Load(), req))
	case pluginabi.MethodResponseInterceptStreamChunk:
		var req pluginapi.StreamChunkInterceptRequest
		if json.Unmarshal(raw, &req) != nil {
			return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
		}
		return okEnvelope(interceptStreamChunk(settings.Load(), req))
	case pluginabi.MethodRequestInterceptAfter, pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})
	default:
		return errorEnvelope("unknown_method", "unsupported method"), nil
	}
}

func register(raw []byte) ([]byte, error) {
	var request struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fmt.Errorf("invalid lifecycle request")
		}
	}
	// yaml.Unmarshal 只覆盖 YAML 中出现的字段，没写的保留默认值。
	c := defaultConfig()
	if err := yaml.Unmarshal(request.ConfigYAML, &c); err != nil {
		return nil, fmt.Errorf("invalid plugin configuration")
	}
	if strings.TrimSpace(c.SessionHeader) == "" {
		c.SessionHeader = defaultConfig().SessionHeader
	}
	settings.Store(&c)
	if request.SchemaVersion != 0 && request.SchemaVersion != pluginabi.SchemaVersion {
		logf("host schema_version=%d 与编译时 %d 不同：插件协议可能已变化，请确认日志中修补仍按预期工作", request.SchemaVersion, pluginabi.SchemaVersion)
	}
	return okEnvelope(struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]bool    `json:"capabilities"`
	}{pluginabi.SchemaVersion, pluginapi.Metadata{
		Name: pluginName, Version: pluginVersion, Author: "Scottio",
		GitHubRepository: "https://github.com/szxypi/cpa-devin-compat",
		ConfigFields: []pluginapi.ConfigField{
			{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "是否启用本插件。"},
			{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "生效的模型通配符列表（path.Match 语义，\"*\" 不跨越 \"/\"），默认 devin/*。"},
			{Name: "session-pin", Type: pluginapi.ConfigFieldTypeBoolean, Description: "请求没有显式会话标识时，按系统提示词与首条用户消息补一个稳定会话头，让 Devin 的 prompt cache 跨轮命中。"},
			{Name: "session-header", Type: pluginapi.ConfigFieldTypeString, Description: "补写的会话头名称，默认 X-Session-Id。"},
			{Name: "flatten-namespace-tools", Type: pluginapi.ConfigFieldTypeBoolean, Description: "把顶层 tools 与 Codex additional_tools 里的 namespace 工具展开，保留 function/custom 子工具，避免 Devin MCP configuration issue。"},
			{Name: "sanitize-tool-schemas", Type: pluginapi.ConfigFieldTypeBoolean, Description: "内联 Responses function parameters 中的本地 $defs/$ref，避免 Devin 对复杂引用图返回 invalid_argument。"},
			{Name: "fix-responses", Type: pluginapi.ConfigFieldTypeBoolean, Description: "补齐 Responses 流式与非流式输出缺失的必填字段（created_at、item_id、summary_index、status、输出条目 id）。"},
			{Name: "normalize-chat-tool-index", Type: pluginapi.ConfigFieldTypeBoolean, Description: "把 chat/completions 流里的 tool_calls.index 规范为从 0 开始连续编号。"},
			{Name: "log", Type: pluginapi.ConfigFieldTypeBoolean, Description: "是否把会话补写、工具展开、字段修补记录到日志。"},
		},
	}, map[string]bool{
		"request_interceptor":         true,
		"response_interceptor":        true,
		"response_stream_interceptor": true,
	}})
}

func interceptRequest(c *config, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	var resp pluginapi.RequestInterceptResponse
	if c == nil || !c.Enabled || !matchModel(c, req.RequestedModel, req.Model) {
		return resp
	}
	body := req.Body
	if c.FlattenNamespaceTools && mayCarryResponses(req.SourceFormat) {
		if out, namespaces, tools := flattenNamespaceTools(body); namespaces > 0 {
			body = out
			resp.Body = out
			logf("namespace-flatten model=%s namespaces=%d tools=%d", firstNonEmpty(req.RequestedModel, req.Model), namespaces, tools)
		}
	}
	if c.SanitizeToolSchemas && mayCarryResponses(req.SourceFormat) {
		if out, schemas, refs := sanitizeResponsesToolSchemas(body); schemas > 0 {
			body = out
			resp.Body = out
			logf("schema-inline model=%s schemas=%d refs=%d", firstNonEmpty(req.RequestedModel, req.Model), schemas, refs)
		}
	}
	if c.SessionPin && req.Headers.Get(c.SessionHeader) == "" {
		if id := pinnedSessionID(req.Headers, body, req.Metadata, req.SourceFormat); id != "" {
			resp.Headers = http.Header{}
			resp.Headers.Set(c.SessionHeader, id)
			logf("session-pin source=%s model=%s header=%s id=%s", req.SourceFormat, firstNonEmpty(req.RequestedModel, req.Model), c.SessionHeader, shortID(id))
		}
	}
	return resp
}

func interceptResponse(c *config, req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	var resp pluginapi.ResponseInterceptResponse
	if c == nil || !c.Enabled || !c.FixResponses || req.Stream || !matchModel(c, req.RequestedModel, req.Model) {
		return resp
	}
	if req.StatusCode != 0 && (req.StatusCode < 200 || req.StatusCode >= 300) {
		return resp
	}
	if !isResponsesPayload(req.SourceFormat, req.Body) {
		return resp
	}
	if out, fixes := patchResponsesObject(req.Body); fixes > 0 {
		resp.Body = out
		logf("responses-fix nonstream model=%s fixes=%d", firstNonEmpty(req.RequestedModel, req.Model), fixes)
	}
	return resp
}

func interceptStreamChunk(c *config, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	var resp pluginapi.StreamChunkInterceptResponse
	if c == nil || !c.Enabled || req.ChunkIndex < 0 || len(req.Body) == 0 || !matchModel(c, req.RequestedModel, req.Model) {
		return resp
	}
	switch {
	case c.FixResponses && isResponsesPayload(req.SourceFormat, req.Body):
		if out, changed := patchResponsesChunk(req.RequestID, req.Body); changed {
			resp.Body = out
		}
	case c.NormalizeChatToolIndex && isChatPayload(req.SourceFormat, req.Body):
		if out, changed := normalizeChatChunk(req.RequestID, req.Body); changed {
			resp.Body = out
		}
	}
	return resp
}

func sameFormat(value string, format sdktranslator.Format) bool {
	return strings.EqualFold(strings.TrimSpace(value), format.String())
}

// knownFormats 是编译时 SDK 已知的协议格式名。宿主给出不在此列的格式名时（例如以后改名），
// 改为按内容识别协议，避免修补因为标签对不上而静默失效；已知的其他协议一律不碰。
var knownFormats = []sdktranslator.Format{
	sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude,
	sdktranslator.FormatGemini, sdktranslator.FormatCodex, sdktranslator.FormatAntigravity,
	sdktranslator.FormatInteractions,
}

func isKnownFormat(value string) bool {
	for _, f := range knownFormats {
		if sameFormat(value, f) {
			return true
		}
	}
	return false
}

// mayCarryResponses 判断请求体可能是 OpenAI Responses 协议（含改名后的未知格式名）。
func mayCarryResponses(format string) bool {
	return sameFormat(format, sdktranslator.FormatOpenAIResponse) || !isKnownFormat(format)
}

func isResponsesPayload(format string, body []byte) bool {
	if sameFormat(format, sdktranslator.FormatOpenAIResponse) {
		return true
	}
	return !isKnownFormat(format) &&
		(bytes.Contains(body, []byte(`"type":"response.`)) || bytes.Contains(body, []byte(`"object":"response"`)))
}

func isChatPayload(format string, body []byte) bool {
	if sameFormat(format, sdktranslator.FormatOpenAI) {
		return true
	}
	return !isKnownFormat(format) && bytes.Contains(body, []byte(`"object":"chat.completion.chunk"`))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func shortID(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}

func okEnvelope(result any) ([]byte, error) {
	return json.Marshal(struct {
		OK     bool `json:"ok"`
		Result any  `json:"result"`
	}{true, result})
}

func errorEnvelope(code, message string) []byte {
	out, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": message}})
	return out
}
