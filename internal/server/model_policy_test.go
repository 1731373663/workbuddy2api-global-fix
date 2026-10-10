package server

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

func TestModelVisibleForRealmIsRealmScoped(t *testing.T) {
	pool.SetAccountModelSettingsPath(filepath.Join(t.TempDir(), "account_models.json"))
	t.Cleanup(func() { pool.SetAccountModelSettingsPath("") })

	cn := &auth.Auth{UID: "cn-user", AccessToken: "a", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999}
	gl := &auth.Auth{UID: "global-user", AccessToken: "b", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
	p := testPoolWith(cn, gl)
	h := NewHandler(Config{Pool: p, GlobalEnabled: true})

	if err := pool.SetAccountModelSettings(cn.UID, pool.AccountModelSettings{EnabledModels: []string{"cn-only"}}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetAccountModelSettings(gl.UID, pool.AccountModelSettings{EnabledModels: []string{"gl-only"}}); err != nil {
		t.Fatal(err)
	}

	if !h.modelVisibleForRealm("cn", "cn-only") {
		t.Fatal("CN enabled model should be visible in CN")
	}
	if h.modelVisibleForRealm("cn", "gl-only") {
		t.Fatal("global-only model must not leak into CN visibility")
	}
	if !h.modelVisibleForRealm("global", "gl-only") {
		t.Fatal("global enabled model should be visible in global")
	}
	if h.modelVisibleForRealm("global", "cn-only") {
		t.Fatal("CN-only model must not leak into global visibility")
	}
}

func TestModelVisibleForRealmWithoutPolicyAllowsAll(t *testing.T) {
	pool.SetAccountModelSettingsPath(filepath.Join(t.TempDir(), "account_models.json"))
	t.Cleanup(func() { pool.SetAccountModelSettingsPath("") })
	p := testPoolWith(&auth.Auth{UID: "cn-user", AccessToken: "a", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, GlobalEnabled: true})
	if !h.modelVisibleForRealm("cn", "anything") {
		t.Fatal("without realm policy every model should remain visible")
	}
}
