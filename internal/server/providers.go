package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

func splitProviderModel(model string) (string, string, bool) {
	model = strings.TrimSpace(model)
	idx := strings.IndexByte(model, ':')
	if idx <= 0 || idx == len(model)-1 {
		return "", "", false
	}
	name, bare := model[:idx], model[idx+1:]
	if name == "cn" || name == "global" {
		return "", "", false
	}
	return name, bare, true
}

func (h *Handler) providerModelList() []map[string]any {
	if h.cfg.Providers == nil || !h.cfg.Providers.Enabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out := make([]map[string]any, 0)
	for _, name := range h.cfg.Providers.Names() {
		list, err := h.cfg.Providers.ConfiguredModels(ctx, name)
		if err != nil {
			log.Printf("WARN: [provider] list models provider=%s: %v", name, err)
			continue
		}
		nameCount := map[string]int{}
		for _, model := range list {
			nm := strings.TrimSpace(model.Name)
			if nm == "" {
				nm = model.ID
			}
			nameCount[nm]++
		}
		for _, model := range list {
			public := model.ID
			if strings.TrimSpace(model.Name) != "" {
				public = model.Name
				if nameCount[public] > 1 {
					public = public + " [" + model.ID + "]"
				}
			}
			owned := model.OwnedBy
			if owned == "" {
				owned = name
			}
			entry := map[string]any{
				"id":       name + ":" + public,
				"object":   "model",
				"created":  int64(1753600000),
				"owned_by": owned,
				"provider": name,
			}
			if model.ContextLength > 0 {
				entry["context_length"] = model.ContextLength
			}
			if model.MaxOutput > 0 {
				entry["max_output_tokens"] = model.MaxOutput
			}
			if len(model.Efforts) > 0 {
				entry["reasoning_supported_efforts"] = append([]string(nil), model.Efforts...)
				if model.DefaultEffort != "" {
					entry["reasoning_default_effort"] = model.DefaultEffort
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

// providerChat handles a request addressed as "<provider>:<model>".
func (h *Handler) providerChat(w http.ResponseWriter, r *http.Request, body []byte, stream bool, st *chatStat, providerName, bareModel string) {
	upstreamModel := h.cfg.Providers.ResolveModel(providerName, bareModel)
	rc, status, respBody, err := h.cfg.Providers.Stream(r.Context(), providerName, upstreamModel, body)
	st.status = status
	st.uid = "provider:" + providerName
	st.detail.Endpoint = "/" + providerName + "/chat/completions"
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "provider_error", err.Error())
		st.status = http.StatusBadGateway
		st.detail.ErrorCode = "provider_error"
		st.detail.ErrorMessage = err.Error()
		return
	}
	if status >= 400 {
		msg := string(respBody)
		writeOpenAIError(w, status, "provider_error", msg)
		st.detail.ErrorCode = "provider_error"
		st.detail.ErrorMessage = msg
		return
	}
	defer rc.Close()
	if stream {
		st.status = http.StatusOK
		stats := newChatStatsReaderSince(rc, st.start)
		if err := upstream.Stream(w, stats); err != nil {
			if sink, ok := w.(interface{ streamError(error) }); ok {
				sink.streamError(err)
			}
		}
		st.ttfb = stats.TTFB()
		st.toks, _ = stats.Tokens()
		st.usage = stats.Usage()
		st.detail.FinishReason = stats.FinishReason()
		return
	}
	raw, readErr := io.ReadAll(io.LimitReader(rc, 16<<20))
	if readErr != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_read", readErr.Error())
		st.status = http.StatusBadGateway
		st.detail.ErrorCode = "upstream_read"
		st.detail.ErrorMessage = readErr.Error()
		return
	}
	var resp map[string]any
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "data:") {
		resp, err = upstream.Aggregate(strings.NewReader(string(raw)))
	} else {
		err = json.Unmarshal(raw, &resp)
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		st.status = http.StatusBadGateway
		st.detail.ErrorCode = "upstream_parse"
		st.detail.ErrorMessage = err.Error()
		return
	}
	writeJSON(w, http.StatusOK, resp)
	st.status = http.StatusOK
	st.toks = completionTokens(resp)
	if u, ok := resp["usage"].(map[string]any); ok {
		st.usage = ParseUsage(u)
	}
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			st.detail.FinishReason, _ = ch["finish_reason"].(string)
		}
	}
}

var _ = io.Discard
