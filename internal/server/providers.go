package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/provider"
	"workbuddy2api/internal/upstream"
)

var providerModelsCache struct {
	sync.RWMutex
	models  map[string][]provider.Model
	fetched time.Time
}

const providerModelsTTL = 10 * time.Minute

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
	models := h.fetchProviderModels(ctx)
	out := make([]map[string]any, 0)
	for name, list := range models {
		for _, model := range list {
			owned := model.OwnedBy
			if owned == "" {
				owned = name
			}
			out = append(out, map[string]any{
				"id":       name + ":" + model.ID,
				"object":   "model",
				"created":  int64(1753600000),
				"owned_by": owned,
				"provider": name,
			})
		}
	}
	return out
}

func (h *Handler) fetchProviderModels(ctx context.Context) map[string][]provider.Model {
	providerModelsCache.RLock()
	if len(providerModelsCache.models) > 0 && time.Since(providerModelsCache.fetched) < providerModelsTTL {
		out := providerModelsCache.models
		providerModelsCache.RUnlock()
		return out
	}
	providerModelsCache.RUnlock()
	out := h.cfg.Providers.ListModels(ctx)
	providerModelsCache.Lock()
	providerModelsCache.models = out
	providerModelsCache.fetched = time.Now()
	providerModelsCache.Unlock()
	return out
}

// providerChat handles a request addressed as "<provider>:<model>".
func (h *Handler) providerChat(w http.ResponseWriter, r *http.Request, body []byte, stream bool, st *chatStat, providerName, bareModel string) {
	rc, status, respBody, err := h.cfg.Providers.Stream(r.Context(), providerName, bareModel, body)
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
	resp, err := upstream.Aggregate(rc)
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
