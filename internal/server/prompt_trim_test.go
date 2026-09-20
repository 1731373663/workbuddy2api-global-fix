package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestTrimChatPromptDropsOldestTurn(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"system"},{"role":"user","content":"` + strings.Repeat("x", 40000) + `"},{"role":"assistant","content":"old answer"},{"role":"user","content":"keep this turn"},{"role":"assistant","content":"keep this answer"}]}`)
	out, ok := trimChatPrompt(body, `{"code":11115,"msg":"prompt is too long: 1050976 tokens > 1048576 maximum"}`)
	if !ok {
		t.Fatal("trimChatPrompt returned false")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("trimmed body is not JSON: %v", err)
	}
	messages, _ := obj["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages=%d want 3 after removing oldest turn", len(messages))
	}
	if messageRole(messages[0]) != "system" {
		t.Fatalf("first message role=%q want system", messageRole(messages[0]))
	}
	if content, _ := messages[1].(map[string]any)["content"].(string); content != "keep this turn" {
		t.Fatalf("kept user content=%q want newest turn", content)
	}
}

func TestTrimChatPromptRequiresAnotherUserTurn(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"system"},{"role":"user","content":"only turn"}]}`)
	if _, ok := trimChatPrompt(body, `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum"}`); ok {
		t.Fatal("trimChatPrompt must not remove the newest user turn")
	}
}

func TestChatPromptTooLongTrimsAndRetries(t *testing.T) {
	var calls int
	var firstLen, secondLen int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read upstream request: %v", err)
			}
			switch calls {
			case 1:
				firstLen = len(raw)
				return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":11115,"msg":"prompt is too long: 1050976 tokens > 1048576 maximum","requestId":"r"}`))}, nil
			case 2:
				secondLen = len(raw)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
			default:
				t.Fatalf("unexpected upstream call %d", calls)
				return nil, nil
			}
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	reqBody := `{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"system"},{"role":"user","content":"` + strings.Repeat("x", 40000) + `"},{"role":"assistant","content":"old answer"},{"role":"user","content":"keep this turn"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s want 200 after trim retry", rec.Code, rec.Body)
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2", calls)
	}
	if secondLen >= firstLen {
		t.Fatalf("trimmed request len=%d want less than original %d", secondLen, firstLen)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Fatalf("prompt trimming must not penalize the account: %+v", st)
	}
}
