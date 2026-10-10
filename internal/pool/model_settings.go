package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type AccountModelSettings struct {
	EnabledModels []string          `json:"enabled_models,omitempty"`
	Aliases       map[string]string `json:"model_aliases,omitempty"`
}

var accountModelSettings struct {
	mu   sync.RWMutex
	path string
	data map[string]AccountModelSettings
}

func SetAccountModelSettingsPath(path string) {
	accountModelSettings.mu.Lock()
	defer accountModelSettings.mu.Unlock()
	accountModelSettings.path = path
	accountModelSettings.data = map[string]AccountModelSettings{}
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, &accountModelSettings.data)
	if accountModelSettings.data == nil {
		accountModelSettings.data = map[string]AccountModelSettings{}
	}
}

func GetAccountModelSettings(uid string) AccountModelSettings {
	accountModelSettings.mu.RLock()
	defer accountModelSettings.mu.RUnlock()
	return cloneAccountModelSettings(accountModelSettings.data[uid])
}

func AllAccountModelSettings() map[string]AccountModelSettings {
	accountModelSettings.mu.RLock()
	defer accountModelSettings.mu.RUnlock()
	out := make(map[string]AccountModelSettings, len(accountModelSettings.data))
	for uid, v := range accountModelSettings.data {
		out[uid] = cloneAccountModelSettings(v)
	}
	return out
}

func SetAccountModelSettings(uid string, settings AccountModelSettings) error {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil
	}
	settings.EnabledModels = uniqueStrings(settings.EnabledModels)
	if len(settings.Aliases) == 0 {
		settings.Aliases = nil
	}
	accountModelSettings.mu.Lock()
	defer accountModelSettings.mu.Unlock()
	if accountModelSettings.data == nil {
		accountModelSettings.data = map[string]AccountModelSettings{}
	}
	if len(settings.EnabledModels) == 0 && len(settings.Aliases) == 0 {
		delete(accountModelSettings.data, uid)
	} else {
		accountModelSettings.data[uid] = cloneAccountModelSettings(settings)
	}
	return saveAccountModelSettingsLocked()
}

func saveAccountModelSettingsLocked() error {
	if accountModelSettings.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(accountModelSettings.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(accountModelSettings.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := accountModelSettings.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, accountModelSettings.path)
}

func cloneAccountModelSettings(v AccountModelSettings) AccountModelSettings {
	out := AccountModelSettings{EnabledModels: append([]string(nil), v.EnabledModels...)}
	if len(v.Aliases) > 0 {
		out.Aliases = make(map[string]string, len(v.Aliases))
		for k, val := range v.Aliases {
			out.Aliases[k] = val
		}
	}
	return out
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func (s AccountModelSettings) ModelEnabled(realModel string) bool {
	realModel = strings.TrimSpace(realModel)
	if len(s.EnabledModels) == 0 {
		return true
	}
	for _, m := range s.EnabledModels {
		if m == realModel {
			return true
		}
	}
	return false
}

func (s AccountModelSettings) PublicModelName(realModel string) string {
	if alias := strings.TrimSpace(s.Aliases[realModel]); alias != "" {
		return alias
	}
	return realModel
}

func (s AccountModelSettings) RealModelName(publicModel string) string {
	publicModel = strings.TrimSpace(publicModel)
	for real, alias := range s.Aliases {
		if strings.TrimSpace(alias) == publicModel {
			return real
		}
	}
	return publicModel
}
