package pool

import (
	"path/filepath"
	"testing"
)

func TestAccountModelSettingsPersistAndMatch(t *testing.T) {
	SetAccountModelSettingsPath(filepath.Join(t.TempDir(), "account_models.json"))
	defer SetAccountModelSettingsPath("")
	if err := SetAccountModelSettings("u1", AccountModelSettings{EnabledModels: []string{"m2", "m1"}, Aliases: map[string]string{"m1": "alias-one"}}); err != nil {
		t.Fatal(err)
	}
	s := GetAccountModelSettings("u1")
	if len(s.EnabledModels) != 2 || s.EnabledModels[0] != "m1" || s.EnabledModels[1] != "m2" {
		t.Fatalf("enabled=%v", s.EnabledModels)
	}
	if s.ModelEnabled("m3") {
		t.Fatal("m3 should be disabled")
	}
	if !s.ModelEnabled("m2") {
		t.Fatal("m2 should be enabled")
	}
	if s.PublicModelName("m1") != "alias-one" {
		t.Fatalf("alias=%q", s.PublicModelName("m1"))
	}
	if s.RealModelName("alias-one") != "m1" {
		t.Fatalf("real=%q", s.RealModelName("alias-one"))
	}
}
