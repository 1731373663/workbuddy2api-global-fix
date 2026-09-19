// anthropic.go Anthropic Messages API compatibility for Claude Code.
//
// Claude Code speaks POST /v1/messages, while the upstream only understands
// Chat Completions. This adapter translates requests, tool calls, streaming
// events, and errors in both directions while reusing the existing account
// rotation, retry, metrics, and upstream protection path.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const anthropicVersion = "2023-06-01"

type anthropicRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []anthropicMsg  `json:"messages"`
	Tools         []anthropicTool `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Metadata      map[string]any  `json:"metadata"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func anthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

func anthropicMaxTokens(v int) int {
	if v <= 0 {
		return 4096
	}
	return v
}

func anthropicModelName(model string) string {
	if model == "" {
		return "cn:deepseek-v4.1-flash"
	}
	return model
}

func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		if t, _ := block["type"].(string); t == "text" {
			if text, _ := block["text"].(string); text != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(text)
			}
		}
	}
	return b.String()
}

func anthropicContentToChat(raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]any, 0, len(blocks))
	var text strings.Builder
	for _, block := range blocks {
		typ, _ := block["type"].(string)
		switch typ {
		case "text":
			if v, _ := block["text"].(string); v != "" {
				text.WriteString(v)
				parts = append(parts, map[string]any{"type": "text", "text": v})
			}
		case "image":
			if source, ok := block["source"].(map[string]any); ok {
				if data, _ := source["data"].(string); data != "" {
					media, _ := source["media_type"].(string)
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + media + ";base64," + data}})
				}
			}
		case "tool_use":
			// Tool-use blocks in assistant history are represented as Chat tool_calls.
			// They are handled by the message converter, not here.
		}
	}
	if len(parts) == 1 {
		if text.Len() > 0 {
			return text.String()
		}
	}
	if len(parts) > 0 {
		return parts
	}
	return text.String()
}

func rawJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	raw, _ := json.Marshal(v)
	return raw
}

func anthropicToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, block := range blocks {
		if t, _ := block["type"].(string); t == "text" {
			if v, _ := block["text"].(string); v != "" {
				b.WriteString(v)
			}
		}
	}
	return b.String()
}

func anthropicToChat(body []byte) ([]byte, *anthropicRequest, error) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, fmt.Errorf("parse messages request: %w", err)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, nil, fmt.Errorf("missing required field: model")
	}
	chat := map[string]any{"model": anthropicModelName(req.Model), "max_tokens": anthropicMaxTokens(req.MaxTokens), "stream": req.Stream}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		chat["stop"] = req.StopSequences
	}
	messages := make([]any, 0, len(req.Messages)+1)
	if sys := anthropicSystemText(req.System); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}
	for _, msg := range req.Messages {
		role := msg.Role
		if role == "assistant" {
			var blocks []map[string]any
			_ = json.Unmarshal(msg.Content, &blocks)
			var text strings.Builder
			calls := make([]any, 0)
			for _, block := range blocks {
				switch block["type"] {
				case "text":
					if v, _ := block["text"].(string); v != "" {
						text.WriteString(v)
					}
				case "tool_use":
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					input, _ := json.Marshal(block["input"])
					calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(input)}})
				}
			}
			out := map[string]any{"role": "assistant"}
			if text.Len() > 0 {
				out["content"] = text.String()
			} else {
				out["content"] = nil
			}
			if len(calls) > 0 {
				out["tool_calls"] = calls
			}
			messages = append(messages, out)
			continue
		}
		// User messages may contain tool_result blocks, which must become tool messages.
		var blocks []map[string]any
		if json.Unmarshal(msg.Content, &blocks) == nil {
			hasToolResult := false
			for _, block := range blocks {
				if t, _ := block["type"].(string); t == "tool_result" {
					hasToolResult = true
					id, _ := block["tool_use_id"].(string)
					messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": anthropicToolResultContent(rawJSON(block["content"]))})
				}
			}
			if hasToolResult {
				continue
			}
		}
		messages = append(messages, map[string]any{"role": role, "content": anthropicContentToChat(msg.Content)})
	}
	chat["messages"] = messages
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			schema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
			if len(tool.InputSchema) > 0 {
				schema = tool.InputSchema
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": schema}})
		}
		chat["tools"] = tools
	}
	if len(req.ToolChoice) > 0 {
		var tc map[string]any
		if json.Unmarshal(req.ToolChoice, &tc) == nil {
			switch tc["type"] {
			case "auto":
				chat["tool_choice"] = "auto"
			case "any":
				chat["tool_choice"] = "required"
			case "tool":
				if name, _ := tc["name"].(string); name != "" {
					chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
				}
			case "none":
				chat["tool_choice"] = "none"
			}
		}
	}
	out, err := json.Marshal(chat)
	return out, &req, err
}

func anthropicUsage(u *UsageDetail) map[string]any {
	if u == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	return map[string]any{"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens}
}

func anthropicStopReason(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func anthropicNonStream(w http.ResponseWriter, meta *anthropicRequest, rec *responsesCapture) {
	if rec.status >= 400 {
		anthropicError(w, rec.status, "api_error", strings.TrimSpace(rec.buf.String()))
		return
	}
	var chat map[string]any
	if err := json.Unmarshal(rec.buf.Bytes(), &chat); err != nil {
		anthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	var text strings.Builder
	var reasoning strings.Builder
	finish := "stop"
	content := make([]any, 0, 4)
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			finish, _ = c["finish_reason"].(string)
			if msg, ok := c["message"].(map[string]any); ok {
				if v, _ := msg["reasoning_content"].(string); v != "" {
					reasoning.WriteString(v)
				}
				if v, _ := msg["content"].(string); v != "" {
					text.WriteString(v)
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, raw := range tcs {
						tc, _ := raw.(map[string]any)
						fn, _ := tc["function"].(map[string]any)
						id, _ := tc["id"].(string)
						name, _ := fn["name"].(string)
						var input any = map[string]any{}
						if args, _ := fn["arguments"].(string); args != "" {
							_ = json.Unmarshal([]byte(args), &input)
						}
						content = append(content, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
					}
				}
			}
		}
	}
	if reasoning.Len() > 0 {
		content = append([]any{map[string]any{"type": "thinking", "thinking": reasoning.String()}}, content...)
	}
	if text.Len() > 0 {
		content = append(content, map[string]any{"type": "text", "text": text.String()})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	var usage *UsageDetail
	if u, ok := chat["usage"].(map[string]any); ok {
		usage = ParseUsage(u)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "msg_" + responsesNewID("anthropic"), "type": "message", "role": "assistant",
		"model": meta.Model, "content": content, "stop_reason": anthropicStopReason(finish),
		"stop_sequence": nil, "usage": anthropicUsage(usage),
	})
}

type anthropicStreamProxy struct {
	dest      http.ResponseWriter
	fl        http.Flusher
	meta      *anthropicRequest
	buf       bytes.Buffer
	status    int
	started   bool
	textOpen  bool
	textIndex int
	text      strings.Builder
	callIndex map[int]int
	callOrder []int
	calls     map[int]*responsesCall
	usage     *UsageDetail
	finish    string
}

func newAnthropicStreamProxy(dest http.ResponseWriter, meta *anthropicRequest) *anthropicStreamProxy {
	fl, _ := dest.(http.Flusher)
	return &anthropicStreamProxy{dest: dest, fl: fl, meta: meta, calls: map[int]*responsesCall{}, callIndex: map[int]int{}}
}
func (p *anthropicStreamProxy) Header() http.Header { return p.dest.Header() }
func (p *anthropicStreamProxy) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}
func (p *anthropicStreamProxy) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	p.buf.Write(b)
	p.drain()
	return len(b), nil
}
func (p *anthropicStreamProxy) Flush() {
	if p.fl != nil {
		p.fl.Flush()
	}
}
func (p *anthropicStreamProxy) emit(event string, data map[string]any) {
	raw, _ := json.Marshal(data)
	_, _ = io.WriteString(p.dest, "event: "+event+"\ndata: "+string(raw)+"\n\n")
	p.Flush()
}
func (p *anthropicStreamProxy) drain() {
	for {
		raw := p.buf.String()
		idx := strings.Index(raw, "\n\n")
		if idx < 0 {
			return
		}
		frame := strings.TrimSpace(raw[:idx])
		p.buf.Next(idx + 2)
		if !strings.HasPrefix(frame, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(frame, "data: ")
		if payload == "[DONE]" {
			return
		}
		p.handle(payload)
	}
}
func (p *anthropicStreamProxy) start() {
	if p.started {
		return
	}
	p.started = true
	h := p.dest.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	p.emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_" + responsesNewID("anthropic"), "type": "message", "role": "assistant", "model": p.meta.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
}
func (p *anthropicStreamProxy) openText() {
	if p.textOpen {
		return
	}
	p.start()
	p.textOpen = true
	p.textIndex = len(p.callOrder) + 1
	p.emit("content_block_start", map[string]any{"type": "content_block_start", "index": p.textIndex, "content_block": map[string]any{"type": "text", "text": ""}})
}
func (p *anthropicStreamProxy) handle(payload string) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
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
	if fr, _ := c["finish_reason"].(string); fr != "" {
		p.finish = fr
	}
	delta, _ := c["delta"].(map[string]any)
	if delta == nil {
		return
	}
	if v, _ := delta["content"].(string); v != "" {
		p.openText()
		p.text.WriteString(v)
		p.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": p.textIndex, "delta": map[string]any{"type": "text_delta", "text": v}})
	}
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range tcs {
			tc, _ := raw.(map[string]any)
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
				p.callIndex[idx] = len(p.callOrder)
			}
			fn, _ := tc["function"].(map[string]any)
			if id, _ := tc["id"].(string); id != "" {
				call.callID = id
			}
			if name, _ := fn["name"].(string); name != "" {
				call.name = name
			}
			if !call.added && call.name != "" {
				call.added = true
				p.start()
				p.emit("content_block_start", map[string]any{"type": "content_block_start", "index": p.callIndex[idx], "content_block": map[string]any{"type": "tool_use", "id": call.callID, "name": call.name, "input": map[string]any{}}})
			}
			if args, _ := fn["arguments"].(string); args != "" {
				call.args.WriteString(args)
				p.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": p.callIndex[idx], "delta": map[string]any{"type": "input_json_delta", "partial_json": args}})
			}
		}
	}
}
func (p *anthropicStreamProxy) finishStream() {
	if p.status >= 400 {
		return
	}
	p.start()
	if p.textOpen {
		p.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": p.textIndex})
	}
	for _, idx := range p.callOrder {
		call := p.calls[idx]
		if call != nil && call.added {
			p.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": p.callIndex[idx]})
		}
	}
	p.emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": anthropicStopReason(p.finish), "stop_sequence": nil}, "usage": anthropicUsage(p.usage)})
	p.emit("message_stop", map[string]any{"type": "message_stop"})
}

func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if int64(len(body)) > limit {
		anthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
		return
	}
	chatBody, meta, err := anthropicToChat(body)
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	inner := r.Clone(r.Context())
	inner.Body = io.NopCloser(bytes.NewReader(chatBody))
	inner.ContentLength = int64(len(chatBody))
	if meta.Stream {
		proxy := newAnthropicStreamProxy(w, meta)
		h.chatCompletions(proxy, inner)
		proxy.finishStream()
		return
	}
	rec := newResponsesCapture()
	h.chatCompletions(rec, inner)
	anthropicNonStream(w, meta, rec)
}

func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, h.cfg.MaxBodyBytes+1))
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var req anthropicRequest
	if json.Unmarshal(body, &req) != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid request")
		return
	}
	n := 0
	for _, m := range req.Messages {
		n += len(string(m.Content))/4 + 4
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": n})
}

var _ = time.Now
