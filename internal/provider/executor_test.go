package provider

import (
	"net/http"
	"testing"
)

func TestVertexHost(t *testing.T) {
	if got := vertexHost("global"); got != "https://aiplatform.googleapis.com" {
		t.Errorf("global host = %q", got)
	}
	if got := vertexHost(""); got != "https://aiplatform.googleapis.com" {
		t.Errorf("empty host = %q", got)
	}
	if got := vertexHost("us-east5"); got != "https://us-east5-aiplatform.googleapis.com" {
		t.Errorf("regional host = %q", got)
	}
}

func TestVertexModelURL(t *testing.T) {
	anthropic := resolvedModel{Publisher: "anthropic", UpstreamID: "claude-sonnet-4@20250514"}
	got := vertexModelURL(vertexHost("global"), "my-proj", "global", anthropic, "streamRawPredict", true)
	want := "https://aiplatform.googleapis.com/v1/projects/my-proj/locations/global/publishers/anthropic/models/claude-sonnet-4@20250514:streamRawPredict"
	if got != want {
		t.Errorf("anthropic URL = %q", got)
	}

	gemini := resolvedModel{Publisher: "google", UpstreamID: "gemini-2.5-pro"}
	got = vertexModelURL(vertexHost("us-east5"), "my-proj", "us-east5", gemini, "streamGenerateContent", true)
	want = "https://us-east5-aiplatform.googleapis.com/v1/projects/my-proj/locations/us-east5/publishers/google/models/gemini-2.5-pro:streamGenerateContent?alt=sse"
	if got != want {
		t.Errorf("gemini stream URL = %q", got)
	}

	got = vertexModelURL(vertexHost("global"), "my-proj", "global", gemini, "generateContent", false)
	if want := "https://aiplatform.googleapis.com/v1/projects/my-proj/locations/global/publishers/google/models/gemini-2.5-pro:generateContent"; got != want {
		t.Errorf("gemini non-stream URL = %q", got)
	}
}

func TestTargetForPublisher(t *testing.T) {
	a, err := targetForPublisher(PublisherAnthropic)
	if err != nil || a.action != "rawPredict" || a.streamAction != "streamRawPredict" {
		t.Errorf("anthropic target = %+v err=%v", a, err)
	}
	g, err := targetForPublisher(PublisherGoogle)
	if err != nil || g.action != "generateContent" || g.streamAction != "streamGenerateContent" {
		t.Errorf("google target = %+v err=%v", g, err)
	}
	if _, err := targetForPublisher("meta"); err == nil {
		t.Error("expected error for unsupported publisher")
	}
}

func TestNormalizeRequestFormat(t *testing.T) {
	cases := map[string]string{
		"openai":           "openai",
		"chat-completions": "openai",
		"claude":           "claude",
		"anthropic":        "claude",
		"gemini":           "gemini",
		"responses":        "openai-response",
		"nonsense":         "",
	}
	for in, want := range cases {
		if got := normalizeRequestFormat(in); got != want {
			t.Errorf("normalizeRequestFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyAuthHeader(t *testing.T) {
	h := http.Header{"X-Goog-Api-Key": {"secret"}}
	applyAuthHeader(h, "tok", "quota-proj")
	if h["Authorization"][0] != "Bearer tok" {
		t.Errorf("Authorization = %v", h["Authorization"])
	}
	if _, ok := h["X-Goog-Api-Key"]; ok {
		t.Error("X-Goog-Api-Key should be removed")
	}
	if h["X-Goog-User-Project"][0] != "quota-proj" {
		t.Errorf("X-Goog-User-Project = %v", h["X-Goog-User-Project"])
	}
}

func TestEmitPayloads(t *testing.T) {
	// Claude: forwarded verbatim (named events).
	raw := []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	got := emitPayloads("claude", raw)
	if len(got) != 1 || string(got[0]) != string(raw) {
		t.Errorf("claude should be verbatim, got %q", got)
	}
	// OpenAI: bare JSON extracted (host adds data: framing).
	got = emitPayloads("openai", []byte("data: {\"a\":1}\n\n"))
	if len(got) != 1 || string(got[0]) != `{"a":1}` {
		t.Errorf("openai bare payload = %q", got)
	}
	// DONE sentinel passes through bare.
	got = emitPayloads("openai", []byte("data: [DONE]\n\n"))
	if len(got) != 1 || string(got[0]) != "[DONE]" {
		t.Errorf("done payload = %q", got)
	}
}
