package provider

import (
	"strings"
	"testing"
)

func TestParsePublisherModelName(t *testing.T) {
	p, m, ok := parsePublisherModelName("publishers/anthropic/models/claude-sonnet-4")
	if !ok || p != "anthropic" || m != "claude-sonnet-4" {
		t.Errorf("got (%q,%q,%v)", p, m, ok)
	}
	if _, _, ok := parsePublisherModelName("projects/x/models/y"); ok {
		t.Error("expected failure for non-publisher name")
	}
	if _, _, ok := parsePublisherModelName("publishers/google/models/a/b"); ok {
		t.Error("expected failure for nested model path")
	}
}

func TestHasFamilyPrefix(t *testing.T) {
	if !hasFamilyPrefix("claude-sonnet-4", []string{"claude"}) {
		t.Error("claude should match")
	}
	if hasFamilyPrefix("text-embedding-004", []string{"gemini"}) {
		t.Error("embedding should not match gemini family")
	}
	if !hasFamilyPrefix("anything", nil) {
		t.Error("empty prefixes should match all")
	}
}

func TestFilterCatalog(t *testing.T) {
	in := []catalogModel{
		{Publisher: "google", UpstreamID: "gemini-2.5-pro"},
		{Publisher: "google", UpstreamID: "gemini-1.5-flash"},
	}
	out := filterCatalog(in, []string{"google/gemini-1.5"})
	if len(out) != 1 || out[0].UpstreamID != "gemini-2.5-pro" {
		t.Errorf("filterCatalog = %+v", out)
	}
}

func TestDedupeCatalog(t *testing.T) {
	in := []catalogModel{
		{Publisher: "anthropic", UpstreamID: "claude"},
		{Publisher: "anthropic", UpstreamID: "Claude"},
		{Publisher: "google", UpstreamID: "gemini"},
	}
	out := dedupeCatalog(in)
	if len(out) != 2 {
		t.Errorf("dedupeCatalog len = %d, want 2", len(out))
	}
}

func TestModelsFromConfig(t *testing.T) {
	cfg := Config{ModelPrefix: "vertex", Models: []ModelConfig{
		{Name: "google/gemini-2.5-pro", Alias: "gem"},
		{Name: "anthropic/claude-sonnet-4@20250514"},
		{Name: "bogus-no-slash"},
	}}
	out := modelsFromConfig(cfg)
	if len(out) != 2 {
		t.Fatalf("modelsFromConfig len = %d, want 2 (bogus skipped)", len(out))
	}
	if out[0].Publisher != "google" || out[0].UpstreamID != "gemini-2.5-pro" {
		t.Errorf("first = %+v", out[0])
	}
}

func TestGoogleErrorDetail(t *testing.T) {
	body := []byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Agent Platform API has not been used in project X or it is disabled."}}`)
	got := googleErrorDetail(body)
	if !strings.Contains(got, "PERMISSION_DENIED") || !strings.Contains(got, "disabled") {
		t.Errorf("googleErrorDetail = %q", got)
	}
	if googleErrorDetail([]byte("<html>not json</html>")) != "" {
		t.Error("non-JSON body should yield empty detail")
	}
}
