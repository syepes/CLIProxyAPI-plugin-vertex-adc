package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"cliproxyapi-vertex-adc/internal/provider"
	"cliproxyapi-vertex-adc/internal/transport"
)

const ID = "vertex-adc"

var (
	Version    = "0.1.0"
	Repository = "UNCONFIGURED"
)

type Plugin struct {
	mu          sync.RWMutex
	configureMu sync.Mutex
	service     *provider.Service
	host        transport.Caller
	config      []byte
	configError string
	closed      bool
}

func New(host transport.Caller) *Plugin { return &Plugin{host: host} }
func (p *Plugin) Close() {
	p.configureMu.Lock()
	defer p.configureMu.Unlock()
	p.mu.Lock()
	s := p.service
	p.service = nil
	p.closed = true
	p.mu.Unlock()
	if s != nil {
		s.Shutdown()
	}
}

func (p *Plugin) Dispatch(method string, raw []byte) (out []byte) {
	defer func() {
		if recover() != nil {
			out = errorEnvelope("internal_error", "plugin operation failed safely", 500, false)
		}
	}()
	out, _ = p.handleMethod(method, raw)
	return
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ManagementAPI         bool                         `json:"management_api"`
	ModelProvider         bool                         `json:"model_provider"`
	ModelRouter           bool                         `json:"model_router"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

func (p *Plugin) handleMethod(method string, request []byte) ([]byte, bool) {
	result, errHandle := p.dispatch(method, request)
	if errHandle != nil {
		var statusErr *provider.StatusError
		if errors.As(errHandle, &statusErr) {
			return errorEnvelope(statusErr.Code, statusErr.Message, statusErr.HTTPStatus, statusErr.Retryable), true
		}
		return errorEnvelope("plugin_error", errHandle.Error(), http.StatusInternalServerError, false), true
	}
	raw, errEnvelope := okEnvelope(result)
	if errEnvelope != nil {
		return errorEnvelope("encoding_error", errEnvelope.Error(), http.StatusInternalServerError, false), true
	}
	return raw, false
}

func (p *Plugin) dispatch(method string, request []byte) (any, error) {
	ctx := context.Background()
	p.mu.RLock()
	pluginService := p.service
	p.mu.RUnlock()
	if pluginService != nil {
		ctx = pluginService.Context()
	}
	if pluginService == nil && method != "plugin.register" && method != "plugin.reconfigure" && method != "plugin.quiesce" && method != "plugin.shutdown" && method != "management.register" && method != "management.handle" && method != "model.static" && method != pluginabi.MethodModelRoute {
		return nil, &provider.StatusError{Code: "unavailable", Message: "plugin is disabled or not configured", HTTPStatus: 503}
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
				return nil, errUnmarshal
			}
		}
		if req.SchemaVersion < 6 {
			return nil, &provider.StatusError{Code: "unsupported_host", Message: "CLIProxyAPI schema 6 is required", HTTPStatus: 400}
		}
		p.configureMu.Lock()
		defer p.configureMu.Unlock()
		p.mu.RLock()
		same := p.service != nil && bytes.Equal(p.config, req.ConfigYAML)
		p.mu.RUnlock()
		if same {
			return pluginRegistration(), nil
		}
		next := provider.New(transport.New(p.host))
		err := next.Configure(req.ConfigYAML)
		if err != nil || !next.Config().Enabled {
			next.Shutdown()
			next = nil
		}
		p.mu.Lock()
		old := p.service
		p.service = next
		p.config = append([]byte(nil), req.ConfigYAML...)
		p.configError = ""
		if err != nil {
			p.configError = err.Error()
		}
		p.mu.Unlock()
		if old != nil {
			old.Shutdown()
		}
		if next != nil {
			next.Start()
		}

		return pluginRegistration(), nil
	case "plugin.quiesce", "plugin.shutdown":
		p.Close()
		return struct{}{}, nil
	case "management.register":
		return managementRegistration(), nil
	case "management.handle":
		return p.manage(request)
	case pluginabi.MethodModelStatic:
		if pluginService == nil {
			return pluginapi.ModelResponse{Provider: ID, Models: []pluginapi.ModelInfo{}}, nil
		}
		return pluginService.StaticModels(), nil
	case pluginabi.MethodModelRoute:
		var req pluginapi.ModelRouteRequest
		if len(request) > 0 {
			if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
				return nil, errUnmarshal
			}
		}
		if pluginService == nil || !pluginService.Config().Handles(req.RequestedModel) {
			return pluginapi.ModelRouteResponse{Handled: false}, nil
		}
		// Static ADC executor: route claimed models to our own executor so the
		// host does not require a built-in auth credential for this provider.
		return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetSelf}, nil
	case pluginabi.MethodExecutorIdentifier:
		return identifierResponse{Identifier: ID}, nil
	case pluginabi.MethodExecutorExecute:
		var req provider.ExecuteRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return pluginService.Execute(ctx, req)
	case pluginabi.MethodExecutorExecuteStream:
		var req provider.ExecuteRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		headers, errStream := pluginService.ExecuteStream(ctx, req)
		if errStream != nil {
			return nil, errStream
		}
		return map[string]any{"headers": headers}, nil
	case pluginabi.MethodExecutorCountTokens:
		var req provider.ExecuteRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return pluginService.CountTokensChecked(ctx, req)
	case pluginabi.MethodExecutorHTTPRequest:
		var req provider.HTTPRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return pluginService.HTTP(ctx, req)
	default:
		return nil, &provider.StatusError{
			Code:       "unknown_method",
			Message:    "unknown plugin method: " + method,
			HTTPStatus: http.StatusNotImplemented,
		}
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Vertex AI (ADC) Provider",
			Version:          Version,
			Author:           "syepes",
			GitHubRepository: Repository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "project_id", Type: pluginapi.ConfigFieldTypeString, Description: "Google Cloud project used in the Vertex request path. Falls back to ANTHROPIC_VERTEX_PROJECT_ID, GOOGLE_CLOUD_PROJECT, or the ADC project."},
				{Name: "location", Type: pluginapi.ConfigFieldTypeString, Description: "Vertex location, e.g. global (default) or a region like us-east5."},
				{Name: "quota_project_id", Type: pluginapi.ConfigFieldTypeString, Description: "Quota/billing project for the x-goog-user-project header. Defaults to GOOGLE_CLOUD_QUOTA_PROJECT, the ADC quota_project_id, then project_id."},
				{Name: "credentials", Type: pluginapi.ConfigFieldTypeString, Description: "Optional path (file:/abs/path) to an ADC JSON file. Empty uses standard Application Default Credentials resolution."},
				{Name: "publishers", Type: pluginapi.ConfigFieldTypeArray, Description: "Vertex publishers to surface: anthropic and/or google (default both)."},
				{Name: "model_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix for discovered model aliases (default vertex/)."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional model allowlist: objects with publisher-qualified name (e.g. anthropic/claude-sonnet-4@20250514) and optional alias. Empty auto-discovers."},
				{Name: "models_excluded", Type: pluginapi.ConfigFieldTypeArray, Description: "Case-insensitive model ID prefixes omitted from discovery."},
				{Name: "model_cache_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "In-memory model catalog cache lifetime (30-3600)."},
				{Name: "proxy_url", Type: pluginapi.ConfigFieldTypeString, Description: "Optional proxy URL for the ADC token exchange."},
			},
		},
		Capabilities: registrationCapability{
			ManagementAPI:         true,
			ModelProvider:         true,
			ModelRouter:           true,
			AuthProvider:          false,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeStatic,
			ExecutorInputFormats:  []string{"openai", "openai-response", "claude", "gemini"},
			ExecutorOutputFormats: []string{"openai", "openai-response", "claude", "gemini"},
		},
	}
}

func okEnvelope(value any) ([]byte, error) {
	result, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string, status int, retryable bool) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:       code,
			Message:    message,
			HTTPStatus: status,
			Retryable:  retryable,
		},
	})
	return raw
}
