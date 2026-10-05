package server

import "strings"

// isGPTModelName reports whether a model is GPT-family.
func isGPTModelName(name string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "gpt-")
}

// resolveModel parses the optional realm prefix.
// All GPT-family models are forced to the global realm.
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		if isGPTModelName(model) {
			return "global", model
		}
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model
	}
	bare = model[idx+1:]
	if isGPTModelName(bare) {
		return "global", bare
	}
	return prefix, bare
}

func ResolveModel(model string) (realm, bare string) {
	return resolveModel(model)
}
