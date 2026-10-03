package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestRequestForVertexClaudePassthrough(t *testing.T) {
	body := []byte(`{"model":"should-be-removed","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
	out, err := RequestForVertex("claude", EndpointMessages, "claude-sonnet-4", body, true)
	if err != nil {
		t.Fatalf("RequestForVertex: %v", err)
	}
	var value map[string]any
	if err := json.Unmarshal(out, &value); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := value["model"]; ok {
		t.Error("model must be stripped for Vertex")
	}
	if value["anthropic_version"] != "vertex-2023-10-16" {
		t.Errorf("anthropic_version = %v", value["anthropic_version"])
	}
	if value["stream"] != true {
		t.Errorf("stream = %v, want true", value["stream"])
	}
}

func TestRequestForVertexOpenAIToGemini(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	out, err := RequestForVertex("openai", EndpointGemini, "gemini-2.5-pro", body, true)
	if err != nil {
		t.Fatalf("RequestForVertex: %v", err)
	}
	var value map[string]any
	if err := json.Unmarshal(out, &value); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := value["model"]; ok {
		t.Error("model must be stripped for Vertex")
	}
	if _, ok := value["stream"]; ok {
		t.Error("stream must be stripped for the Gemini surface")
	}
	if _, ok := value["contents"]; !ok {
		t.Errorf("expected Gemini contents field, got %v", value)
	}
}

func TestResponseForVertexGeminiToOpenAI(t *testing.T) {
	original := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	translated, err := RequestForVertex("openai", EndpointGemini, "gemini-2.5-pro", original, false)
	if err != nil {
		t.Fatalf("RequestForVertex: %v", err)
	}
	geminiResp := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
	out, err := ResponseForVertex(context.Background(), EndpointGemini, "openai", "gemini-2.5-pro", original, translated, geminiResp)
	if err != nil {
		t.Fatalf("ResponseForVertex: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("response not valid JSON: %s", out)
	}
	if !bytes.Contains(out, []byte("OK")) {
		t.Errorf("translated response missing content: %s", out)
	}
}

func TestStreamForVertexGeminiToOpenAI(t *testing.T) {
	original := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	translated, err := RequestForVertex("openai", EndpointGemini, "gemini-2.5-pro", original, true)
	if err != nil {
		t.Fatalf("RequestForVertex: %v", err)
	}
	frame := []byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]}}]}`)
	var state any
	out, err := StreamForVertex(context.Background(), EndpointGemini, "openai", "gemini-2.5-pro", original, translated, frame, &state)
	if err != nil {
		t.Fatalf("StreamForVertex: %v", err)
	}
	joined := bytes.Join(out, nil)
	if !bytes.Contains(joined, []byte("OK")) {
		t.Errorf("translated stream missing content: %s", joined)
	}
}

func TestEndpointFormatGemini(t *testing.T) {
	f, err := endpointFormat(EndpointGemini)
	if err != nil || f.String() != "gemini" {
		t.Errorf("endpointFormat(gemini) = %q err=%v", f, err)
	}
}
