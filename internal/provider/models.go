package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"cliproxyapi-vertex-adc/internal/transport"
)

const (
	maxCatalogModels = 4096
	maxCatalogPages  = 32
)

// familyPrefixes limits each supported publisher to its chat/generative model
// family. The raw Model Garden catalog also lists embeddings, image, and video
// models that are not chat-completable through the surfaces this plugin routes.
var familyPrefixes = map[string][]string{
	PublisherAnthropic: {"claude"},
	PublisherGoogle:    {"gemini"},
}

// catalogModel is a publisher-qualified upstream model discovered from Vertex.
type catalogModel struct {
	Publisher  string
	UpstreamID string
	VersionID  string
}

type modelCacheEntry struct {
	fetchedAt time.Time
	expiresAt time.Time
	models    []catalogModel
	err       string
	retryAt   time.Time
}

type modelFlight struct {
	done   chan struct{}
	models []catalogModel
	err    error
}

type publisherModelsResponse struct {
	PublisherModels []struct {
		Name      string `json:"name"`
		VersionID string `json:"versionId"`
	} `json:"publisherModels"`
	NextPageToken string `json:"nextPageToken"`
}

// StaticModels returns the discovered catalog. The model scope is static, so the
// host fetches the full model list through this method rather than per-auth.
func (s *Service) StaticModels() pluginapi.ModelResponse {
	empty := pluginapi.ModelResponse{Provider: providerID, Models: []pluginapi.ModelInfo{}}
	ctx, cancel := context.WithTimeout(s.ctx, 25*time.Second)
	defer cancel()
	models, err := s.catalog(ctx, false)
	if err != nil {
		return empty
	}
	return pluginapi.ModelResponse{Provider: providerID, Models: modelInfos(models, s.Config())}
}

// catalog returns the cached discovery result, refreshing it when stale. Concurrent
// callers coalesce onto a single in-flight discovery.
func (s *Service) catalog(ctx context.Context, force bool) ([]catalogModel, error) {
	cfg := s.Config()
	if !cfg.Enabled || s.ctx.Err() != nil {
		return nil, errors.New("plugin unavailable")
	}
	// An explicit allowlist is served directly and never depends on the Vertex
	// catalog list, which requires a quota project with the API enabled. This
	// lets inference work even when discovery is not permitted.
	if len(cfg.Models) > 0 {
		return modelsFromConfig(cfg), nil
	}
	now := s.now()
	s.modelMu.Lock()
	if cached := s.modelCache; cached != nil && !force {
		if cached.err == "" && now.Before(cached.expiresAt) {
			models := cloneCatalog(cached.models)
			s.modelMu.Unlock()
			return models, nil
		}
		if cached.err != "" && now.Before(cached.retryAt) {
			s.modelMu.Unlock()
			return nil, errors.New("model catalog unavailable")
		}
	}
	flight := s.modelInflight
	if flight == nil {
		flight = &modelFlight{done: make(chan struct{})}
		s.modelInflight = flight
		if !s.spawn(func() {
			requestCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
			defer cancel()
			models, err := s.fetchCatalog(requestCtx)
			entry := &modelCacheEntry{fetchedAt: s.now(), expiresAt: s.now().Add(s.Config().modelCacheTTL()), models: cloneCatalog(models)}
			if err != nil {
				// Surface the real reason (HTTP status + Vertex message) so quota /
				// API-enablement / permission problems are diagnosable from the dashboard.
				entry.err = err.Error()
				entry.expiresAt = s.now()
				entry.retryAt = s.now().Add(15 * time.Second)
			}
			s.modelMu.Lock()
			s.modelCache = entry
			flight.models = models
			flight.err = err
			s.modelInflight = nil
			close(flight.done)
			s.modelMu.Unlock()
		}) {
			s.modelInflight = nil
			s.modelMu.Unlock()
			return nil, errors.New("plugin unavailable")
		}
	}
	s.modelMu.Unlock()
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		return cloneCatalog(flight.models), flight.err
	}
}

func (s *Service) fetchCatalog(ctx context.Context) ([]catalogModel, error) {
	cfg := s.Config()
	creds, err := s.credentials(ctx)
	if err != nil {
		return nil, err
	}
	token, err := creds.token()
	if err != nil {
		return nil, err
	}
	host := cfg.apiBase()
	var all []catalogModel
	for _, publisher := range cfg.Publishers {
		models, errFetch := s.fetchPublisherModels(ctx, host, publisher, creds, token)
		if errFetch != nil {
			return nil, errFetch
		}
		all = append(all, models...)
		if len(all) >= maxCatalogModels {
			break
		}
	}
	all = dedupeCatalog(all)
	all = filterCatalog(all, cfg.ModelsExcluded)
	sort.Slice(all, func(i, j int) bool {
		if all[i].Publisher != all[j].Publisher {
			return all[i].Publisher < all[j].Publisher
		}
		return all[i].UpstreamID < all[j].UpstreamID
	})
	return all, nil
}

func (s *Service) fetchPublisherModels(ctx context.Context, host, publisher string, creds *adcCredentials, token string) ([]catalogModel, error) {
	prefixes := familyPrefixes[publisher]
	var out []catalogModel
	pageToken := ""
	for page := 0; page < maxCatalogPages; page++ {
		endpoint := host + "/v1beta1/publishers/" + url.PathEscape(publisher) + "/models?pageSize=200"
		if pageToken != "" {
			endpoint += "&pageToken=" + url.QueryEscape(pageToken)
		}
		headers := http.Header{"Accept": {"application/json"}}
		applyAuthHeader(headers, token, creds.quotaProject)
		resp, err := s.host.Do(ctx, "", transport.Request{Method: http.MethodGet, URL: endpoint, Headers: headers})
		if err != nil {
			return nil, errors.New("model discovery transport failed")
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// The catalog list carries no secrets, so surface the Vertex error to
			// make quota/API-enablement/permission failures diagnosable.
			return nil, fmt.Errorf("discovery for publisher %s failed: HTTP %d%s", publisher, resp.StatusCode, googleErrorDetail(resp.Body))
		}
		var list publisherModelsResponse
		if len(resp.Body) > 16<<20 || json.Unmarshal(resp.Body, &list) != nil {
			return nil, errors.New("invalid model catalog response")
		}
		for _, item := range list.PublisherModels {
			parsedPublisher, upstreamID, ok := parsePublisherModelName(item.Name)
			if !ok || parsedPublisher != publisher {
				continue
			}
			if !hasFamilyPrefix(upstreamID, prefixes) {
				continue
			}
			out = append(out, catalogModel{Publisher: publisher, UpstreamID: upstreamID, VersionID: strings.TrimSpace(item.VersionID)})
			if len(out) >= maxCatalogModels {
				return out, nil
			}
		}
		pageToken = strings.TrimSpace(list.NextPageToken)
		if pageToken == "" {
			break
		}
	}
	return out, nil
}

// parsePublisherModelName extracts the publisher and model ID from a resource name
// such as "publishers/anthropic/models/claude-sonnet-4".
func parsePublisherModelName(name string) (publisher, upstreamID string, ok bool) {
	name = strings.TrimSpace(name)
	const modelsSep = "/models/"
	idx := strings.Index(name, modelsSep)
	if !strings.HasPrefix(name, "publishers/") || idx < 0 {
		return "", "", false
	}
	publisher = strings.ToLower(strings.TrimPrefix(name[:idx], "publishers/"))
	upstreamID = name[idx+len(modelsSep):]
	if publisher == "" || upstreamID == "" || strings.Contains(upstreamID, "/") {
		return "", "", false
	}
	return publisher, upstreamID, true
}

func hasFamilyPrefix(upstreamID string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	lower := strings.ToLower(upstreamID)
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func dedupeCatalog(models []catalogModel) []catalogModel {
	seen := make(map[string]struct{}, len(models))
	out := make([]catalogModel, 0, len(models))
	for _, model := range models {
		key := model.Publisher + "/" + strings.ToLower(model.UpstreamID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	return out
}

func filterCatalog(models []catalogModel, excludedPrefixes []string) []catalogModel {
	if len(excludedPrefixes) == 0 {
		return models
	}
	out := make([]catalogModel, 0, len(models))
	for _, model := range models {
		id := strings.ToLower(model.Publisher + "/" + model.UpstreamID)
		bare := strings.ToLower(model.UpstreamID)
		excluded := false
		for _, prefix := range excludedPrefixes {
			if strings.HasPrefix(id, prefix) || strings.HasPrefix(bare, prefix) {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, model)
		}
	}
	return out
}

// modelsFromConfig derives the catalog directly from the explicit allowlist,
// bypassing Vertex discovery entirely.
func modelsFromConfig(cfg Config) []catalogModel {
	out := make([]catalogModel, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		publisher, upstreamID, ok := splitPublisherModel(m.Name)
		if !ok {
			continue
		}
		out = append(out, catalogModel{Publisher: publisher, UpstreamID: upstreamID})
	}
	return out
}

// googleErrorDetail extracts a short, non-sensitive reason from a Google API
// error body for diagnostics. It returns an empty string when nothing parses.
func googleErrorDetail(body []byte) string {
	var parsed struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	message := strings.TrimSpace(parsed.Error.Message)
	if message == "" {
		return ""
	}
	if len(message) > 300 {
		message = message[:300] + "..."
	}
	status := strings.TrimSpace(parsed.Error.Status)
	if status != "" {
		return fmt.Sprintf(" %s: %s", status, message)
	}
	return " " + message
}

func cloneCatalog(in []catalogModel) []catalogModel {
	out := make([]catalogModel, len(in))
	copy(out, in)
	return out
}

func modelInfos(models []catalogModel, cfg Config) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		owner := "vertex-" + model.Publisher
		generationMethods := []string{generationMethod(model.Publisher)}
		for _, alias := range cfg.modelAliases(model.Publisher, model.UpstreamID) {
			out = append(out, pluginapi.ModelInfo{
				ID:                         alias,
				Object:                     "model",
				OwnedBy:                    owner,
				Type:                       "chat",
				DisplayName:                model.UpstreamID,
				Name:                       alias,
				Version:                    model.VersionID,
				Description:                modelDescription(model),
				SupportedGenerationMethods: generationMethods,
				SupportedParameters:        []string{"stream", "tools", "tool_choice"},
				SupportedInputModalities:   []string{"TEXT", "IMAGE"},
				SupportedOutputModalities:  []string{"TEXT"},
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func generationMethod(publisher string) string {
	if publisher == PublisherAnthropic {
		return "rawPredict"
	}
	return "generateContent"
}

func modelDescription(model catalogModel) string {
	family := "Gemini"
	if model.Publisher == PublisherAnthropic {
		family = "Claude"
	}
	return fmt.Sprintf("%s model served by Vertex AI (publisher %s)", family, model.Publisher)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
