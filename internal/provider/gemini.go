package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"workbuddy2api/internal/geminiapi"
	"workbuddy2api/internal/geminiauth"
)

// ProtocolGeminiCodeAssist selects the Google Cloud Code Assist backend used by
// the official Gemini CLI. Credentials are a Google OAuth refresh token (stored
// in the provider api_key field), letting the gateway serve Gemini models from
// a Google AI Pro subscription without an AI Studio API key.
const ProtocolGeminiCodeAssist = "gemini_code_assist"

// geminiClient builds a Code Assist client from the provider refresh token.
func geminiClient(cfg Config) (*geminiapi.Client, error) {
	token := strings.TrimSpace(cfg.APIKey)
	if token == "" {
		return nil, fmt.Errorf("gemini provider %q has no refresh token configured", cfg.Name)
	}
	cred := &geminiauth.Credential{RefreshToken: token}
	if p := strings.TrimSpace(cfg.BaseURL); p != "" && p != "-" {
		cred.ProjectID = p
	}
	return geminiapi.New(cred), nil
}

// streamGemini translates an OpenAI chat request to the Code Assist API and
// returns the reply in OpenAI shape (SSE when stream is requested).
func streamGemini(ctx context.Context, cfg Config, model string, body []byte) (io.ReadCloser, int, []byte, error) {
	client, err := geminiClient(cfg)
	if err != nil {
		return nil, 0, nil, err
	}
	var req geminiapi.OpenAIChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, 0, nil, fmt.Errorf("parse chat request: %w", err)
	}
	if strings.TrimSpace(model) != "" {
		req.Model = model
	}

	if req.Stream {
		return geminiStreamReader(ctx, client, req), 200, nil, nil
	}

	text, usage, err := client.Generate(ctx, req)
	if err != nil {
		return nil, 502, []byte(err.Error()), nil
	}
	resp := map[string]any{
		"id":      "chatcmpl-gemini",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": usage,
	}
	raw, _ := json.Marshal(resp)
	return io.NopCloser(bytes.NewReader(raw)), 200, nil, nil
}

// geminiStreamReader runs the upstream stream and emits OpenAI SSE chunks via an
// io.Pipe so the caller can consume it incrementally.
func geminiStreamReader(ctx context.Context, client *geminiapi.Client, req geminiapi.OpenAIChatRequest) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		writeChunk := func(delta string, finish any) error {
			payload := map[string]any{
				"id":      "chatcmpl-gemini",
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   req.Model,
				"choices": []any{map[string]any{
					"index":         0,
					"delta":         map[string]any{"content": delta},
					"finish_reason": finish,
				}},
			}
			raw, _ := json.Marshal(payload)
			_, err := fmt.Fprintf(pw, "data: %s\n\n", raw)
			return err
		}
		if err := client.GenerateStream(ctx, req, func(c geminiapi.StreamChunk) error {
			return writeChunk(c.Text, nil)
		}); err != nil {
			_, _ = fmt.Fprintf(pw, "data: {\"error\":%q}\n\n", err.Error())
			return
		}
		_ = writeChunk("", "stop")
		_, _ = io.WriteString(pw, "data: [DONE]\n\n")
	}()
	return pr
}

// listGeminiModels returns the model catalogue for a Gemini provider.
func listGeminiModels(cfg Config) []Model {
	client, err := geminiClient(cfg)
	if err != nil {
		return nil
	}
	src := client.Models()
	out := make([]Model, 0, len(src))
	for _, m := range src {
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		out = append(out, Model{ID: m.ID, Name: name, OwnedBy: "google"})
	}
	return out
}
