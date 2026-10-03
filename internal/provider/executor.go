package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"cliproxyapi-vertex-adc/internal/sse"
	"cliproxyapi-vertex-adc/internal/translate"
	"cliproxyapi-vertex-adc/internal/transport"
)

type ExecuteRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type HTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// vertexTarget describes the upstream surface and translation format for a
// resolved model, derived from its publisher.
type vertexTarget struct {
	publisher    string
	endpoint     string // translate endpoint token (format discriminator)
	action       string // non-streaming action verb
	streamAction string // streaming action verb
}

func targetForPublisher(publisher string) (vertexTarget, error) {
	switch publisher {
	case PublisherAnthropic:
		return vertexTarget{publisher: publisher, endpoint: translate.EndpointMessages, action: "rawPredict", streamAction: "streamRawPredict"}, nil
	case PublisherGoogle:
		return vertexTarget{publisher: publisher, endpoint: translate.EndpointGemini, action: "generateContent", streamAction: "streamGenerateContent"}, nil
	default:
		return vertexTarget{}, statusError("model_not_found", "unsupported publisher "+publisher, 404)
	}
}

func (s *Service) Execute(ctx context.Context, req ExecuteRequest) (pluginapi.ExecutorResponse, error) {
	sourceFormat := normalizeRequestFormat(firstNonEmpty(req.SourceFormat, req.Format))
	if sourceFormat == "" {
		return pluginapi.ExecutorResponse{}, statusError("unsupported_format", fmt.Sprintf("unsupported request format %q", firstNonEmpty(req.SourceFormat, req.Format)), http.StatusUnprocessableEntity)
	}
	resolved, errModel := s.Config().resolveModel(req.Model)
	if errModel != nil {
		return pluginapi.ExecutorResponse{}, errModel
	}
	target, errTarget := targetForPublisher(resolved.Publisher)
	if errTarget != nil {
		return pluginapi.ExecutorResponse{}, errTarget
	}
	creds, errCreds := s.credentials(ctx)
	if errCreds != nil {
		return pluginapi.ExecutorResponse{}, errCreds
	}
	requestBody, errTranslate := translate.RequestForVertex(sourceFormat, target.endpoint, resolved.UpstreamID, req.Payload, false)
	if errTranslate != nil {
		return pluginapi.ExecutorResponse{}, statusError("translation_error", errTranslate.Error(), http.StatusUnprocessableEntity)
	}
	cfg := s.Config()
	endpointURL := vertexModelURL(cfg.apiBase(), creds.projectID, cfg.Location, resolved, target.action, false)
	resp, errDo := s.doModelRequest(ctx, req.HostCallbackID, creds, endpointURL, requestBody, false)
	if errDo != nil {
		return pluginapi.ExecutorResponse{}, errDo
	}
	body, errResponse := translate.ResponseForVertex(ctx, target.endpoint, sourceFormat, req.Model, req.OriginalRequest, requestBody, resp.Body)
	if errResponse != nil {
		return pluginapi.ExecutorResponse{}, statusError("translation_error", errResponse.Error(), http.StatusBadGateway)
	}
	body, errResponse = rewriteResponseModel(body, req.Model)
	if errResponse != nil {
		return pluginapi.ExecutorResponse{}, statusError("translation_error", "cannot encode namespaced response", 502)
	}
	return pluginapi.ExecutorResponse{
		Payload: body,
		Headers: filterResponseHeaders(resp.Headers),
		Metadata: map[string]any{
			"vertex_publisher": resolved.Publisher,
			"vertex_model":     resolved.UpstreamID,
		},
	}, nil
}

func (s *Service) ExecuteStream(ctx context.Context, req ExecuteRequest) (http.Header, error) {
	if strings.TrimSpace(req.StreamID) == "" {
		return nil, statusError("invalid_request", "stream_id is required", http.StatusBadRequest)
	}
	sourceFormat := normalizeRequestFormat(firstNonEmpty(req.SourceFormat, req.Format))
	if sourceFormat == "" {
		return nil, statusError("unsupported_format", fmt.Sprintf("unsupported request format %q", firstNonEmpty(req.SourceFormat, req.Format)), http.StatusUnprocessableEntity)
	}
	resolved, errModel := s.Config().resolveModel(req.Model)
	if errModel != nil {
		return nil, errModel
	}
	target, errTarget := targetForPublisher(resolved.Publisher)
	if errTarget != nil {
		return nil, errTarget
	}
	creds, errCreds := s.credentials(ctx)
	if errCreds != nil {
		return nil, errCreds
	}
	requestBody, errTranslate := translate.RequestForVertex(sourceFormat, target.endpoint, resolved.UpstreamID, req.Payload, true)
	if errTranslate != nil {
		return nil, statusError("translation_error", errTranslate.Error(), http.StatusUnprocessableEntity)
	}
	cfg := s.Config()
	endpointURL := vertexModelURL(cfg.apiBase(), creds.projectID, cfg.Location, resolved, target.streamAction, true)
	upstream, errOpen := s.openModelStream(ctx, req.HostCallbackID, creds, endpointURL, requestBody)
	if errOpen != nil {
		return nil, errOpen
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		body, errCollect := s.collectStreamError(ctx, upstream)
		if errCollect != nil {
			return nil, errCollect
		}
		_ = body
		return nil, upstreamStatusError(upstream.StatusCode, "stream rejected")
	}
	if !s.spawn(func() {
		s.pumpStream(req.StreamID, target.endpoint, sourceFormat, req.Model, req.OriginalRequest, requestBody, upstream)
	}) {
		_ = s.host.CloseStream(ctx, upstream.ID)
		return nil, errors.New("plugin shutting down")
	}
	headers := filterResponseHeaders(upstream.Headers)
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	return headers, nil
}

func (s *Service) doModelRequest(ctx context.Context, callbackID string, creds *adcCredentials, endpointURL string, body []byte, stream bool) (transport.Response, error) {
	token, errToken := creds.token()
	if errToken != nil {
		return transport.Response{}, errToken
	}
	request := transport.Request{
		Method:  http.MethodPost,
		URL:     endpointURL,
		Headers: vertexHeaders(token, creds.quotaProject, stream),
		Body:    body,
	}
	resp, errDo := s.host.Do(ctx, callbackID, request)
	if errDo != nil {
		return transport.Response{}, fmt.Errorf("call Vertex model endpoint: %w", errDo)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if token, errToken = creds.token(); errToken != nil {
			return transport.Response{}, errToken
		}
		request.Headers = vertexHeaders(token, creds.quotaProject, stream)
		resp, errDo = s.host.Do(ctx, callbackID, request)
		if errDo != nil {
			return transport.Response{}, fmt.Errorf("call Vertex model endpoint after token refresh: %w", errDo)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, upstreamStatusError(resp.StatusCode, "request rejected")
	}
	return resp, nil
}

func (s *Service) openModelStream(ctx context.Context, callbackID string, creds *adcCredentials, endpointURL string, body []byte) (transport.Stream, error) {
	token, errToken := creds.token()
	if errToken != nil {
		return transport.Stream{}, errToken
	}
	request := transport.Request{
		Method:  http.MethodPost,
		URL:     endpointURL,
		Headers: vertexHeaders(token, creds.quotaProject, true),
		Body:    body,
	}
	stream, errOpen := s.host.OpenStream(ctx, callbackID, request)
	if errOpen != nil {
		return transport.Stream{}, fmt.Errorf("open Vertex model stream: %w", errOpen)
	}
	if stream.StatusCode == http.StatusUnauthorized {
		_ = s.host.CloseStream(ctx, stream.ID)
		if token, errToken = creds.token(); errToken != nil {
			return transport.Stream{}, errToken
		}
		request.Headers = vertexHeaders(token, creds.quotaProject, true)
		stream, errOpen = s.host.OpenStream(ctx, callbackID, request)
		if errOpen != nil {
			return transport.Stream{}, fmt.Errorf("open Vertex model stream after token refresh: %w", errOpen)
		}
	}
	return stream, nil
}

func (s *Service) collectStreamError(ctx context.Context, stream transport.Stream) ([]byte, error) {
	defer func() { _ = s.host.CloseStream(context.Background(), stream.ID) }()
	var body []byte
	for {
		chunk, errRead := s.host.ReadStream(ctx, stream.ID)
		if errRead != nil {
			return nil, fmt.Errorf("read Vertex error stream: %w", errRead)
		}
		if len(body)+len(chunk.Payload) > 1<<20 {
			return nil, errors.New("upstream error body too large")
		}
		body = append(body, chunk.Payload...)
		if chunk.Error != "" {
			return nil, errors.New("upstream error stream transport failed")
		}
		if chunk.Done {
			return body, nil
		}
	}
}

func (s *Service) pumpStream(outputID, endpoint, destination, model string, original, translated []byte, upstream transport.Stream) {
	ctx := s.ctx
	var terminalErr error
	defer func() {
		if recover() != nil {
			terminalErr = errors.New("upstream stream processing failed")
		}
		_ = s.host.CloseStream(ctx, upstream.ID)
		message := ""
		if terminalErr != nil {
			message = "vertex stream failed or was canceled" // Upstream translation errors may contain private response text.
		}
		s.host.CloseOutput(ctx, outputID, message)
	}()

	decoder := &sse.Decoder{}
	var state any
	terminal := false
	// Gemini streams end on connection close with no sentinel event, so the
	// final-chunk completeness check only applies to the Claude message stream.
	requireTerminal := endpoint == translate.EndpointMessages
	emit := func(frame []byte) error {
		for _, line := range bytes.Split(frame, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			var event struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &event) == nil {
				switch event.Type {
				case "message_stop":
					terminal = endpoint == translate.EndpointMessages
				case "error":
					return errors.New("upstream stream failed")
				}
			}
		}
		frames, errTranslate := translate.StreamForVertex(ctx, endpoint, destination, model, original, translated, frame, &state)
		if errTranslate != nil {
			return errTranslate
		}
		for _, output := range frames {
			if len(output) == 0 {
				continue
			}
			output, errTranslate = rewriteStreamModel(output, model)
			if errTranslate != nil {
				return errTranslate
			}
			// The Claude client stream is written verbatim by the host (named SSE
			// events). Other client formats are re-framed by the host's SSE writer,
			// which adds the "data:" prefix, so emit the bare event payload only.
			for _, payload := range emitPayloads(destination, output) {
				if len(payload) == 0 {
					continue
				}
				if errEmit := s.host.Emit(ctx, outputID, payload); errEmit != nil {
					return errEmit
				}
			}
		}
		return nil
	}

	for {
		chunk, errRead := s.host.ReadStream(ctx, upstream.ID)
		if errRead != nil {
			terminalErr = fmt.Errorf("read Vertex stream: %w", errRead)
			return
		}
		if chunk.Error != "" {
			terminalErr = errors.New("upstream stream transport failed")
			return
		}
		if decoder.Buffered()+len(chunk.Payload) > 8<<20 {
			terminalErr = errors.New("upstream stream event too large")
			return
		}
		for _, frame := range decoder.Feed(chunk.Payload) {
			if errEmit := emit(frame); errEmit != nil {
				terminalErr = fmt.Errorf("translate Vertex stream: %w", errEmit)
				return
			}
		}
		if chunk.Done {
			if len(bytes.TrimSpace(decoder.Flush())) > 0 || (requireTerminal && !terminal) {
				terminalErr = errors.New("upstream stream ended without a terminal event")
			}
			return
		}
	}
}

// emitPayloads converts a translated SSE frame into the chunks to emit for the
// client format. Claude is forwarded verbatim (named events written as-is by the
// host); other formats emit the bare event payload because the host's SSE writer
// adds the "data:" framing for them.
func emitPayloads(destination string, frame []byte) [][]byte {
	if destination == "claude" {
		return [][]byte{frame}
	}
	var out [][]byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(value) == 0 {
			continue
		}
		out = append(out, value)
	}
	return out
}

func normalizeRequestFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "responses", "openai-response", "openai-responses":
		return "openai-response"
	case "openai", "chat", "chat-completions":
		return "openai"
	case "claude", "anthropic":
		return "claude"
	case "gemini":
		return "gemini"
	default:
		return ""
	}
}

func (s *Service) HTTP(context.Context, HTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return pluginapi.ExecutorHTTPResponse{}, statusError("unsupported_operation", "arbitrary HTTP forwarding is disabled", 501)
}

// CountTokensChecked is unsupported; Vertex token counting is not proxied.
func (s *Service) CountTokensChecked(context.Context, ExecuteRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, statusError("unsupported_operation", "token counting is not supported", http.StatusNotImplemented)
}

func vertexHeaders(token, quotaProject string, stream bool) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	applyAuthHeader(headers, token, quotaProject)
	return headers
}

func filterResponseHeaders(headers http.Header) http.Header {
	out := http.Header{}
	for key, values := range headers {
		switch strings.ToLower(key) {
		case "content-type", "cache-control", "retry-after", "x-request-id":
			out[key] = append([]string(nil), values...)
		}
	}
	return out
}

// vertexHost returns the Vertex AI API host for a location. The global location
// uses the unprefixed host; regional locations use the region-prefixed host.
func vertexHost(location string) string {
	location = strings.TrimSpace(location)
	if location == "" || strings.EqualFold(location, "global") {
		return "https://aiplatform.googleapis.com"
	}
	return "https://" + location + "-aiplatform.googleapis.com"
}

// vertexModelURL builds the Vertex model endpoint URL. Model IDs are validated to
// contain no path separators upstream, so they are embedded directly (the "@"
// version separator must not be percent-encoded).
func vertexModelURL(base, project, location string, model resolvedModel, action string, stream bool) string {
	loc := strings.TrimSpace(location)
	if loc == "" {
		loc = defaultLocation
	}
	url := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/%s/models/%s:%s", base, project, loc, model.Publisher, model.UpstreamID, action)
	if stream && model.Publisher == PublisherGoogle {
		url += "?alt=sse"
	}
	return url
}
