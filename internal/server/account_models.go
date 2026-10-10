package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"workbuddy2api/internal/pool"
)

type accountModelEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	PublicName  string `json:"public_name"`
	Enabled     bool   `json:"enabled"`
	ContextSize int64  `json:"context_length,omitempty"`
	MaxOutput   int64  `json:"max_output_tokens,omitempty"`
}

func (h *Handler) accountModelUID(r *http.Request) string {
	return strings.TrimSpace(r.PathValue("uid"))
}

func (h *Handler) accountModels(w http.ResponseWriter, r *http.Request) {
	uid := h.accountModelUID(r)
	if uid == "" {
		writeJSON(w, http.StatusOK, map[string]any{"settings": pool.AllAccountModelSettings()})
		return
	}
	acct := h.cfg.Pool.PickByUIDForModel(uid, "")
	if acct == nil {
		writeOpenAIError(w, http.StatusNotFound, "account_not_found", "account not found or unavailable")
		return
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		writeOpenAIError(w, http.StatusBadGateway, "models_unavailable", "failed to fetch models for this account")
		return
	}
	settings := pool.GetAccountModelSettings(uid)
	out := make([]accountModelEntry, 0, len(infos))
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		out = append(out, accountModelEntry{ID: mi.ID, Name: mi.Name, PublicName: settings.PublicModelName(mi.ID), Enabled: settings.ModelEnabled(mi.ID), ContextSize: mi.ContextWindow, MaxOutput: mi.MaxTokens})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "models": out, "enabled": settings.EnabledModels, "aliases": settings.Aliases, "account": acct.Nickname, "realm": acct.Realm(), "fetched_at": time.Now().UTC()})
}

func (h *Handler) accountModelSettingsPut(w http.ResponseWriter, r *http.Request) {
	uid := h.accountModelUID(r)
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid is required")
		return
	}
	var req struct {
		EnabledModels []string          `json:"enabled_models"`
		Aliases       map[string]string `json:"model_aliases"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	if err := pool.SetAccountModelSettings(uid, pool.AccountModelSettings{EnabledModels: req.EnabledModels, Aliases: req.Aliases}); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "模型设置已保存"})
}
