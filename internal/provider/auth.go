package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// cloudPlatformScope is the OAuth scope required to call the Vertex AI API.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// adcCredentials holds a resolved Application Default Credentials token source
// plus the project identifiers Vertex requires. ADC access tokens are not
// project-bound, so every request must also carry x-goog-user-project.
type adcCredentials struct {
	source       oauth2.TokenSource
	projectID    string // used in the Vertex request URL path
	quotaProject string // used in the x-goog-user-project header
}

// credentials lazily resolves ADC once per Service generation. A new Service is
// created on every config change, so sync.Once scoping to the Service is correct.
func (s *Service) credentials(ctx context.Context) (*adcCredentials, error) {
	s.authOnce.Do(func() {
		s.adc, s.adcErr = s.buildCredentials(ctx)
	})
	return s.adc, s.adcErr
}

func (s *Service) buildCredentials(ctx context.Context) (*adcCredentials, error) {
	cfg := s.Config()
	ctx = contextWithProxy(ctx, cfg.ProxyURL)

	var (
		creds   *google.Credentials
		err     error
		rawJSON []byte
	)
	if cfg.Credentials != "" {
		path := strings.TrimSpace(strings.TrimPrefix(cfg.Credentials, "file:"))
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, statusError("credentials_error", "cannot read credentials file", 500)
		}
		rawJSON = data
		creds, err = google.CredentialsFromJSON(ctx, data, cloudPlatformScope)
	} else {
		creds, err = google.FindDefaultCredentials(ctx, cloudPlatformScope)
		if creds != nil {
			rawJSON = creds.JSON
		}
	}
	if err != nil || creds == nil {
		return nil, statusError("credentials_error", "application default credentials are not available; run 'gcloud auth application-default login' or configure a workload identity", 500)
	}

	project := firstNonEmpty(
		cfg.ProjectID,
		os.Getenv("ANTHROPIC_VERTEX_PROJECT_ID"),
		os.Getenv("GOOGLE_CLOUD_PROJECT"),
		os.Getenv("GCLOUD_PROJECT"),
		creds.ProjectID,
	)
	if project == "" {
		return nil, statusError("config_error", "project_id is required (set project_id, ANTHROPIC_VERTEX_PROJECT_ID, or an ADC project)", 500)
	}

	quota := firstNonEmpty(
		cfg.QuotaProjectID,
		os.Getenv("GOOGLE_CLOUD_QUOTA_PROJECT"),
		adcQuotaProject(rawJSON),
	)
	if quota == "" {
		// ADC tokens are not project-bound; fall back to the request project so
		// Vertex can attribute service usage instead of rejecting the call.
		quota = project
	}

	return &adcCredentials{source: creds.TokenSource, projectID: project, quotaProject: quota}, nil
}

// token returns a current ADC access token. The google token source caches and
// refreshes transparently, so repeated calls are cheap.
func (a *adcCredentials) token() (string, error) {
	tok, err := a.source.Token()
	if err != nil {
		return "", statusError("credentials_error", "cannot obtain an ADC access token", 502)
	}
	if tok == nil || !tok.Valid() {
		return "", statusError("credentials_error", "ADC returned an invalid access token", 502)
	}
	return tok.AccessToken, nil
}

// applyAuthHeader sets the Vertex ADC authentication headers on h. It attaches
// the bearer token, removes any inherited API-key header, and sets the quota
// project so service usage is attributed correctly.
func applyAuthHeader(h http.Header, token, quotaProject string) {
	h.Set("Authorization", "Bearer "+token)
	h.Del("X-Goog-Api-Key")
	if quotaProject != "" {
		h.Set("X-Goog-User-Project", quotaProject)
	}
}

// adcQuotaProject extracts quota_project_id from raw ADC JSON. The oauth2
// credentials struct does not expose it, so it is parsed directly.
func adcQuotaProject(rawCreds []byte) string {
	if len(rawCreds) == 0 {
		return ""
	}
	var parsed struct {
		QuotaProjectID string `json:"quota_project_id"`
	}
	if err := json.Unmarshal(rawCreds, &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.QuotaProjectID)
}

// contextWithProxy injects an HTTP client honoring proxyURL into ctx so the ADC
// token exchange routes through the configured proxy. An empty proxyURL is a
// no-op and keeps the default transport.
func contextWithProxy(ctx context.Context, proxyURL string) context.Context {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return ctx
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return ctx
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(parsed)}}
	return context.WithValue(ctx, oauth2.HTTPClient, client)
}
