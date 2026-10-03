package provider

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	// PublisherAnthropic serves Claude models via the Vertex :rawPredict surface.
	PublisherAnthropic = "anthropic"
	// PublisherGoogle serves Gemini models via the Vertex :generateContent surface.
	PublisherGoogle = "google"

	defaultModelPrefix = "vertex"
	defaultLocation    = "global"
)

// supportedPublishers are the Vertex publisher namespaces this plugin can route
// and translate. Other Model Garden publishers (meta, mistral, ...) are typically
// self-deployed endpoints rather than directly callable partner APIs, so they are
// filtered out of discovery until first-class support exists.
var supportedPublishers = map[string]struct{}{
	PublisherAnthropic: {},
	PublisherGoogle:    {},
}

// ModelConfig is an optional explicit model entry. Name is the publisher-qualified
// upstream identifier, e.g. "anthropic/claude-sonnet-4@20250514" or
// "google/gemini-2.5-pro". Alias overrides the public, namespaced model ID.
type ModelConfig struct {
	Name  string `yaml:"name"`
	Alias string `yaml:"alias"`
}

type Config struct {
	Enabled              bool          `yaml:"enabled"`
	ProjectID            string        `yaml:"project_id"`
	Location             string        `yaml:"location"`
	APIEndpoint          string        `yaml:"api_endpoint"`
	QuotaProjectID       string        `yaml:"quota_project_id"`
	Credentials          string        `yaml:"credentials"`
	AllowInsecureBaseURL bool          `yaml:"allow_insecure_base_url"`
	Publishers           []string      `yaml:"publishers"`
	ModelPrefix          string        `yaml:"model_prefix"`
	Models               []ModelConfig `yaml:"models"`
	ModelsExcluded       []string      `yaml:"models_excluded"`
	ModelCacheTTLSeconds int           `yaml:"model_cache_ttl_seconds"`
	ProxyURL             string        `yaml:"proxy_url"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:              true,
		Location:             defaultLocation,
		Publishers:           []string{PublisherAnthropic, PublisherGoogle},
		ModelPrefix:          defaultModelPrefix,
		ModelCacheTTLSeconds: 300,
	}
}

func ParseConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(raw) > 0 {
		// Zero the slice defaults so an explicit empty list in YAML is honored and
		// an omitted key falls back to the defaults applied below.
		cfg.Publishers = nil
		if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
			return Config{}, fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
	}

	cfg.ProjectID = strings.TrimSpace(cfg.ProjectID)
	cfg.QuotaProjectID = strings.TrimSpace(cfg.QuotaProjectID)
	cfg.Credentials = strings.TrimSpace(cfg.Credentials)
	cfg.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
	cfg.APIEndpoint = strings.TrimRight(strings.TrimSpace(cfg.APIEndpoint), "/")
	if cfg.APIEndpoint != "" {
		if err := validateAPIEndpoint(cfg.APIEndpoint, cfg.AllowInsecureBaseURL); err != nil {
			return Config{}, fmt.Errorf("api_endpoint: %w", err)
		}
	}

	cfg.Location = strings.TrimSpace(cfg.Location)
	if cfg.Location == "" {
		cfg.Location = defaultLocation
	}

	cfg.ModelPrefix = strings.TrimRight(strings.TrimSpace(cfg.ModelPrefix), "/")
	if cfg.ModelPrefix == "" {
		cfg.ModelPrefix = defaultModelPrefix
	}

	publishers, err := normalizePublishers(cfg.Publishers)
	if err != nil {
		return Config{}, err
	}
	cfg.Publishers = publishers

	seen := make(map[string]bool, len(cfg.Models))
	for i := range cfg.Models {
		model := &cfg.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" {
			return Config{}, fmt.Errorf("each configured model requires a name")
		}
		publisher, upstreamID, ok := splitPublisherModel(model.Name)
		if !ok {
			return Config{}, fmt.Errorf("model %q must be publisher-qualified, e.g. anthropic/claude-sonnet-4", model.Name)
		}
		if _, supported := supportedPublishers[publisher]; !supported {
			return Config{}, fmt.Errorf("model %q uses unsupported publisher %q", model.Name, publisher)
		}
		if model.Alias == "" {
			model.Alias = exposedModelID(cfg.ModelPrefix, publisher, upstreamID)
		}
		if seen[model.Alias] {
			return Config{}, fmt.Errorf("model aliases must be unique")
		}
		seen[model.Alias] = true
	}

	cfg.ModelsExcluded = normalizeModelPrefixes(cfg.ModelsExcluded)

	if cfg.ModelCacheTTLSeconds < 30 || cfg.ModelCacheTTLSeconds > 3600 {
		return Config{}, fmt.Errorf("model_cache_ttl_seconds must be between 30 and 3600")
	}
	return cfg, nil
}

func normalizePublishers(publishers []string) ([]string, error) {
	if len(publishers) == 0 {
		return []string{PublisherAnthropic, PublisherGoogle}, nil
	}
	seen := make(map[string]struct{}, len(publishers))
	out := make([]string, 0, len(publishers))
	for _, publisher := range publishers {
		publisher = strings.ToLower(strings.TrimSpace(publisher))
		if publisher == "" {
			continue
		}
		if _, supported := supportedPublishers[publisher]; !supported {
			return nil, fmt.Errorf("unsupported publisher %q (supported: anthropic, google)", publisher)
		}
		if _, exists := seen[publisher]; exists {
			continue
		}
		seen[publisher] = struct{}{}
		out = append(out, publisher)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("publishers must contain at least one of: anthropic, google")
	}
	return out, nil
}

func normalizeModelPrefixes(prefixes []string) []string {
	seen := make(map[string]struct{}, len(prefixes))
	out := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix == "" {
			continue
		}
		if _, exists := seen[prefix]; exists {
			continue
		}
		seen[prefix] = struct{}{}
		out = append(out, prefix)
	}
	return out
}

// apiBase returns the Vertex API host base, preferring an explicit override and
// otherwise deriving it from the location.
func (c Config) apiBase() string {
	if c.APIEndpoint != "" {
		return c.APIEndpoint
	}
	return vertexHost(c.Location)
}

func validateAPIEndpoint(raw string, allowInsecure bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("invalid absolute URL")
	}
	loopback := parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost" || parsed.Hostname() == "::1"
	if parsed.Scheme != "https" && !(allowInsecure && parsed.Scheme == "http" && loopback) {
		return fmt.Errorf("URL must use HTTPS")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("endpoint must not contain user, query, or fragment")
	}
	return nil
}

func (c Config) modelCacheTTL() time.Duration {
	return time.Duration(c.ModelCacheTTLSeconds) * time.Second
}
