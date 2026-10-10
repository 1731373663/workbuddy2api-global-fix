package server

import "testing"

func TestSplitProviderModel(t *testing.T) {
	cases := []struct {
		in, name, model string
		ok              bool
	}{
		{"demo:gpt-x", "demo", "gpt-x", true},
		{"cn:glm", "", "", false},
		{"global:gpt-x", "", "", false},
		{"noprefix", "", "", false},
		{":bad", "", "", false},
		{"bad:", "", "", false},
	}
	for _, c := range cases {
		name, model, ok := splitProviderModel(c.in)
		if name != c.name || model != c.model || ok != c.ok {
			t.Fatalf("%q => %q %q %v", c.in, name, model, ok)
		}
	}
}
