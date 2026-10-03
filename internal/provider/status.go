package provider

import (
	"context"
	"time"
)

// StatusReport is a secret-free summary of the plugin's runtime state for the
// management dashboard.
type StatusReport struct {
	Configured       bool      `json:"configured"`
	Enabled          bool      `json:"enabled"`
	ProjectID        string    `json:"project_id"`
	QuotaProjectID   string    `json:"quota_project_id"`
	Location         string    `json:"location"`
	Publishers       []string  `json:"publishers"`
	ModelPrefix      string    `json:"model_prefix"`
	CredentialsReady bool      `json:"credentials_ready"`
	CredentialsError string    `json:"credentials_error,omitempty"`
	ModelCount       int       `json:"model_count"`
	CatalogError     string    `json:"catalog_error,omitempty"`
	CatalogFetchedAt time.Time `json:"catalog_fetched_at,omitempty"`
	CatalogExpiresAt time.Time `json:"catalog_expires_at,omitempty"`
}

// Status returns a snapshot of configuration, ADC availability, and the model
// catalog. It never exposes tokens or credential contents.
func (s *Service) Status() StatusReport {
	cfg := s.Config()
	report := StatusReport{
		Configured:  true,
		Enabled:     cfg.Enabled,
		Location:    cfg.Location,
		Publishers:  cfg.Publishers,
		ModelPrefix: cfg.ModelPrefix,
	}

	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	if creds, err := s.credentials(ctx); err == nil {
		report.CredentialsReady = true
		report.ProjectID = creds.projectID
		report.QuotaProjectID = creds.quotaProject
	} else {
		report.CredentialsError = err.Error()
	}

	if len(cfg.Models) > 0 {
		// Explicit allowlist is served without discovery.
		report.ModelCount = len(modelsFromConfig(cfg))
		return report
	}

	s.modelMu.Lock()
	cache := s.modelCache
	s.modelMu.Unlock()
	if cache != nil {
		report.ModelCount = len(cache.models)
		report.CatalogFetchedAt = cache.fetchedAt
		report.CatalogExpiresAt = cache.expiresAt
		if cache.err != "" {
			report.CatalogError = cache.err
		}
	}
	return report
}

// ForceRefresh re-discovers the model catalog, bypassing the cache.
func (s *Service) ForceRefresh(ctx context.Context) error {
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := s.catalog(refreshCtx, true)
	return err
}
