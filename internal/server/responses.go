// responses.go OpenAI Responses API 兼容层。
//
// 网关上游只讲 Chat Completions；Codex 等客户端默认讲 Responses（/v1/responses）。
// 本文件把 Responses 请求翻译成 Chat 请求，复用现有 chatCompletions 的选号/轮转/
// 重试/统计全链路，再把结果翻译回 Responses 协议（含 Codex 依赖的语义化流式事件）。
//
// 关键约束（DeepSeek 思考模式 + 工具循环）：
//   - 上游要求有 tool_calls 的 assistant 消息必须携带 reasoning_content，
//     否则报 11155 reasoning_content_missing；
//   - 同一轮的多个 function_call 必须合并进一条 assistant 消息的 tool_calls，
//     并保证 function_call_output 紧随其后，否则报 11148 tool_call_sequence_broken。
//
// 因此 Responses 的 reasoning item 内容会被收集并附回 assistant 消息，多个
// function_call item 会被合并为一条 assistant 消息。
package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

var responsesSeq atomic.Int64

func responsesNewID(prefix string) string {
	return fmt.Sprintf("%s_%d%06d", prefix, time.Now().UnixNano(), responsesSeq.Add(1)%1000000)
}

const encodedToolNamePrefix = "codex_tool_"

func isSafeToolName(name string) bool {
	if name == "" || strings.HasPrefix(name, encodedToolNamePrefix) {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// encodeToolName rewrites a Codex tool name into the conservative function-name
// alphabet accepted by the Chat upstream. Unsafe names are base64url encoded
// behind a marker, which keeps the mapping reversible and collision-free.
func encodeToolName(name string) string {
	if isSafeToolName(name) {
		return name
	}
	return encodedToolNamePrefix + base64.RawURLEncoding.EncodeToString([]byte(name))
}

func decodeToolName(name string) string {
	if strings.HasPrefix(name, encodedToolNamePrefix) {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(name, encodedToolNamePrefix))
		if err == nil {
			return string(raw)
		}
	}
	return name
}

// responsesRequest 只解析适配层需要的字段，其余原样忽略。
type responsesRequest struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	Instructions       json.RawMessage   `json:"instructions"`
	Stream             bool              `json:"stream"`
	Tools              []json.RawMessage `json:"tools"`
	ToolChoice         json.RawMessage   `json:"tool_choice"`
	MaxOutputTokens    *int              `json:"max_output_tokens"`
	Temperature        *float64          `json:"temperature"`
	TopP               *float64          `json:"top_p"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls"`
	Reasoning          json.RawMessage   `json:"reasoning"`
	Text               json.RawMessage   `json:"text"`
	PromptCacheKey     string            `json:"prompt_cache_key"`
	Store              *bool             `json:"store"`
	PreviousResponseID string            `json:"previous_response_id"`
	Metadata           map[string]any    `json:"metadata"`
	Truncation         string            `json:"truncation"`
	Include            []string          `json:"include"`
	Background         *bool             `json:"background"`
	ServiceTier        string            `json:"service_tier"`
	MaxToolCalls       *int              `json:"max_tool_calls"`
	SafetyIdentifier   string            `json:"safety_identifier"`
	User               string            `json:"user"`

	// baseMessages 转换后的出站 Chat messages（不含本轮 assistant 回复）。
	// 本轮回复追加后写入本地 Responses 历史（previous_response_id）。
	baseMessages []any `json:"-"`
}

// responsesUnsupportedError 明确的「本地适配层不支持」错误。
// handler 据 Code 返回 unsupported_parameter，而不是静默丢弃字段。
type responsesUnsupportedError struct {
	Code    string
	Message string
}

func (e *responsesUnsupportedError) Error() string { return e.Message }

func unsupported(code, message string) error {
	return &responsesUnsupportedError{Code: code, Message: message}
}

// validateResponsesRequest 明确拒绝本地适配层无法真实执行的语义。
// 能本地模拟的字段（store/previous_response_id/metadata/truncation=auto）放行；
// 上游没有执行器的字段立即报错，避免静默丢弃造成「看似成功」。
func validateResponsesRequest(req *responsesRequest) error {
	if req.Background != nil && *req.Background {
		return unsupported("unsupported_parameter", "background=true is not supported by this gateway")
	}
	if req.MaxToolCalls != nil {
		return unsupported("unsupported_parameter", "max_tool_calls is not supported by this gateway")
	}
	if req.Truncation != "" && req.Truncation != "disabled" && req.Truncation != "auto" {
		return unsupported("unsupported_parameter", "truncation="+req.Truncation+" is not supported by this gateway")
	}
	if req.ServiceTier != "" && req.ServiceTier != "auto" && req.ServiceTier != "default" {
		return unsupported("unsupported_parameter", "service_tier="+req.ServiceTier+" is not supported by this gateway")
	}
	for _, inc := range req.Include {
		switch inc {
		case "reasoning.encrypted_content":
			// 本地 reasoning item 始终携带 encrypted_content 载体。
		default:
			return unsupported("unsupported_parameter", "include="+inc+" is not supported by this gateway")
		}
	}
	return nil
}

// textFromContent 把 Responses 的 content（字符串或内容数组）压平成纯文本。
func textFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		t, _ := p["type"].(string)
		switch t {
		case "input_text", "output_text", "text", "summary_text", "reasoning_text":
			if v, ok := p["text"].(string); ok {
				b.WriteString(v)
			}
		case "refusal":
			if v, ok := p["refusal"].(string); ok {
				b.WriteString(v)
			}
		}
	}
	return b.String()
}

// reasoningItemText 取 reasoning item 的推理文本。
//
// 回传优先级：encrypted_content（本网关输出的可回传载体）→ summary → content。
// Codex 在 store=false 时不会带上被剥离的 summary 之外的信息，因此网关把推理文本
// 编码进 encrypted_content 交给客户端原样回传，是保证 DeepSeek 思考模式链路不断
// 的唯一可靠方式（上游要求带 tool_calls 的 assistant 消息必须回传 reasoning_content）。
func reasoningItemText(it map[string]any) string {
	if enc, ok := it["encrypted_content"].(string); ok && enc != "" {
		if raw, err := base64.StdEncoding.DecodeString(enc); err == nil && len(raw) > 0 {
			return string(raw)
		}
	}
	var b strings.Builder
	if raw, err := json.Marshal(it["summary"]); err == nil {
		b.WriteString(textFromContent(raw))
	}
	if raw, err := json.Marshal(it["content"]); err == nil {
		b.WriteString(textFromContent(raw))
	}
	return b.String()
}

// reasoningCarrier 把推理文本编码成可回传载体（encrypted_content）。
func reasoningCarrier(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(text))
}

// reasoningItem 构造带可回传载体的 reasoning output item。
func reasoningItem(id, text, status string) map[string]any {
	item := map[string]any{
		"type":    "reasoning",
		"id":      id,
		"status":  status,
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
		"content": []any{},
	}
	if c := reasoningCarrier(text); c != "" {
		item["encrypted_content"] = c
	}
	return item
}

// imagePartToChat converts a Responses input_image part to a Chat image_url
// part. detail is preserved when present; image_url may be a string or object.
func imagePartToChat(p map[string]any) (map[string]any, bool) {
	url := ""
	detail := ""
	switch v := p["image_url"].(type) {
	case string:
		url = v
	case map[string]any:
		url, _ = v["url"].(string)
		if d, ok := v["detail"].(string); ok {
			detail = d
		}
	}
	if url == "" {
		return nil, false
	}
	if d, ok := p["detail"].(string); ok && d != "" {
		detail = d
	}
	img := map[string]any{"url": url}
	if detail != "" {
		img["detail"] = detail
	}
	return map[string]any{"type": "image_url", "image_url": img}, true
}

// contentToChat 把 Responses 内容数组转成 Chat 的 content（纯文本或分段数组）。
func contentToChat(raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return textFromContent(raw)
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch p["type"] {
		case "input_text", "output_text", "text":
			out = append(out, map[string]any{"type": "text", "text": p["text"]})
		case "input_image":
			if part, ok := imagePartToChat(p); ok {
				out = append(out, part)
			}
		}
	}
	if len(out) == 0 {
		return textFromContent(raw)
	}
	return out
}

// toolOutputToChat converts function_call_output.output into Chat tool content.
// Plain strings stay strings. Standard content blocks (notably images) become
// chat multimodal parts instead of being JSON-stringified: a data URL image
// would otherwise travel as high-entropy base64 text, which the upstream
// tokenizer charges at roughly 0.7 token per character (a 100KB PNG measured
// about 93k prompt tokens, versus about 250 tokens as an image part).
func toolOutputToChat(v any) any {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		if isImageDataURL(t) {
			return []any{map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": t},
			}}
		}
		return t
	case []any:
		out := make([]any, 0, len(t))
		for _, part := range t {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch p["type"] {
			case "input_text", "output_text", "text":
				if text, ok := p["text"].(string); ok {
					out = append(out, map[string]any{"type": "text", "text": text})
				}
			case "input_image":
				if part, ok := imagePartToChat(p); ok {
					out = append(out, part)
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if raw, err := json.Marshal(v); err == nil {
		return string(raw)
	}
	return ""
}

// isImageDataURL reports whether s is a base64 data URL for a supported image
// type. Tool output occasionally carries a raw data URL string rather than a
// structured content block; sending it as text makes the tokenizer charge the
// base64 payload by character instead of billing it as an image.
func isImageDataURL(s string) bool {
	if len(s) < 32 || !strings.HasPrefix(s, "data:image/") {
		return false
	}
	return strings.Contains(s, ";base64,")
}

// stringifyOutput 工具的 output 可能是字符串或结构化数组，统一转成字符串。
func stringifyOutput(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		if raw, err := json.Marshal(t); err == nil {
			return string(raw)
		}
		return ""
	}
}

// convertResponsesTool converts one Responses API tool declaration into a Chat
// Completions function declaration. Custom/freeform tools are exposed as a
// function with a single `input` string, which is the closest lossless shape.
func convertResponsesTool(t map[string]any) map[string]any {
	if t == nil {
		return nil
	}
	name, _ := t["name"].(string)
	if name == "" {
		return nil
	}
	if ns, _ := t["namespace"].(string); ns != "" {
		name = ns + "." + name
	}
	name = encodeToolName(name)
	typ, _ := t["type"].(string)
	fn := map[string]any{"name": name}
	if v, ok := t["description"]; ok {
		fn["description"] = v
	}
	switch typ {
	case "function", "":
		for _, k := range []string{"parameters", "strict"} {
			if v, ok := t[k]; ok {
				fn[k] = v
			}
		}
	case "custom":
		fn["parameters"] = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{"type": "string", "description": "Raw tool input"},
			},
			"required":             []any{"input"},
			"additionalProperties": false,
		}
	case "tool_search":
		if v, ok := t["parameters"]; ok {
			fn["parameters"] = v
		}
		fn["strict"] = false
	default:
		return nil
	}
	if fn["parameters"] == nil {
		fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	}
	return map[string]any{"type": "function", "function": fn}
}

// responsesToChat 把 Responses 请求体翻译成 Chat Completions 请求体。
// 兼容入口：无本地历史时等价 responsesToChatWithHistory(body, nil)。
func responsesToChat(body []byte) ([]byte, *responsesRequest, error) {
	return responsesToChatWithHistory(body, nil)
}

// responsesToChatWithHistory 在转换前把 previous_response_id 对应的历史
// Chat messages 前置到本轮消息之前。prior 为 nil 时行为与旧实现完全一致。
func responsesToChatWithHistory(body []byte, prior []any) ([]byte, *responsesRequest, error) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, fmt.Errorf("parse responses request: %w", err)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, nil, fmt.Errorf("missing required field: model")
	}
	if err := validateResponsesRequest(&req); err != nil {
		return nil, nil, err
	}
	chat := map[string]any{"model": req.Model}
	messages := make([]any, 0, 8+len(prior))

	// previous_response_id：前置上一轮历史（含 assistant 回复）。
	// 上游无状态，历史必须随本轮请求重放；本地拼接只补协议语义，不减少上游 token。
	// 过滤 system/developer：本轮会按 instructions/消息重新生成系统提示词，
	// 若把历史 system 一并回放，多轮后系统提示词会重复累积、放大 token。
	for _, m := range prior {
		msg, _ := m.(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		messages = append(messages, m)
	}

	// instructions → system 消息
	if len(req.Instructions) > 0 {
		if s := textFromContent(req.Instructions); s != "" {
			messages = append(messages, map[string]any{"role": "system", "content": s})
		}
	}

	// Responses 把助手的一轮拆成多个 item（message 文本 + 若干 function_call）；
	// Chat Completions 要求它们是**同一条** assistant 消息（content + tool_calls）。
	// 拆成多条会被上游判定工具序列不配对（11148），并丢失 reasoning 关联（11155）。
	// 因此这里把同一轮的 assistant 文本、reasoning、function_call 合并成一条消息。
	var pendingReasoning strings.Builder
	var pendingAssistant strings.Builder
	var pendingCalls []any
	var discoveredTools []any
	flushAssistant := func() {
		text := pendingAssistant.String()
		hasCalls := len(pendingCalls) > 0
		reasoning := strings.TrimSpace(pendingReasoning.String())
		if !hasCalls && text == "" && reasoning == "" {
			return
		}
		msg := map[string]any{"role": "assistant"}
		if text != "" {
			msg["content"] = text
		} else {
			msg["content"] = nil
		}
		if hasCalls {
			msg["tool_calls"] = pendingCalls
		}
		// DeepSeek 思考模式要求：带 tool_calls 的 assistant 消息必须携带
		// reasoning_content（缺失会被上游 400/11155）。无明文可回传时回填最小占位，
		// 仅满足协议字段要求，不编造推理结论。
		if reasoning == "" && hasCalls {
			reasoning = "continued tool use"
		}
		if reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
		messages = append(messages, msg)
		pendingCalls = nil
		pendingAssistant.Reset()
		pendingReasoning.Reset()
	}

	appendInput := func(items []map[string]any) {
		logResponsesInputShape(items)
		for _, it := range items {
			typ, _ := it["type"].(string)
			switch typ {
			case "reasoning":
				pendingReasoning.WriteString(reasoningItemText(it))
			case "function_call":
				callID, _ := it["call_id"].(string)
				if callID == "" {
					callID, _ = it["id"].(string)
				}
				name, _ := it["name"].(string)
				name = encodeToolName(name)
				args, _ := it["arguments"].(string)
				pendingCalls = append(pendingCalls, map[string]any{
					"id": callID, "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				})
			case "tool_search_call":
				callID, _ := it["call_id"].(string)
				if callID == "" {
					callID, _ = it["id"].(string)
				}
				args := "{}"
				switch v := it["arguments"].(type) {
				case string:
					if v != "" {
						args = v
					}
				case nil:
				default:
					if raw, err := json.Marshal(v); err == nil {
						args = string(raw)
					}
				}
				pendingCalls = append(pendingCalls, map[string]any{
					"id": callID, "type": "function",
					"function": map[string]any{"name": "tool_search", "arguments": args},
				})
			case "function_call_output":
				flushAssistant()
				callID, _ := it["call_id"].(string)
				messages = append(messages, map[string]any{
					"role": "tool", "tool_call_id": callID, "content": toolOutputToChat(it["output"]),
				})
			case "tool_search_output":
				flushAssistant()
				callID, _ := it["call_id"].(string)
				if callID == "" {
					callID, _ = it["id"].(string)
				}
				payload := it["tools"]
				if payload == nil {
					payload = it["output"]
				}
				if rawTools, ok := payload.([]any); ok {
					discoveredTools = append(discoveredTools, rawTools...)
				}
				messages = append(messages, map[string]any{
					"role": "tool", "tool_call_id": callID, "content": stringifyOutput(payload),
				})
			default:
				role, _ := it["role"].(string)
				if role == "" {
					continue
				}
				if role == "developer" {
					role = "system"
				}
				// assistant 文本先暂存，等同一轮的 function_call 到达后合并成一条消息。
				if role == "assistant" {
					if rawContent, ok := it["content"]; ok {
						if b, err := json.Marshal(rawContent); err == nil {
							if s := textFromContent(b); s != "" {
								pendingAssistant.WriteString(s)
							}
						}
					}
					continue
				}
				flushAssistant()
				if rawContent, ok := it["content"]; ok {
					if b, err := json.Marshal(rawContent); err == nil {
						messages = append(messages, map[string]any{"role": role, "content": contentToChat(b)})
						continue
					}
				}
				messages = append(messages, map[string]any{"role": role, "content": ""})
			}
		}
	}

	if len(req.Input) > 0 {
		var s string
		if json.Unmarshal(req.Input, &s) == nil {
			messages = append(messages, map[string]any{"role": "user", "content": s})
		} else {
			var items []map[string]any
			if err := json.Unmarshal(req.Input, &items); err != nil {
				return nil, nil, fmt.Errorf("parse input: %w", err)
			}
			appendInput(items)
		}
	}
	flushAssistant()
	chat["messages"] = messages
	req.baseMessages = messages

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		seenTools := map[string]bool{}
		appendTool := func(tool map[string]any) {
			name, _ := tool["name"].(string)
			if name == "" {
				return
			}
			if ns, _ := tool["namespace"].(string); ns != "" {
				name = ns + "." + name
			}
			if seenTools[name] {
				return
			}
			seenTools[name] = true
			if fn := convertResponsesTool(tool); fn != nil {
				tools = append(tools, fn)
			}
		}
		for _, raw := range req.Tools {
			var t map[string]any
			if json.Unmarshal(raw, &t) != nil {
				continue
			}
			typ, _ := t["type"].(string)
			switch typ {
			case "namespace":
				ns, _ := t["name"].(string)
				children, _ := t["tools"].([]any)
				for _, rawChild := range children {
					child, _ := rawChild.(map[string]any)
					if child == nil {
						continue
					}
					clone := make(map[string]any, len(child)+1)
					for k, v := range child {
						clone[k] = v
					}
					if _, ok := clone["namespace"]; !ok && ns != "" {
						clone["namespace"] = ns
					}
					appendTool(clone)
				}
			case "tool_search":
				search := map[string]any{
					"type":        "function",
					"name":        "tool_search",
					"description": t["description"],
					"parameters":  t["parameters"],
					"strict":      false,
				}
				appendTool(search)
			default:
				appendTool(t)
			}
		}
		for _, discovered := range discoveredTools {
			found, _ := discovered.(map[string]any)
			if found == nil {
				continue
			}
			ns, _ := found["name"].(string)
			children, _ := found["tools"].([]any)
			for _, rawChild := range children {
				child, _ := rawChild.(map[string]any)
				if child == nil {
					continue
				}
				clone := make(map[string]any, len(child)+1)
				for k, v := range child {
					clone[k] = v
				}
				if _, ok := clone["namespace"]; !ok && ns != "" {
					clone["namespace"] = ns
				}
				appendTool(clone)
			}
		}
		if len(tools) > 0 {
			chat["tools"] = tools
		}
	}
	if len(req.ToolChoice) > 0 {
		var s string
		if json.Unmarshal(req.ToolChoice, &s) == nil {
			chat["tool_choice"] = s
		} else {
			var tc map[string]any
			if json.Unmarshal(req.ToolChoice, &tc) == nil {
				if name, ok := tc["name"].(string); ok && name != "" {
					chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
				}
			}
		}
	}
	if req.MaxOutputTokens != nil {
		chat["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		chat["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if len(req.Reasoning) > 0 {
		var r map[string]any
		if json.Unmarshal(req.Reasoning, &r) == nil {
			if e, ok := r["effort"].(string); ok && e != "" {
				chat["reasoning_effort"] = e
			}
		}
	}
	if len(req.Text) > 0 {
		var t struct {
			Format json.RawMessage `json:"format"`
		}
		if json.Unmarshal(req.Text, &t) == nil && len(t.Format) > 0 {
			chat["response_format"] = t.Format
		}
	}
	chat["stream"] = req.Stream
	out, err := json.Marshal(chat)
	if err != nil {
		return nil, nil, err
	}
	return out, &req, nil
}

// logResponsesInputShape 记录 Codex 回传的 item 结构（只记类型与字段名，不记内容），
// 用于诊断 reasoning/工具回传协议问题。
func logResponsesInputShape(items []map[string]any) {
	if len(items) == 0 {
		return
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		typ, _ := it["type"].(string)
		if typ == "" {
			if role, ok := it["role"].(string); ok {
				typ = "message:" + role
			} else {
				typ = "unknown"
			}
		}
		keys := make([]string, 0, len(it))
		for k := range it {
			keys = append(keys, k)
		}
		parts = append(parts, typ+"{"+strings.Join(keys, ",")+"}")
	}
	log.Printf("[responses] input items: %s", strings.Join(parts, " "))
}

// usageToResponses Chat usage → Responses usage。
func usageToResponses(u *UsageDetail) map[string]any {
	if u == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	return map[string]any{
		"input_tokens":          u.PromptTokens,
		"output_tokens":         u.CompletionTokens,
		"total_tokens":          u.TotalTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": u.CacheHitTokens},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
}

// responsesEnvelope 构造 Response 对象。
func responsesEnvelope(id, model, status string, output []any, usage map[string]any) map[string]any {
	if output == nil {
		output = []any{}
	}
	if usage == nil {
		usage = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	return map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           time.Now().Unix(),
		"status":               status,
		"model":                model,
		"output":               output,
		"usage":                usage,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"metadata":             map[string]any{},
		"parallel_tool_calls":  true,
		"temperature":          1.0,
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                1.0,
		"reasoning":            nil,
		"store":                false,
		"truncation":           "disabled",
		"previous_response_id": nil,
	}
}

// responsesStoreRequested 判断请求是否要求本地保存响应。
// store 未传时按 OpenAI 默认（false）处理，避免默认放大内存占用。
func responsesStoreRequested(req *responsesRequest) bool {
	return req != nil && req.Store != nil && *req.Store
}

// applyResponsesEcho 把请求里的可回显字段写回最终 Response 对象。
// 上游不参与这些字段，回显是为了让客户端看到的 Response 与其请求一致。
func applyResponsesEcho(env map[string]any, req *responsesRequest) {
	if env == nil || req == nil {
		return
	}
	if req.Metadata != nil {
		env["metadata"] = req.Metadata
	} else {
		env["metadata"] = map[string]any{}
	}
	env["store"] = responsesStoreRequested(req)
	if req.Truncation != "" {
		env["truncation"] = req.Truncation
	} else {
		env["truncation"] = "disabled"
	}
	if req.PreviousResponseID != "" {
		env["previous_response_id"] = req.PreviousResponseID
	} else {
		env["previous_response_id"] = nil
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, raw := range req.Tools {
			var t map[string]any
			if json.Unmarshal(raw, &t) == nil {
				tools = append(tools, t)
			}
		}
		env["tools"] = tools
	} else {
		env["tools"] = []any{}
	}
	if len(req.ToolChoice) > 0 {
		var tc any
		if json.Unmarshal(req.ToolChoice, &tc) == nil {
			env["tool_choice"] = tc
		}
	}
	if req.ParallelToolCalls != nil {
		env["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.Temperature != nil {
		env["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		env["top_p"] = *req.TopP
	}
	if len(req.Reasoning) > 0 {
		var r any
		if json.Unmarshal(req.Reasoning, &r) == nil {
			env["reasoning"] = r
		}
	}
	if req.MaxOutputTokens != nil {
		env["max_output_tokens"] = *req.MaxOutputTokens
	}
	if req.User != "" {
		env["user"] = req.User
	}
	if req.SafetyIdentifier != "" {
		env["safety_identifier"] = req.SafetyIdentifier
	}
}

// responsesMessageItem 构造 assistant message output item。
func responsesMessageItem(id, text string, status string) map[string]any {
	return map[string]any{
		"id":      id,
		"type":    "message",
		"status":  status,
		"role":    "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
}

func responsesToolCallItem(id, callID, name, args, status string) map[string]any {
	name = decodeToolName(name)
	if name == "tool_search" {
		var arguments any
		if args == "" || json.Unmarshal([]byte(args), &arguments) != nil {
			arguments = map[string]any{}
		}
		return map[string]any{
			"type":      "tool_search_call",
			"id":        id,
			"call_id":   callID,
			"status":    status,
			"execution": "client",
			"arguments": arguments,
		}
	}
	return map[string]any{
		"type": "function_call", "id": id, "call_id": callID,
		"name": name, "arguments": args, "status": status,
	}
}

// responsesHandle 处理 POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限", limit>>20))
		return
	}
	// previous_response_id：先解析顶层字段，命中本地历史再拼接。
	var peek responsesRequest
	if err := json.Unmarshal(body, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse responses request: "+err.Error())
		return
	}
	var prior []any
	if peek.PreviousResponseID != "" {
		var ok bool
		_, prior, ok = h.respStore.Get(peek.PreviousResponseID)
		if !ok {
			writeOpenAIError(w, http.StatusNotFound, "not_found",
				"previous_response_id not found: "+peek.PreviousResponseID)
			return
		}
	}
	chatBody, meta, err := responsesToChatWithHistory(body, prior)
	if err != nil {
		var ue *responsesUnsupportedError
		if errors.As(err, &ue) {
			writeOpenAIError(w, http.StatusBadRequest, ue.Code, ue.Message)
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var base []any
	if meta != nil && len(meta.baseMessages) > 0 {
		base = meta.baseMessages
	}
	// 复用 chatCompletions：替换 body，保持同一请求上下文（断连取消）。
	inner := r.Clone(r.Context())
	inner.Body = io.NopCloser(bytes.NewReader(chatBody))
	inner.ContentLength = int64(len(chatBody))

	if meta.Stream {
		proxy := newResponsesStreamProxy(w, meta)
		proxy.baseMessages = base
		h.chatCompletions(proxy, inner)
		proxy.finish()
		h.saveResponsesResult(meta, proxy, base)
		return
	}
	rec := newResponsesCapture()
	h.chatCompletions(rec, inner)
	env := responsesWriteNonStream(w, meta, rec)
	if env != nil {
		h.saveResponsesEnvelope(meta, env, base)
	}
}

// saveResponsesResult 在流式结束后按 store=true 保存最终 Response 对象。
func (h *Handler) saveResponsesResult(meta *responsesRequest, proxy *responsesStreamProxy, base []any) {
	if !responsesStoreRequested(meta) || proxy == nil || proxy.finalEnvelope == nil {
		return
	}
	h.saveResponsesEnvelope(meta, proxy.finalEnvelope, base)
}

// saveResponsesEnvelope 保存 Response 对象与下一轮所需的 Chat messages。
// messages = 本轮出站 messages + 本轮 assistant 回复（文本与 tool_calls 合并为同一条）。
func (h *Handler) saveResponsesEnvelope(meta *responsesRequest, env map[string]any, base []any) {
	if h.respStore == nil || !responsesStoreRequested(meta) || env == nil {
		return
	}
	id, _ := env["id"].(string)
	if id == "" {
		return
	}
	msgs := append([]any{}, base...)
	if out, ok := env["output"].([]any); ok {
		if msg := assistantMessageFromOutput(out); msg != nil {
			msgs = append(msgs, msg)
		}
	}
	h.respStore.Put(id, env, msgs)
}

// assistantMessageFromOutput 把 Responses output 里的 assistant 文本与 function_call
// 合并为一条 Chat assistant 消息（深寻思考模式要求带 tool_calls 的消息同时有 content
// 与 reasoning 关联；这里保留 content=tool_calls 的配对语义）。两者都无 → nil。
func assistantMessageFromOutput(output []any) map[string]any {
	var text strings.Builder
	var calls []any
	for _, item := range output {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		switch m["type"] {
		case "message":
			text.WriteString(outputMessageText(m))
		case "function_call":
			callID, _ := m["call_id"].(string)
			if callID == "" {
				callID, _ = m["id"].(string)
			}
			name, _ := m["name"].(string)
			args, _ := m["arguments"].(string)
			calls = append(calls, map[string]any{
				"id": callID, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
		}
	}
	if len(calls) == 0 && text.Len() == 0 {
		return nil
	}
	msg := map[string]any{"role": "assistant"}
	if s := text.String(); s != "" {
		msg["content"] = s
	} else {
		msg["content"] = nil
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	return msg
}

// outputMessageText 提取 Responses message item 的纯文本。
func outputMessageText(item map[string]any) string {
	content, _ := item["content"].([]any)
	var b strings.Builder
	for _, part := range content {
		p, _ := part.(map[string]any)
		if p == nil {
			continue
		}
		if t, _ := p["type"].(string); t == "output_text" {
			if s, ok := p["text"].(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

// responsesCapture 缓冲 chatCompletions 的非流式输出。
type responsesCapture struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func newResponsesCapture() *responsesCapture {
	return &responsesCapture{header: http.Header{}}
}

func (c *responsesCapture) Header() http.Header { return c.header }
func (c *responsesCapture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
func (c *responsesCapture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.buf.Write(p)
}

// responsesWriteNonStream 把 Chat 聚合响应翻译成 Responses 响应，并返回 Response 对象。
func responsesWriteNonStream(w http.ResponseWriter, meta *responsesRequest, rec *responsesCapture) map[string]any {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	if status >= 400 {
		writeOpenAIError(w, status, "upstream_error", strings.TrimSpace(rec.buf.String()))
		return nil
	}
	var chat map[string]any
	if err := json.Unmarshal(rec.buf.Bytes(), &chat); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return nil
	}
	respID := responsesNewID("resp")
	var text strings.Builder
	var reasoning strings.Builder
	finishReason := ""
	output := make([]any, 0, 4)
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			finishReason, _ = c["finish_reason"].(string)
			if msg, ok := c["message"].(map[string]any); ok {
				if v, ok := msg["reasoning_content"].(string); ok && v != "" {
					reasoning.WriteString(v)
				}
				if v, ok := msg["content"].(string); ok {
					text.WriteString(v)
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tci := range tcs {
						tc, _ := tci.(map[string]any)
						if tc == nil {
							continue
						}
						fn, _ := tc["function"].(map[string]any)
						name, _ := fn["name"].(string)
						args, _ := fn["arguments"].(string)
						callID, _ := tc["id"].(string)
						if callID == "" {
							callID = responsesNewID("call")
						}
						output = append(output, responsesToolCallItem(
							responsesNewID("fc"), callID, name, args, "completed",
						))
					}
				}
			}
		}
	}
	if reasoning.Len() > 0 {
		output = append([]any{reasoningItem(responsesNewID("rs"), reasoning.String(), "completed")}, output...)
	}
	if text.Len() > 0 || len(output) == 0 {
		output = append(output, responsesMessageItem(responsesNewID("msg"), text.String(), "completed"))
	}
	var usage *UsageDetail
	if u, ok := chat["usage"].(map[string]any); ok {
		usage = ParseUsage(u)
	}
	respStatus := "completed"
	if finishReason == "length" {
		respStatus = "incomplete"
	}
	env := responsesEnvelope(respID, meta.Model, respStatus, output, usageToResponses(usage))
	env["output_text"] = text.String()
	applyResponsesEcho(env, meta)
	if respStatus == "incomplete" {
		env["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	writeJSON(w, http.StatusOK, env)
	return env
}

// responsesCall 单个 function_call 的累计状态。
type responsesCall struct {
	id     string
	callID string
	name   string
	args   strings.Builder
	index  int
	added  bool
}

// responsesStreamProxy 实现 http.ResponseWriter，把 chatCompletions 写出的
// SSE 帧实时翻译成 Responses 语义事件。
type responsesStreamProxy struct {
	dest http.ResponseWriter
	fl   http.Flusher
	meta *responsesRequest

	header http.Header
	status int
	buf    bytes.Buffer

	respID    string
	started   bool
	completed bool
	seq       int
	nextIndex int

	rsOpen   bool
	rsID     string
	rsIndex  int
	rsText   strings.Builder
	rsDone   bool
	msgOpen  bool
	msgID    string
	msgIndex int
	text     strings.Builder

	calls     map[int]*responsesCall
	callOrder []int

	usage    *UsageDetail
	hadError bool
	errMsg   string
	// finishReason 上游末帧 finish_reason（stop/length/tool_calls）。
	// length 时最终状态必须是 incomplete，不能谎报 completed。
	finishReason string

	// finalEnvelope 最终 Response 对象（finish 时写入，供 store=true 保存）。
	finalEnvelope map[string]any
	// baseMessages 本轮出站 Chat messages（不含 assistant 回复）。
	baseMessages []any
}

func newResponsesStreamProxy(dest http.ResponseWriter, meta *responsesRequest) *responsesStreamProxy {
	fl, _ := dest.(http.Flusher)
	return &responsesStreamProxy{
		dest: dest, fl: fl, meta: meta, header: http.Header{},
		respID: responsesNewID("resp"), calls: map[int]*responsesCall{},
	}
}

func (p *responsesStreamProxy) Header() http.Header { return p.header }
func (p *responsesStreamProxy) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}
func (p *responsesStreamProxy) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	p.buf.Write(b)
	p.drain()
	return len(b), nil
}
func (p *responsesStreamProxy) Flush() {
	if p.fl != nil {
		p.fl.Flush()
	}
}
func (p *responsesStreamProxy) allocIndex() int {
	i := p.nextIndex
	p.nextIndex++
	return i
}

// drain 解析缓冲区里已完整的 SSE 帧。
func (p *responsesStreamProxy) drain() {
	for {
		raw := p.buf.String()
		idx := strings.Index(raw, "\n\n")
		if idx < 0 {
			return
		}
		frame := raw[:idx]
		p.buf.Next(idx + 2)
		frame = strings.TrimSpace(frame)
		if !strings.HasPrefix(frame, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(frame, "data: ")
		if payload == "[DONE]" {
			return
		}
		p.handleChunk(payload)
	}
}

func (p *responsesStreamProxy) handleChunk(payload string) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return
	}
	if errObj, ok := obj["error"].(map[string]any); ok {
		p.hadError = true
		if m, ok := errObj["message"].(string); ok {
			p.errMsg = m
		}
		return
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		p.usage = ParseUsage(u)
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		p.finishReason = fr
	}
	if delta, ok := c["delta"].(map[string]any); ok {
		if v, ok := delta["reasoning_content"].(string); ok && v != "" {
			p.appendReasoning(v)
		}
		if v, ok := delta["content"].(string); ok && v != "" {
			p.appendText(v)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			p.appendToolCalls(tcs)
		}
	}
}

// openSSE 发送 SSE 响应头（只发一次）。
func (p *responsesStreamProxy) openSSE() {
	h := p.dest.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	// 不显式 WriteHeader：由首个事件写入触发隐式 200，避免在 chat 路径已写过
	// 状态码时产生 superfluous response.WriteHeader 噪声。
}

func (p *responsesStreamProxy) ensureStart() {
	if p.started {
		return
	}
	p.started = true
	p.openSSE()
	p.emit("response.created", map[string]any{
		"response": responsesEnvelope(p.respID, p.meta.Model, "in_progress", nil, nil),
	})
	p.emit("response.in_progress", map[string]any{
		"response": responsesEnvelope(p.respID, p.meta.Model, "in_progress", nil, nil),
	})
}

func (p *responsesStreamProxy) emit(eventType string, fields map[string]any) {
	p.seq++
	ev := map[string]any{"type": eventType, "sequence_number": p.seq}
	for k, v := range fields {
		ev[k] = v
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = io.WriteString(p.dest, "event: "+eventType+"\ndata: "+string(raw)+"\n\n")
	p.Flush()
}

func (p *responsesStreamProxy) appendReasoning(delta string) {
	p.ensureStart()
	p.rsText.WriteString(delta)
	if !p.rsOpen {
		p.rsOpen = true
		p.rsID = responsesNewID("rs")
		p.rsIndex = p.allocIndex()
		p.emit("response.output_item.added", map[string]any{
			"output_index": p.rsIndex,
			"item": map[string]any{
				"type": "reasoning", "id": p.rsID, "status": "in_progress", "summary": []any{},
			},
		})
		p.emit("response.reasoning_summary_part.added", map[string]any{
			"output_index": p.rsIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	p.emit("response.reasoning_summary_text.delta", map[string]any{
		"output_index": p.rsIndex, "summary_index": 0, "delta": delta,
	})
}

func (p *responsesStreamProxy) appendText(delta string) {
	p.ensureStart()
	if !p.msgOpen {
		p.msgOpen = true
		p.msgID = responsesNewID("msg")
		p.msgIndex = p.allocIndex()
		p.emit("response.output_item.added", map[string]any{
			"output_index": p.msgIndex, "item": responsesMessageItem(p.msgID, "", "in_progress"),
		})
		p.emit("response.content_part.added", map[string]any{
			"item_id": p.msgID, "output_index": p.msgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	p.text.WriteString(delta)
	p.emit("response.output_text.delta", map[string]any{
		"item_id": p.msgID, "output_index": p.msgIndex, "content_index": 0, "delta": delta,
	})
}

func (p *responsesStreamProxy) appendToolCalls(tcs []any) {
	p.ensureStart()
	for _, tci := range tcs {
		tc, _ := tci.(map[string]any)
		if tc == nil {
			continue
		}
		idx := 0
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		call := p.calls[idx]
		if call == nil {
			call = &responsesCall{}
			p.calls[idx] = call
			p.callOrder = append(p.callOrder, idx)
		}
		if v, ok := tc["id"].(string); ok && v != "" {
			call.callID = v
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if v, ok := fn["name"].(string); ok && v != "" {
				call.name = v
			}
			if call.id == "" {
				call.id = responsesNewID("fc")
			}
			if call.callID == "" {
				call.callID = responsesNewID("call")
			}
			if call.name != "" && !call.added {
				call.added = true
				call.index = p.allocIndex()
				p.emit("response.output_item.added", map[string]any{
					"output_index": call.index,
					"item":         responsesToolCallItem(call.id, call.callID, call.name, "", "in_progress"),
				})
			}
			if v, ok := fn["arguments"].(string); ok && v != "" {
				call.args.WriteString(v)
				if call.added && decodeToolName(call.name) != "tool_search" {
					p.emit("response.function_call_arguments.delta", map[string]any{
						"item_id": call.id, "output_index": call.index, "delta": v,
					})
				}
			}
		}
	}
}

// finish 收尾：关闭未结束的 item，发出 response.completed / response.failed。
func (p *responsesStreamProxy) finish() {
	if p.completed {
		return
	}
	p.completed = true

	// 上游失败（chatCompletions 走了错误分支）：把错误翻译成 response.failed，
	// 避免把 JSON 错误体直接当作 SSE 写给客户端。
	if p.status >= 400 || p.hadError {
		p.openSSE()
		msg := strings.TrimSpace(p.buf.String())
		if p.errMsg != "" {
			msg = p.errMsg
		}
		if msg == "" {
			msg = "upstream request failed"
		}
		p.emit("response.created", map[string]any{
			"response": responsesEnvelope(p.respID, p.meta.Model, "in_progress", nil, nil),
		})
		p.emit("response.failed", map[string]any{
			"response": failedResponsesEnvelope(p.respID, p.meta, msg),
		})
		return
	}
	p.ensureStart()

	output := make([]any, 0, 4)
	if p.rsOpen {
		summary := p.rsText.String()
		p.emit("response.reasoning_summary_text.done", map[string]any{
			"output_index": p.rsIndex, "summary_index": 0, "text": summary,
		})
		p.emit("response.reasoning_summary_part.done", map[string]any{
			"output_index": p.rsIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": summary},
		})
		item := reasoningItem(p.rsID, summary, "completed")
		p.emit("response.output_item.done", map[string]any{"output_index": p.rsIndex, "item": item})
		output = append(output, item)
	}
	if p.msgOpen {
		text := p.text.String()
		p.emit("response.output_text.done", map[string]any{
			"item_id": p.msgID, "output_index": p.msgIndex, "content_index": 0, "text": text,
		})
		p.emit("response.content_part.done", map[string]any{
			"item_id": p.msgID, "output_index": p.msgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})
		item := responsesMessageItem(p.msgID, text, "completed")
		p.emit("response.output_item.done", map[string]any{"output_index": p.msgIndex, "item": item})
		output = append(output, item)
	}
	for _, idx := range p.callOrder {
		call := p.calls[idx]
		if call == nil {
			continue
		}
		if call.id == "" {
			call.id = responsesNewID("fc")
		}
		if call.callID == "" {
			call.callID = responsesNewID("call")
		}
		if !call.added {
			call.added = true
			call.index = p.allocIndex()
			item := responsesToolCallItem(call.id, call.callID, call.name, "", "in_progress")
			p.emit("response.output_item.added", map[string]any{
				"output_index": call.index,
				"item":         item,
			})
		}
		args := call.args.String()
		if decodeToolName(call.name) != "tool_search" {
			p.emit("response.function_call_arguments.done", map[string]any{
				"item_id": call.id, "output_index": call.index, "arguments": args,
			})
		}
		item := responsesToolCallItem(call.id, call.callID, call.name, args, "completed")
		p.emit("response.output_item.done", map[string]any{"output_index": call.index, "item": item})
		output = append(output, item)
	}
	if len(output) == 0 {
		item := responsesMessageItem(responsesNewID("msg"), "", "completed")
		output = append(output, item)
	}
	respStatus := "completed"
	if p.finishReason == "length" {
		respStatus = "incomplete"
	}
	env := responsesEnvelope(p.respID, p.meta.Model, respStatus, output, usageToResponses(p.usage))
	applyResponsesEcho(env, p.meta)
	if respStatus == "incomplete" {
		env["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	p.finalEnvelope = env
	if respStatus == "incomplete" {
		p.emit("response.incomplete", map[string]any{"response": env})
		return
	}
	p.emit("response.completed", map[string]any{"response": env})
}

// failedResponsesEnvelope 构造完整的失败 Response 对象。
// 旧实现只回六个字段；这里复用完整 envelope 再覆盖状态与错误，
// 客户端仍能拿到请求相关的 metadata/store 回显。
func failedResponsesEnvelope(id string, meta *responsesRequest, msg string) map[string]any {
	model := ""
	if meta != nil {
		model = meta.Model
	}
	env := responsesEnvelope(id, model, "failed", []any{}, nil)
	env["error"] = map[string]any{"code": "server_error", "message": msg}
	applyResponsesEcho(env, meta)
	return env
}

// responsesGet 返回本地保存的 Response；未保存/已过期返回 404。
func (h *Handler) responsesGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	env, _, ok := h.respStore.Get(id)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "response not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// responsesDelete 删除本地保存的 Response；未找到返回 404。
func (h *Handler) responsesDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.respStore.Delete(id) {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "response not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}
