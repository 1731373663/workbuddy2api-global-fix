package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListModelsAndChatPassthrough(t *testing.T) {
	var chatModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if got := r.Header.Get("Authorization"); got != "Bearer key" {
				t.Fatalf("authorization=%q", got)
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"m1","name":"Model One"},{"id":"m2"}]}`)
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			var obj map[string]any
			_ = json.Unmarshal(body, &obj)
			chatModel, _ = obj["model"].(string)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New([]Config{{Name: "demo", BaseURL: srv.URL + "/v1", APIKey: "key"}})
	models := c.ListModels(context.Background())
	if len(models["demo"]) != 2 || models["demo"][0].ID != "m1" {
		t.Fatalf("models=%+v", models)
	}
	rc, status, _, err := c.Stream(context.Background(), "demo", "m2", []byte(`{"model":"demo:m2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("stream status=%d err=%v", status, err)
	}
	_, _ = io.ReadAll(rc)
	rc.Close()
	if chatModel != "m2" {
		t.Fatalf("upstream model=%q", chatModel)
	}
}

func TestResponsesProtocolConversion(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","model":"upstream-model","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}`)
	}))
	defer srv.Close()

	c := New([]Config{{Name: "resp", BaseURL: srv.URL + "/v1", Protocol: "responses"}})
	rc, status, _, err := c.Stream(context.Background(), "resp", "upstream-model", []byte(`{"model":"resp:upstream-model","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"stream":false}`))
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()
	if got["model"] != "upstream-model" || got["instructions"] != "sys" {
		t.Fatalf("converted request=%+v", got)
	}
	if !strings.Contains(string(raw), `"content":"hello"`) {
		t.Fatalf("converted response=%s", raw)
	}
}
