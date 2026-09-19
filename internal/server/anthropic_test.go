package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicToChatMessagesAndTools(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-v4.1-flash",
		"max_tokens":128,
		"system":"sys",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"name":"get_weather","description":"weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
		"tool_choice":{"type":"auto"}
	}`)
	out, meta, err := anthropicToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model=%q", meta.Model)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "sys" {
		t.Fatalf("system=%v", sys)
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", tools)
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tool=%v", fn)
	}
}

func TestAnthropicToolResultRoundTrip(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-v4.1-flash","max_tokens":128,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Beijing"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"sunny"}]}]}
		]
	}`)
	out, _, err := anthropicToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", msgs)
	}
	asst, _ := msgs[0].(map[string]any)
	if _, ok := asst["tool_calls"].([]any); !ok {
		t.Fatalf("assistant=%v", asst)
	}
	tool, _ := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" || tool["content"] != "sunny" {
		t.Fatalf("tool=%v", tool)
	}
}

func TestAnthropicNonStreamResponse(t *testing.T) {
	rec := newResponsesCapture()
	rec.WriteHeader(200)
	_, _ = rec.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	dest := httptest.NewRecorder()
	anthropicNonStream(dest, &anthropicRequest{Model: "deepseek-v4.1-flash"}, rec)
	var env map[string]any
	if err := json.Unmarshal(dest.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["type"] != "message" || env["role"] != "assistant" {
		t.Fatalf("env=%v", env)
	}
	content, _ := env["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v", content)
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hello" {
		t.Fatalf("block=%v", block)
	}
}

func TestAnthropicCountTokensEndpoint(t *testing.T) {
	h := &Handler{cfg: Config{MaxBodyBytes: 1 << 20}}
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hello world"}]}`))
	rec := httptest.NewRecorder()
	h.anthropicCountTokens(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAnthropicAuthHeadersAndCompatiblePaths(t *testing.T) {
	h := NewHandler(Config{APIKey: "secret"})
	body := `{"model":"x","messages":[{"role":"user","content":"hello world"}]}`
	cases := []struct {
		name   string
		path   string
		header string
		value  string
		want   int
	}{
		{"x-api-key canonical", "/v1/messages/count_tokens", "x-api-key", "secret", 200},
		{"bearer canonical", "/v1/messages/count_tokens", "Authorization", "Bearer secret", 200},
		{"x-api-key duplicated v1", "/v1/v1/messages/count_tokens", "x-api-key", "secret", 200},
		{"bearer duplicated v1", "/v1/v1/messages/count_tokens", "Authorization", "Bearer secret", 200},
		{"wrong key", "/v1/messages/count_tokens", "Authorization", "Bearer wrong", 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", tc.path, strings.NewReader(body))
			req.Header.Set(tc.header, tc.value)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("code=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
