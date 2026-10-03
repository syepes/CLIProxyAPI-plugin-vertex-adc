package provider

import "testing"

func TestSplitPublisherModel(t *testing.T) {
	cases := []struct {
		in        string
		publisher string
		model     string
		ok        bool
	}{
		{"anthropic/claude-sonnet-4@20250514", "anthropic", "claude-sonnet-4@20250514", true},
		{"google/gemini-2.5-pro", "google", "gemini-2.5-pro", true},
		{"claude-sonnet-4", "", "", false},
		{"anthropic/", "", "", false},
		{"/model", "", "", false},
	}
	for _, c := range cases {
		p, m, ok := splitPublisherModel(c.in)
		if ok != c.ok || p != c.publisher || m != c.model {
			t.Errorf("splitPublisherModel(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, p, m, ok, c.publisher, c.model, c.ok)
		}
	}
}

func TestResolveModelDiscoveryPath(t *testing.T) {
	cfg := Config{ModelPrefix: "vertex"}
	got, err := cfg.resolveModel("vertex/anthropic/claude-sonnet-4@20250514")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if got.Publisher != "anthropic" || got.UpstreamID != "claude-sonnet-4@20250514" {
		t.Errorf("resolved = %+v", got)
	}
	if _, err := cfg.resolveModel("vertex/meta/llama"); err == nil {
		t.Error("expected unsupported publisher error")
	}
	if _, err := cfg.resolveModel("other/anthropic/claude"); err == nil {
		t.Error("expected namespace error")
	}
}

func TestResolveModelAllowlistPath(t *testing.T) {
	cfg := Config{
		ModelPrefix: "vertex",
		Models:      []ModelConfig{{Name: "google/gemini-2.5-pro", Alias: "gem"}},
	}
	got, err := cfg.resolveModel("gem")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if got.Publisher != "google" || got.UpstreamID != "gemini-2.5-pro" {
		t.Errorf("resolved = %+v", got)
	}
	if _, err := cfg.resolveModel("vertex/google/gemini-2.5-pro"); err == nil {
		t.Error("expected allowlist rejection for non-alias ID")
	}
}

func TestModelAliases(t *testing.T) {
	cfg := Config{ModelPrefix: "vertex"}
	aliases := cfg.modelAliases("anthropic", "claude-sonnet-4")
	if len(aliases) != 1 || aliases[0] != "vertex/anthropic/claude-sonnet-4" {
		t.Errorf("aliases = %v", aliases)
	}
}
