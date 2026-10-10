// Package provider implements direct passthrough to third-party OpenAI-compatible APIs.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config is one third-party OpenAI-compatible provider.
type Config struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	ModelsURL string `json:"models_url,omitempty"`
	// Protocol selects the upstream API shape: chat_completions (default) or responses.
	Protocol string `json:"protocol,omitempty"`
}

// Model is one model exposed by a provider.
type Model struct {
	ID      string
	Name    string
	OwnedBy string
}

// Client forwards chat requests to configured third-party providers.
type Client struct {
	providers map[string]Config
	http      *http.Client
}

// New builds a provider client. Invalid entries are skipped.
func New(configs []Config) *Client {
	c := &Client{
		providers: make(map[string]Config, len(configs)),
		http: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          64,
				MaxIdleConnsPerHost:   16,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
			},
		},
	}
	for _, cfg := range configs {
		name := strings.TrimSpace(cfg.Name)
		base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		if name == "" || base == "" {
			continue
		}
		cfg.Name = name
		cfg.BaseURL = base
		cfg.Protocol = normalizeProtocol(cfg.Protocol)
		c.providers[name] = cfg
	}
	return c
}

// Enabled reports whether at least one provider is configured.
func (c *Client) Enabled() bool { return c != nil && len(c.providers) > 0 }

// Has reports whether name is a configured provider.
func (c *Client) Has(name string) bool {
	if c == nil {
		return false
	}
	_, ok := c.providers[strings.TrimSpace(name)]
	return ok
}

// Names returns configured provider names.
func (c *Client) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.providers))
	for name := range c.providers {
		out = append(out, name)
	}
	return out
}

// ListModels fetches models from all configured providers. A failing provider is
// skipped so one bad supplier cannot hide the others.
func (c *Client) ListModels(ctx context.Context) map[string][]Model {
	out := make(map[string][]Model, len(c.providers))
	for name, cfg := range c.providers {
		models, err := c.listModels(ctx, cfg)
		if err == nil && len(models) > 0 {
			out[name] = models
		}
	}
	return out
}

// ListProviderModels fetches models from one provider.
func (c *Client) ListProviderModels(ctx context.Context, name string) ([]Model, error) {
	if c == nil {
		return nil, fmt.Errorf("provider client disabled")
	}
	cfg, ok := c.providers[strings.TrimSpace(name)]
	if !ok {
		return nil, fmt.Errorf("provider %q not found", name)
	}
	return c.listModels(ctx, cfg)
}

func (c *Client) listModels(ctx context.Context, cfg Config) ([]Model, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.ModelsURL), "/")
	if endpoint == "<nil>" || strings.EqualFold(endpoint, "null") {
		endpoint = ""
	}
	if endpoint == "" {
		endpoint = cfg.BaseURL + "/models"
	}
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return nil, fmt.Errorf("invalid models url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, cfg)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var env struct {
		Data []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
		Models []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			OwnedBy string `json:"owned_by"`
		} `json:"models"`
	}
	// Normalize the optional models URL. Some GUI/config writers emit "<nil>" for an absent value.
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	items := env.Data
	if len(items) == 0 {
		items = env.Models
	}
	out := make([]Model, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = id
		}
		out = append(out, Model{ID: id, Name: name, OwnedBy: strings.TrimSpace(item.OwnedBy)})
	}
	return out, nil
}

// Stream forwards a chat completion request and returns the upstream body.
// The upstream protocol is selected per provider: chat_completions or responses.
func (c *Client) Stream(ctx context.Context, providerName, model string, body []byte) (io.ReadCloser, int, []byte, error) {
	if c == nil {
		return nil, 0, nil, fmt.Errorf("provider client disabled")
	}
	cfg, ok := c.providers[strings.TrimSpace(providerName)]
	if !ok {
		return nil, 0, nil, fmt.Errorf("provider %q not found", providerName)
	}
	protocol := normalizeProtocol(cfg.Protocol)
	endpoint := cfg.BaseURL + "/chat/completions"
	requestBody := body
	if protocol == "responses" {
		endpoint = cfg.BaseURL + "/responses"
		converted, err := chatToResponses(body, model)
		if err != nil {
			return nil, 0, nil, err
		}
		requestBody = converted
	} else {
		rewritten, err := rewriteModel(body, model)
		if err != nil {
			return nil, 0, nil, err
		}
		requestBody = rewritten
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return nil, 0, nil, err
	}
	c.setHeaders(req, cfg)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	if protocol == "responses" {
		if isEventStream(resp.Header.Get("Content-Type")) {
			return newResponsesToChatReader(resp.Body, model), resp.StatusCode, nil, nil
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, resp.StatusCode, nil, readErr
		}
		converted, convErr := responsesToChat(raw, model)
		if convErr != nil {
			return nil, resp.StatusCode, raw, convErr
		}
		return io.NopCloser(bytes.NewReader(converted)), resp.StatusCode, nil, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func chatToResponses(body []byte, model string) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse chat request: %w", err)
	}
	out := map[string]any{"model": model}
	if v, ok := in["stream"]; ok {
		out["stream"] = v
	}
	if v, ok := in["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := in["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := in["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	if v, ok := in["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	if v, ok := in["instructions"]; ok {
		out["instructions"] = v
	}
	if msgs, ok := in["messages"].([]any); ok {
		var input []any
		var instructions []string
		for _, raw := range msgs {
			m, _ := raw.(map[string]any)
			if m == nil {
				continue
			}
			role, _ := m["role"].(string)
			if role == "system" || role == "developer" {
				if text := contentText(m["content"]); text != "" {
					instructions = append(instructions, text)
				}
				continue
			}
			item := map[string]any{"role": role, "content": responsesContent(m["content"])}
			if role == "tool" {
				item["type"] = "function_call_output"
				item["call_id"] = m["tool_call_id"]
			}
			if calls, ok := m["tool_calls"].([]any); ok && len(calls) > 0 {
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					if call == nil {
						continue
					}
					fn, _ := call["function"].(map[string]any)
					input = append(input, map[string]any{
						"type": "function_call", "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"],
					})
				}
			}
			input = append(input, item)
		}
		if len(instructions) > 0 {
			out["instructions"] = strings.Join(instructions, "\n\n")
		}
		out["input"] = input
	}
	if tools, ok := in["tools"]; ok {
		out["tools"] = chatToolsToResponses(tools)
	}
	if v, ok := in["tool_choice"]; ok {
		out["tool_choice"] = v
	}
	return json.Marshal(out)
}

func chatToolsToResponses(raw any) any {
	tools, ok := raw.([]any)
	if !ok {
		return raw
	}
	out := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		if tool == nil {
			continue
		}
		if typ, _ := tool["type"].(string); typ == "function" {
			if fn, ok := tool["function"].(map[string]any); ok {
				out = append(out, map[string]any{"type": "function", "name": fn["name"], "description": fn["description"], "parameters": fn["parameters"]})
				continue
			}
		}
		out = append(out, tool)
	}
	return out
}

func responsesContent(raw any) any {
	switch v := raw.(type) {
	case string:
		return []any{map[string]any{"type": "input_text", "text": v}}
	case []any:
		out := make([]any, 0, len(v))
		for _, partRaw := range v {
			part, _ := partRaw.(map[string]any)
			if part == nil {
				continue
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "text":
				out = append(out, map[string]any{"type": "input_text", "text": part["text"]})
			case "image_url":
				out = append(out, map[string]any{"type": "input_image", "image_url": part["image_url"]})
			default:
				out = append(out, part)
			}
		}
		return out
	default:
		return raw
	}
}

func contentText(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, partRaw := range v {
			part, _ := partRaw.(map[string]any)
			if part == nil {
				continue
			}
			if text, ok := part["text"].(string); ok {
				b.WriteString(text)
			}
		}
		return b.String()
	default:
		return ""
	}
}

type responsesToChatReader struct {
	src     io.ReadCloser
	rd      *bufio.Reader
	model   string
	done    bool
	buf     bytes.Buffer
	id      string
	created int64
}

func newResponsesToChatReader(src io.ReadCloser, model string) io.ReadCloser {
	return &responsesToChatReader{src: src, rd: bufio.NewReaderSize(src, 64*1024), created: time.Now().Unix(), id: fmt.Sprintf("chatcmpl-provider-%d", time.Now().UnixNano()), model: model}
}

func (r *responsesToChatReader) Read(p []byte) (int, error) {
	for r.buf.Len() == 0 && !r.done {
		line, err := r.rd.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				r.done = true
				break
			}
			var ev struct {
				Type   string         `json:"type"`
				Delta  string         `json:"delta"`
				Text   string         `json:"text"`
				ItemID string         `json:"item_id"`
				Name   string         `json:"name"`
				CallID string         `json:"call_id"`
				Args   string         `json:"arguments"`
				Usage  map[string]any `json:"usage"`
			}
			if json.Unmarshal([]byte(payload), &ev) == nil {
				switch ev.Type {
				case "response.output_text.delta":
					r.writeDelta(ev.Delta, nil)
				case "response.function_call_arguments.delta":
					r.writeToolDelta(ev.ItemID, ev.Name, ev.CallID, ev.Args)
				case "response.output_text.done":
					if ev.Text != "" {
						r.writeDelta("", nil)
					}
				case "response.completed":
					r.writeUsage(ev.Usage)
					r.done = true
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				r.done = true
				break
			}
			return 0, err
		}
	}
	if r.done && r.buf.Len() == 0 {
		if r.src != nil {
			_ = r.src.Close()
		}
		return 0, io.EOF
	}
	return r.buf.Read(p)
}

func (r *responsesToChatReader) Close() error { return r.src.Close() }

func (r *responsesToChatReader) writeDelta(text string, usage map[string]any) {
	chunk := map[string]any{"id": r.id, "object": "chat.completion.chunk", "created": r.created, "model": r.model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}}}
	if usage != nil {
		chunk["usage"] = usage
	}
	raw, _ := json.Marshal(chunk)
	r.buf.WriteString("data: ")
	r.buf.Write(raw)
	r.buf.WriteString("\n\n")
}

func (r *responsesToChatReader) writeToolDelta(itemID, name, callID, args string) {
	call := map[string]any{"index": 0, "id": firstNonEmpty(callID, itemID), "type": "function", "function": map[string]any{"name": name, "arguments": args}}
	chunk := map[string]any{"id": r.id, "object": "chat.completion.chunk", "created": r.created, "model": r.model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}, "finish_reason": nil}}}
	raw, _ := json.Marshal(chunk)
	r.buf.WriteString("data: ")
	r.buf.Write(raw)
	r.buf.WriteString("\n\n")
}

func (r *responsesToChatReader) writeUsage(usage map[string]any) {
	if usage == nil {
		return
	}
	chunk := map[string]any{"id": r.id, "object": "chat.completion.chunk", "created": r.created, "model": r.model, "choices": []any{}, "usage": usage}
	raw, _ := json.Marshal(chunk)
	r.buf.WriteString("data: ")
	r.buf.Write(raw)
	r.buf.WriteString("\n\n")
}
func responsesToChat(raw []byte, model string) ([]byte, error) {
	var env struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
			Args    string `json:"arguments"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage map[string]any `json:"usage"`
	}
	// Normalize the optional models URL. Some GUI/config writers emit "<nil>" for an absent value.
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse responses response: %w", err)
	}
	var text strings.Builder
	var calls []any
	for _, item := range env.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" || part.Text != "" {
					text.WriteString(part.Text)
				}
			}
		case "function_call":
			calls = append(calls, map[string]any{"id": item.CallID, "type": "function", "function": map[string]any{"name": item.Name, "arguments": item.Args}})
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	resp := map[string]any{"id": env.ID, "object": "chat.completion", "model": firstNonEmpty(env.Model, model), "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}}}
	if env.Usage != nil {
		resp["usage"] = env.Usage
	}
	return json.Marshal(resp)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func normalizeProtocol(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "responses", "response", "responses_api", "openai_responses":
		return "responses"
	default:
		return "chat_completions"
	}
}

// Protocol returns the configured upstream protocol for a provider.
func (c *Client) Protocol(name string) string {
	if c == nil {
		return ""
	}
	cfg, ok := c.providers[strings.TrimSpace(name)]
	if !ok {
		return ""
	}
	return normalizeProtocol(cfg.Protocol)
}

func (c *Client) setHeaders(req *http.Request, cfg Config) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(cfg.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func rewriteModel(body []byte, model string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}
	obj["model"] = model
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
