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

// responsesRequest 只解析适配层需要的字段，其余原样忽略。
type responsesRequest struct {
	Model             string            `json:"model"`
	Input             json.RawMessage   `json:"input"`
	Instructions      json.RawMessage   `json:"instructions"`
	Stream            bool              `json:"stream"`
	Tools             []json.RawMessage `json:"tools"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	MaxOutputTokens   *int              `json:"max_output_tokens"`
	Temperature       *float64          `json:"temperature"`
	TopP              *float64          `json:"top_p"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	Reasoning         json.RawMessage   `json:"reasoning"`
	Text              json.RawMessage   `json:"text"`
	PromptCacheKey    string            `json:"prompt_cache_key"`
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
		"type":     "reasoning",
		"id":       id,
		"status":   status,
		"summary":  []any{map[string]any{"type": "summary_text", "text": text}},
		"content":  []any{},
	}
	if c := reasoningCarrier(text); c != "" {
		item["encrypted_content"] = c
	}
	return item
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
			if u, ok := p["image_url"].(string); ok && u != "" {
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		}
	}
	if len(out) == 0 {
		return textFromContent(raw)
	}
	return out
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

// responsesToChat 把 Responses 请求体翻译成 Chat Completions 请求体。
func responsesToChat(body []byte) ([]byte, *responsesRequest, error) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, fmt.Errorf("parse responses request: %w", err)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, nil, fmt.Errorf("missing required field: model")
	}
	chat := map[string]any{"model": req.Model}
	messages := make([]any, 0, 8)

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
				args, _ := it["arguments"].(string)
				pendingCalls = append(pendingCalls, map[string]any{
					"id": callID, "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				})
			case "function_call_output":
				flushAssistant()
				callID, _ := it["call_id"].(string)
				messages = append(messages, map[string]any{
					"role": "tool", "tool_call_id": callID, "content": stringifyOutput(it["output"]),
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

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, raw := range req.Tools {
			var t map[string]any
			if json.Unmarshal(raw, &t) != nil {
				continue
			}
			if typ, _ := t["type"].(string); typ != "function" && typ != "" {
				continue // 网关只支持 function 类工具
			}
			fn := map[string]any{}
			for _, k := range []string{"name", "description", "parameters", "strict"} {
				if v, ok := t[k]; ok {
					fn[k] = v
				}
			}
			if fn["name"] == nil {
				continue
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
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
	chatBody, meta, err := responsesToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 复用 chatCompletions：替换 body，保持同一请求上下文（断连取消）。
	inner := r.Clone(r.Context())
	inner.Body = io.NopCloser(bytes.NewReader(chatBody))
	inner.ContentLength = int64(len(chatBody))

	if meta.Stream {
		proxy := newResponsesStreamProxy(w, meta)
		h.chatCompletions(proxy, inner)
		proxy.finish()
		return
	}
	rec := newResponsesCapture()
	h.chatCompletions(rec, inner)
	responsesWriteNonStream(w, meta, rec)
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

// responsesWriteNonStream 把 Chat 聚合响应翻译成 Responses 响应。
func responsesWriteNonStream(w http.ResponseWriter, meta *responsesRequest, rec *responsesCapture) {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	if status >= 400 {
		writeOpenAIError(w, status, "upstream_error", strings.TrimSpace(rec.buf.String()))
		return
	}
	var chat map[string]any
	if err := json.Unmarshal(rec.buf.Bytes(), &chat); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	respID := responsesNewID("resp")
	var text strings.Builder
	var reasoning strings.Builder
	output := make([]any, 0, 4)
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
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
						output = append(output, map[string]any{
							"type": "function_call", "id": responsesNewID("fc"),
							"call_id": callID, "name": name, "arguments": args,
							"status": "completed",
						})
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
	env := responsesEnvelope(respID, meta.Model, "completed", output, usageToResponses(usage))
	env["output_text"] = text.String()
	writeJSON(w, http.StatusOK, env)
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
			// 首见即宣告 item（即使参数分片尚未到达），避免无参工具丢失。
			if !call.added {
				call.added = true
				call.id = responsesNewID("fc")
				if call.callID == "" {
					call.callID = responsesNewID("call")
				}
				call.index = p.allocIndex()
				p.emit("response.output_item.added", map[string]any{
					"output_index": call.index,
					"item": map[string]any{
						"type": "function_call", "id": call.id, "call_id": call.callID,
						"name": call.name, "arguments": "", "status": "in_progress",
					},
				})
			}
			if v, ok := fn["arguments"].(string); ok && v != "" {
				call.args.WriteString(v)
				p.emit("response.function_call_arguments.delta", map[string]any{
					"item_id": call.id, "output_index": call.index, "delta": v,
				})
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
			"response": map[string]any{
				"id": p.respID, "object": "response", "status": "failed",
				"model": p.meta.Model, "output": []any{},
				"error": map[string]any{"code": "server_error", "message": msg},
			},
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
			p.emit("response.output_item.added", map[string]any{
				"output_index": call.index,
				"item": map[string]any{
					"type": "function_call", "id": call.id, "call_id": call.callID,
					"name": call.name, "arguments": "", "status": "in_progress",
				},
			})
		}
		args := call.args.String()
		p.emit("response.function_call_arguments.done", map[string]any{
			"item_id": call.id, "output_index": call.index, "arguments": args,
		})
		item := map[string]any{
			"type": "function_call", "id": call.id, "call_id": call.callID,
			"name": call.name, "arguments": args, "status": "completed",
		}
		p.emit("response.output_item.done", map[string]any{"output_index": call.index, "item": item})
		output = append(output, item)
	}
	if len(output) == 0 {
		item := responsesMessageItem(responsesNewID("msg"), "", "completed")
		output = append(output, item)
	}
	env := responsesEnvelope(p.respID, p.meta.Model, "completed", output, usageToResponses(p.usage))
	p.emit("response.completed", map[string]any{"response": env})
}

// responsesGet 网关不保存会话状态；返回 404 由客户端按 unsupported 处理。
func (h *Handler) responsesGet(w http.ResponseWriter, r *http.Request) {
	writeOpenAIError(w, http.StatusNotFound, "not_found", "response state is not stored by this gateway")
}
