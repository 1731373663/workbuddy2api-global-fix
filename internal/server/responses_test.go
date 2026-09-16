package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestResponsesToChatBasic 字符串 input + instructions + stream 的翻译。
func TestResponsesToChatBasic(t *testing.T) {
	body := []byte(`{
		"model":"cn:deepseek-v4.1-flash",
		"instructions":"be brief",
		"input":"hello",
		"stream":true
	}`)
	out, meta, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if !meta.Stream {
		t.Error("stream should be true")
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if chat["model"] != "cn:deepseek-v4.1-flash" {
		t.Errorf("model=%v", chat["model"])
	}
	if chat["stream"] != true {
		t.Errorf("stream=%v", chat["stream"])
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len=%d want 2: %v", len(msgs), msgs)
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "be brief" {
		t.Errorf("system message wrong: %v", m0)
	}
	m1, _ := msgs[1].(map[string]any)
	if m1["role"] != "user" || m1["content"] != "hello" {
		t.Errorf("user message wrong: %v", m1)
	}
}

// TestResponsesToChatToolRoundTrip function_call / function_call_output 翻译成 Chat 工具消息。
func TestResponsesToChatToolRoundTrip(t *testing.T) {
	body := []byte(`{
		"model":"cn:deepseek-v4.1-flash",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"ls"}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"a.txt"}
		],
		"tools":[{"type":"function","name":"shell","description":"run","parameters":{"type":"object"}}],
		"tool_choice":"auto",
		"max_output_tokens":128
	}`)
	out, meta, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if meta.Stream {
		t.Error("stream should be false")
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len=%d want 3: %v", len(msgs), msgs)
	}
	callMsg, _ := msgs[1].(map[string]any)
	tcs, _ := callMsg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%v", callMsg["tool_calls"])
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "a.txt" {
		t.Errorf("tool message wrong: %v", toolMsg)
	}
	ct, _ := chat["tool_choice"].(string)
	if ct != "auto" {
		t.Errorf("tool_choice=%v", chat["tool_choice"])
	}
	if _, ok := chat["max_tokens"]; !ok {
		t.Error("max_output_tokens should map to max_tokens")
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", chat["tools"])
	}
}

// TestResponsesWriteNonStream Chat 聚合响应翻译成 Responses 响应。
func TestResponsesWriteNonStream(t *testing.T) {
	chat := map[string]any{
		"id": "chatcmpl-x", "model": "deepseek-v4.1-flash",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role": "assistant", "content": "hello world",
				"reasoning_content": "think",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7},
	}
	raw, _ := json.Marshal(chat)
	rec := newResponsesCapture()
	rec.WriteHeader(200)
	_, _ = rec.Write(raw)
	dest := httptest.NewRecorder()
	meta := &responsesRequest{Model: "cn:deepseek-v4.1-flash"}
	responsesWriteNonStream(dest, meta, rec)

	var env map[string]any
	if err := json.Unmarshal(dest.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal responses: %v", err)
	}
	if env["object"] != "response" || env["status"] != "completed" {
		t.Errorf("envelope wrong: %v", env)
	}
	if env["output_text"] != "hello world" {
		t.Errorf("output_text=%v", env["output_text"])
	}
	out, _ := env["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output len=%d want 2 (reasoning+message): %v", len(out), out)
	}
	usage, _ := env["usage"].(map[string]any)
	if usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(2) {
		t.Errorf("usage=%v", usage)
	}
}

// TestResponsesReasoningMergedIntoAssistant DeepSeek 思考模式：reasoning item 的
// 文本必须回填到带 tool_calls 的 assistant 消息，避免 11155 reasoning_content_missing。
func TestResponsesReasoningMergedIntoAssistant(t *testing.T) {
	body := []byte(`{
		"model":"cn:deepseek-v4.1-flash",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"ls"}]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"I should run ls"}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"a.txt"}
		]
	}`)
	out, _, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len=%d want 3: %v", len(msgs), msgs)
	}
	asst, _ := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("msgs[1] should be assistant: %v", asst)
	}
	if asst["reasoning_content"] != "I should run ls" {
		t.Errorf("reasoning_content=%v want %q", asst["reasoning_content"], "I should run ls")
	}
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%v", asst["tool_calls"])
	}
	tool, _ := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "a.txt" {
		t.Errorf("tool message wrong: %v", tool)
	}
}

// TestResponsesParallelCallsMerged 同轮多个 function_call 合并进一条 assistant 消息，
// 结果紧随其后，避免 11148 tool_call_sequence_broken。
func TestResponsesParallelCallsMerged(t *testing.T) {
	body := []byte(`{
		"model":"cn:deepseek-v4.1-flash",
		"input":[
			{"type":"message","role":"user","content":"go"},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"plan"}]},
			{"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
			{"type":"function_call","call_id":"c2","name":"b","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"r1"},
			{"type":"function_call_output","call_id":"c2","output":"r2"}
		]
	}`)
	out, _, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len=%d want 4: %v", len(msgs), msgs)
	}
	asst, _ := msgs[1].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("assistant should carry 2 tool_calls: %v", asst)
	}
	for i, want := range []string{"c1", "c2"} {
		tc, _ := tcs[i].(map[string]any)
		if tc["id"] != want {
			t.Errorf("tool_calls[%d].id=%v want %s", i, tc["id"], want)
		}
	}
	for i, want := range []string{"c1", "c2"} {
		tool, _ := msgs[2+i].(map[string]any)
		if tool["role"] != "tool" || tool["tool_call_id"] != want {
			t.Errorf("msgs[%d] want tool %s: %v", 2+i, want, tool)
		}
	}
}
