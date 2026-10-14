package geminiapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"
)

// CaResponse is the Code Assist generateContent response envelope.
type CaResponse struct {
	Response *struct {
		Candidates []struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	} `json:"response"`
	TraceID string `json:"traceId"`
}

// text extracts the concatenated candidate text.
func (r *CaResponse) text() string {
	if r == nil || r.Response == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Response.Candidates {
		for _, p := range c.Content.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Generate calls generateContent and returns the assistant text plus usage.
func (c *Client) Generate(ctx context.Context, in OpenAIChatRequest) (string, map[string]any, error) {
	project, _, err := c.EnsureProject(ctx)
	if err != nil {
		return "", nil, err
	}
	body := BuildRequest(in, project)
	if dbg, _ := json.Marshal(body); true {
		log.Printf("DBG gemini generateContent body: %s", truncate(string(dbg), 600))
	}
	resp, err := c.do(ctx, "generateContent", body)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("generateContent HTTP %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	var out CaResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("parse generateContent: %w", err)
	}
	usage := map[string]any{}
	if out.Response != nil {
		usage["prompt_tokens"] = out.Response.UsageMetadata.PromptTokenCount
		usage["completion_tokens"] = out.Response.UsageMetadata.CandidatesTokenCount
		usage["total_tokens"] = out.Response.UsageMetadata.TotalTokenCount
	}
	return out.text(), usage, nil
}

// StreamChunk is one incremental text delta from streamGenerateContent.
type StreamChunk struct {
	Text         string
	PromptTokens int
	OutputTokens int
	TotalTokens  int
}

// GenerateStream calls streamGenerateContent and invokes onChunk for each delta.
//
// Antigravity returns a JSON array stream ("[{...},\n{...}]"); each element holds
// an incremental candidate delta. A json.Decoder walks the array incrementally so
// frames are emitted as they arrive without buffering the whole body.
func (c *Client) GenerateStream(ctx context.Context, in OpenAIChatRequest, onChunk func(StreamChunk) error) error {
	project, _, err := c.EnsureProject(ctx)
	if err != nil {
		return err
	}
	body := BuildRequest(in, project)
	resp, err := c.do(ctx, "streamGenerateContent", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("streamGenerateContent HTTP %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	dec := json.NewDecoder(bufio.NewReaderSize(resp.Body, 64*1024))
	if _, err := dec.Token(); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	for dec.More() {
		var frame CaResponse
		if err := dec.Decode(&frame); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		text := frame.text()
		if text == "" {
			continue
		}
		chunk := StreamChunk{Text: text}
		if frame.Response != nil {
			chunk.PromptTokens = frame.Response.UsageMetadata.PromptTokenCount
			chunk.OutputTokens = frame.Response.UsageMetadata.CandidatesTokenCount
			chunk.TotalTokens = frame.Response.UsageMetadata.TotalTokenCount
		}
		if err := onChunk(chunk); err != nil {
			return err
		}
	}
	return nil
}

// Probe verifies the credential by resolving the project (cheap health check).
func (c *Client) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, _, err := c.EnsureProject(ctx)
	return err
}
