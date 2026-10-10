package server

import (
	"strings"

	"workbuddy2api/internal/pool"
)

func stripRealmPrefix(model string) string {
	if idx := strings.IndexByte(model, ':'); idx > 0 {
		if p := model[:idx]; p == "cn" || p == "global" {
			return model[idx+1:]
		}
	}
	return model
}

// publicModelID applies the first account alias found for a real model.
func (h *Handler) publicModelID(realm, realModel string) string {
	for _, settings := range pool.AllAccountModelSettings() {
		if alias := strings.TrimSpace(settings.PublicModelName(realModel)); alias != realModel {
			return listedModelID(realm, alias)
		}
	}
	return listedModelID(realm, realModel)
}

// realModelName maps any account's public alias back to the real upstream model.
func (h *Handler) realModelName(realm, publicModel string) string {
	realModel := stripRealmPrefix(publicModel)
	if realm == "global" && isGPTModelID(realModel) {
		return realModel
	}
	for _, settings := range pool.AllAccountModelSettings() {
		if real := settings.RealModelName(realModel); real != realModel {
			return real
		}
	}
	return realModel
}

// modelEnabledForAccount reports whether an account may serve the requested model.
func (h *Handler) modelEnabledForAccount(uid, realm, publicModel string) bool {
	realModel := h.realModelName(realm, publicModel)
	return pool.GetAccountModelSettings(uid).ModelEnabled(realModel)
}
