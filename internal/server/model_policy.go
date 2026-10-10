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

// modelVisibleForRealm reports whether a real model should appear in the public
// model list for a realm.
//
// Account-level model controls are realm-scoped: CN settings must not hide
// global models, and global settings must not hide CN models. When a realm has
// at least one account with an explicit enable list, that realm's public list
// becomes the union of those explicit lists. Accounts without an explicit list
// in the same realm do not keep every model visible, otherwise disabling a model
// on the only configured account would have no visible effect.
func (h *Handler) modelVisibleForRealm(realm, realModel string) bool {
	realModel = strings.TrimSpace(realModel)
	if realModel == "" {
		return false
	}
	settings := pool.AllAccountModelSettings()
	if len(settings) == 0 {
		return true
	}
	seenRealmPolicy := false
	for uid, s := range settings {
		acct := h.cfg.Pool.AuthByUID(uid)
		if acct == nil || acct.Realm() != realm {
			continue
		}
		if len(s.EnabledModels) == 0 {
			continue
		}
		seenRealmPolicy = true
		if s.ModelEnabled(realModel) {
			return true
		}
	}
	return !seenRealmPolicy
}
