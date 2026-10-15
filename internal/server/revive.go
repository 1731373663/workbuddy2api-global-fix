package server

import (
	"net/http"
	"strings"
)

// reviveAccount clears a cooldown once the upstream confirms the account still
// has credits. The pool marks hard_credit cooldowns until the next 04:00 and the
// scheduled refresh path is the only thing that clears them, so a transient
// upstream "insufficient credits" reply can strand a healthy account for hours.
// This endpoint queries the live balance and revives the account when remain > 0.
func (h *Handler) reviveAccount(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid is required")
		return
	}
	acct := h.cfg.Pool.AuthByUID(uid)
	if acct == nil {
		writeOpenAIError(w, http.StatusNotFound, "account_not_found", "account not found")
		return
	}
	before, _ := h.cfg.Pool.Status(uid)
	remain, err := h.cfg.Upstream.UserResource(acct)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "balance_unavailable", "failed to fetch balance: "+err.Error())
		return
	}
	revived := h.cfg.Pool.ReviveAccount(uid, remain)
	after, _ := h.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      remain > 0,
		"uid":     uid,
		"remain":  remain,
		"revived": revived || (before.Cooling && !after.Cooling),
		"cooling": after.Cooling,
		"message": reviveMessage(remain, before.Cooling, after.Cooling),
	})
}

func reviveMessage(remain int64, wasCooling, stillCooling bool) string {
	if remain <= 0 {
		return "余额为 0，未解冻"
	}
	if wasCooling && !stillCooling {
		return "余额充足，已解除冷却"
	}
	if stillCooling {
		return "余额充足，但仍处于其他冷却状态"
	}
	return "余额充足，账号未在冷却中"
}
