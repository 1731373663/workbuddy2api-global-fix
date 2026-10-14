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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/geminiauth"
)

const (
	// fallbackProjectID is used when loadCodeAssist returns no project.
	fallbackProjectID = "rising-fact-p41fc"

	// Endpoint / APIVersion match the Cloud Code Assist service.
	Endpoint   = "https://daily-cloudcode-pa.googleapis.com"
	APIVersion = "v1internal"

	// DefaultModel is used when a request omits the model.
	DefaultModel = "gemini-3-flash"
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
				Proxy:                 geminiauth.LoopbackSafeProxy,
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
	req.Header.Set("User-Agent", "antigravity/1.11.5 windows/amd64")
	req.Header.Set("X-Client-Name", "antigravity")
	req.Header.Set("X-Client-Version", "1.11.5")
	req.Header.Set("X-Goog-Api-Client", "gl-node/18.18.2 fire/0.8.6 grpc/1.10.x")
	req.Header.Set("Client-Metadata", clientMetadata())
	return c.http.Do(req)
}

// clientMetadata is the stable ideType/platform blob gemini-cli sends so the
// backend treats this as a first-party CLI client.
func clientMetadata() string {
	m := map[string]any{
		"ideType":    9,
		"platform":   0,
		"pluginType": 2,
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
	AllowedTiers            []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		IsDefault bool   `json:"isDefault"`
	} `json:"allowedTiers"`
}

// EnsureProject resolves the Cloud Code project id for the account, onboarding
// the account when it has never used Code Assist before.
//
// A brand-new account has no currentTier until onboardUser runs; calling
// generateContent before that returns HTTP 403 "You do not have a valid
// license". Mirrors gemini-cli's setupUser flow.
func (c *Client) EnsureProject(ctx context.Context) (string, string, error) {
	c.cred.Lock()
	project := c.cred.ProjectID
	tier := c.cred.Tier
	c.cred.Unlock()
	if project != "" || tier != "" {
		return project, tier, nil
	}

	meta := map[string]any{
		"ideType":    9,
		"platform":   0,
		"pluginType": 2,
	}
	resp, err := c.do(ctx, "loadCodeAssist", map[string]any{
		"cloudaicompanionProject": "",
		"metadata":                meta,
	})
	if err != nil {
		return "", "", err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("loadCodeAssist HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	log.Printf("DBG gemini loadCodeAssist: %s", truncate(string(raw), 400))
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
	// Antigravity returns the managed project inline ("aicode-consumers" for
	// Google AI Pro accounts). Use it directly; no onboarding step is needed.
	if project == "" && out.CloudAiCompanionProject != nil {
		if m, ok := out.CloudAiCompanionProject.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				project = id
			}
		}
	}
	if project == "" {
		project = fallbackProjectID
	}
	log.Printf("DBG gemini resolved project=%q tier=%q", project, tier)
	if project != "" {
		c.cred.Lock()
		c.cred.ProjectID = project
		c.cred.Tier = tier
		c.cred.Unlock()
		return project, tier, nil
	}
	onboardTier := out.defaultTierID()
	if onboardTier == "" {
		return "", "", fmt.Errorf("Code Assist: account has no eligible tier")
	}
	onboardBody := map[string]any{
		"tierId":   onboardTier,
		"metadata": meta,
	}
	if onboardTier != "free-tier" {
		onboardBody["cloudaicompanionProject"] = project
	}
	oresp, err := c.do(ctx, "onboardUser", onboardBody)
	if err != nil {
		return "", "", err
	}
	oraw, _ := io.ReadAll(io.LimitReader(oresp.Body, 1<<20))
	oresp.Body.Close()
	if oresp.StatusCode < 200 || oresp.StatusCode >= 300 {
		return "", "", fmt.Errorf("onboardUser HTTP %d: %s", oresp.StatusCode, truncate(string(oraw), 300))
	}
	var ob struct {
		Done     bool `json:"done"`
		Response *struct {
			CloudAiCompanionProject *struct {
				ID string `json:"id"`
			} `json:"cloudaicompanionProject"`
		} `json:"response"`
	}
	if err := json.Unmarshal(oraw, &ob); err != nil {
		return "", "", fmt.Errorf("parse onboardUser: %w", err)
	}
	if ob.Response != nil && ob.Response.CloudAiCompanionProject != nil {
		project = ob.Response.CloudAiCompanionProject.ID
	}
	tier = onboardTier
	c.cred.Lock()
	c.cred.ProjectID = project
	c.cred.Tier = tier
	c.cred.Unlock()
	return project, tier, nil
}

// defaultTierID returns the tier the account should be onboarded into.
func (r loadCodeAssistResponse) defaultTierID() string {
	for _, t := range r.AllowedTiers {
		if t.IsDefault && t.ID != "" {
			return t.ID
		}
	}
	for _, t := range r.AllowedTiers {
		if t.ID != "" {
			return t.ID
		}
	}
	return ""
}

// Model is one model this account can use.
type Model struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	// ContextLength / MaxOutput describe the model window.
	ContextLength int64 `json:"context_length,omitempty"`
	MaxOutput     int64 `json:"max_output_tokens,omitempty"`
}

// staticModels is the fallback catalogue; the Code Assist API does not expose a
// public list-models method, so these mirror the models the CLI otherwise sees.
var staticModels = []Model{
	{ID: "gemini-3-flash", DisplayName: "Gemini 3 Flash", ContextLength: 1048576, MaxOutput: 65536},
	{ID: "gemini-3-pro-high", DisplayName: "Gemini 3 Pro (High)", ContextLength: 1048576, MaxOutput: 65536},
	{ID: "gemini-3-pro-low", DisplayName: "Gemini 3 Pro (Low)", ContextLength: 1048576, MaxOutput: 65536},
	{ID: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash", ContextLength: 1048576, MaxOutput: 65536},
	{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", ContextLength: 200000, MaxOutput: 64000},
	{ID: "claude-opus-4-6-thinking", DisplayName: "Claude Opus 4.6 (Thinking)", ContextLength: 200000, MaxOutput: 64000},
}

// FetchModels queries the live available-model list for the account. The static
// catalogue is only a fallback: Google renames and retires model ids, so the
// authoritative list must come from the API.
func (c *Client) FetchModels(ctx context.Context) ([]Model, error) {
	project, _, err := c.EnsureProject(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, "fetchAvailableModels", map[string]any{"project": project})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetchAvailableModels HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var env struct {
		Models map[string]struct {
			DisplayName     string `json:"displayName"`
			MaxTokens       int64  `json:"maxTokens"`
			MaxOutputTokens int64  `json:"maxOutputTokens"`
			Recommended     bool   `json:"recommended"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse fetchAvailableModels: %w", err)
	}
	out := make([]Model, 0, len(env.Models))
	for id, m := range env.Models {
		// Skip upstream-internal entries; keep real models even when they have no
		// display name yet (the provider layer fills one in).
		if strings.HasPrefix(id, "tab_") || strings.HasPrefix(id, "chat_") {
			continue
		}
		out = append(out, Model{ID: id, DisplayName: strings.TrimSpace(m.DisplayName), ContextLength: m.MaxTokens, MaxOutput: m.MaxOutputTokens})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("fetchAvailableModels returned no usable models")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// StaticModels returns the model catalogue. Exported so callers can map a
// public display name back to the upstream model id without a client.
func StaticModels() []Model {
	return append([]Model(nil), staticModels...)
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
	// Antigravity injects its own identity plus an [ignore] copy so the model
	// does not claim to be a third-party agent; the caller system prompt is
	// appended after it.
	sysParts := []map[string]any{
		{"text": antigravitySystemInstruction},
		{"text": "Please ignore the following [ignore]" + antigravitySystemInstruction + "[/ignore]"},
	}
	if systemText.Len() > 0 {
		sysParts = append(sysParts, map[string]any{"text": systemText.String()})
	}
	inner["systemInstruction"] = map[string]any{
		"role":  "user",
		"parts": sysParts,
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
		"model":       model,
		"project":     project,
		"request":     inner,
		"userAgent":   "antigravity",
		"requestType": "agent",
		"requestId":   "agent-" + randomID(),
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

// antigravitySystemInstruction is the minimal Antigravity identity prompt.
const antigravitySystemInstruction = "You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding.You are pair programming with a USER to solve their coding task."

// randomID returns a pseudo-random hex id for the requestId field.
func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
