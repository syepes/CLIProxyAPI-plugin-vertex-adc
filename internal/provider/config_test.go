package provider

import "testing"

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig(nil)
	if err != nil {
		t.Fatalf("ParseConfig(nil): %v", err)
	}
	if cfg.Location != "global" {
		t.Errorf("default location = %q, want global", cfg.Location)
	}
	if cfg.ModelPrefix != "vertex" {
		t.Errorf("default model_prefix = %q, want vertex", cfg.ModelPrefix)
	}
	if len(cfg.Publishers) != 2 {
		t.Errorf("default publishers = %v, want anthropic+google", cfg.Publishers)
	}
	if cfg.ModelCacheTTLSeconds != 300 {
		t.Errorf("default ttl = %d, want 300", cfg.ModelCacheTTLSeconds)
	}
}

func TestParseConfigPublisherValidation(t *testing.T) {
	if _, err := ParseConfig([]byte("publishers:\n  - meta\n")); err == nil {
		t.Fatal("expected error for unsupported publisher")
	}
	cfg, err := ParseConfig([]byte("publishers:\n  - anthropic\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(cfg.Publishers) != 1 || cfg.Publishers[0] != PublisherAnthropic {
		t.Errorf("publishers = %v, want [anthropic]", cfg.Publishers)
	}
}

func TestParseConfigModelAllowlist(t *testing.T) {
	cfg, err := ParseConfig([]byte("models:\n  - name: anthropic/claude-sonnet-4@20250514\n  - name: google/gemini-2.5-pro\n    alias: gem-pro\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Models[0].Alias != "vertex/anthropic/claude-sonnet-4@20250514" {
		t.Errorf("default alias = %q", cfg.Models[0].Alias)
	}
	if cfg.Models[1].Alias != "gem-pro" {
		t.Errorf("explicit alias = %q, want gem-pro", cfg.Models[1].Alias)
	}
}

func TestParseConfigRejectsUnqualifiedModel(t *testing.T) {
	if _, err := ParseConfig([]byte("models:\n  - name: claude-sonnet-4\n")); err == nil {
		t.Fatal("expected error for unqualified model name")
	}
}

func TestParseConfigTTLBounds(t *testing.T) {
	if _, err := ParseConfig([]byte("model_cache_ttl_seconds: 10\n")); err == nil {
		t.Fatal("expected error for ttl below range")
	}
	if _, err := ParseConfig([]byte("model_cache_ttl_seconds: 99999\n")); err == nil {
		t.Fatal("expected error for ttl above range")
	}
}
