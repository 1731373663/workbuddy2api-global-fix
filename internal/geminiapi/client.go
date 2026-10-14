// Package geminiapi implements a client for Google's Cloud Code Assist API
// (cloudcode-pa.googleapis.com/v1internal), the backend used by the official
// Gemini CLI. It lets the gateway serve Gemini models from a Google AI Pro
// subscription without an AI Studio API key.
//
// It also translates between the OpenAI Chat Completions shape used by the
// gateway and the Code Assist generateContent shape.
package geminiapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/geminiauth"
)

const (
	// Endpoint / APIVersion match gemini-cli's CodeAssistServer constants.
	Endpoint   = "https://cloudcode-pa.googleapis.com"
	APIVersion = "v1internal"

	// DefaultModel is used when a request omits the model.
	DefaultModel = "gemini-2.5-pro"
)

// Client calls the Cloud Code Assist API with one Google credential.
type Client struct {
	cred *geminiauth.Credential
	http *http.Client
}

// New builds a client around a credential.
func New(cred *geminiauth.Credential) *Client {
	return &Client{
		cred: cred,
		http: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          64,
				MaxIdleConnsPerHost:   16,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
			},
		},
	}
}

// Credential exposes the underlying credential (for email/project display).
func (c *Client) Credential() *geminiauth.Credential { return c.cred }

// do sends an authenticated POST to <endpoint>/v1internal:<method>.
func (c *Client) do(ctx context.Context, method string, body any) (*http.Response, error) {
	tok, err := c.cred.Token(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/%s:%s", Endpoint, APIVersion, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", "google-cloud-sdk vscode_cloudshelleditor/0.1")
	req.Header.Set("X-Goog-Api-Client", "gl-node/22.17.0")
	req.Header.Set("Client-Metadata", clientMetadata())
	return c.http.Do(req)
}

// clientMetadata is the stable ideType/platform blob gemini-cli sends so the
// backend treats this as a first-party CLI client.
func clientMetadata() string {
	m := map[string]string{
		"ideType":    "IDE_UNSPECIFIED",
		"platform":   "PLATFORM_UNSPECIFIED",
		"pluginType": "GEMINI",
	}
	raw, _ := json.Marshal(m)
	return string(raw)
}

// loadCodeAssistResponse carries the fields we care about from loadCodeAssist.
type loadCodeAssistResponse struct {
	CurrentTier *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"currentTier"`
	CloudAiCompanionProject any `json:"cloudaicompanionProject"`
}

// EnsureProject resolves and caches the Cloud Code project id for the account.
// An empty project means the backend picks the account default.
func (c *Client) EnsureProject(ctx context.Context) (string, string, error) {
	c.cred.Lock()
	project := c.cred.ProjectID
	tier := c.cred.Tier
	c.cred.Unlock()
	if project != "" || tier != "" {
		return project, tier, nil
	}

	body := map[string]any{
		"metadata": map[string]string{
			"ideType":    "IDE_UNSPECIFIED",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	}
	resp, err := c.do(ctx, "loadCodeAssist", body)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("loadCodeAssist HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out loadCodeAssistResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("parse loadCodeAssist: %w", err)
	}
	if out.CurrentTier != nil {
		tier = out.CurrentTier.ID
	}
	if s, ok := out.CloudAiCompanionProject.(string); ok {
		project = s
	}
	c.cred.Lock()
	c.cred.ProjectID = project
	c.cred.Tier = tier
	c.cred.Unlock()
	return project, tier, nil
}

// Model is one model this account can use.
type Model struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}

// staticModels is the fallback catalogue; the Code Assist API does not expose a
// public list-models method, so these mirror the models the CLI otherwise sees.
var staticModels = []Model{
	{ID: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro"},
	{ID: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash"},
	{ID: "gemini-2.5-flash-lite", DisplayName: "Gemini 2.5 Flash Lite"},
	{ID: "gemini-3-pro-preview", DisplayName: "Gemini 3 Pro (Preview)"},
}

// Models returns the model catalogue exposed to clients.
func (c *Client) Models() []Model {
	return append([]Model(nil), staticModels...)
}

// OpenAIChatRequest is the subset of the OpenAI request we translate from.
type OpenAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []OpenAIMessage `json:"messages"`
	Tools       []any           `json:"tools,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

// OpenAIMessage is one chat message.
type OpenAIMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// BuildRequest converts an OpenAI chat request into a Code Assist envelope.
func BuildRequest(in OpenAIChatRequest, project string) map[string]any {
	contents := make([]map[string]any, 0, len(in.Messages))
	var systemText strings.Builder
	for _, m := range in.Messages {
		text := contentToText(m.Content)
		switch m.Role {
		case "system", "developer":
			if text != "" {
				if systemText.Len() > 0 {
					systemText.WriteString("\n\n")
				}
				systemText.WriteString(text)
			}
		case "assistant":
			contents = append(contents, map[string]any{
				"role":  "model",
				"parts": []map[string]any{{"text": text}},
			})
		default: // user / tool
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": []map[string]any{{"text": text}},
			})
		}
	}
	inner := map[string]any{"contents": contents}
	if systemText.Len() > 0 {
		inner["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": systemText.String()}},
		}
	}
	gen := map[string]any{}
	if in.Temperature != nil {
		gen["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		gen["topP"] = *in.TopP
	}
	if in.MaxTokens != nil {
		gen["maxOutputTokens"] = *in.MaxTokens
	}
	if len(gen) > 0 {
		inner["generationConfig"] = gen
	}
	if len(in.Tools) > 0 {
		inner["tools"] = in.Tools
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = DefaultModel
	}
	return map[string]any{
		"model":          model,
		"project":        project,
		"user_prompt_id": "gateway",
		"request":        inner,
	}
}

// contentToText flattens OpenAI message content (string or part array) to text.
func contentToText(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := m["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	case nil:
		return ""
	default:
		blob, _ := json.Marshal(v)
		return string(blob)
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
