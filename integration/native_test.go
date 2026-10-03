// Package integration loads the actual shared library in an isolated CLIProxyAPI process.
//
// ADC is simulated with a synthetic service-account key whose token_uri points at
// the mock server, so the google oauth2 flow mints a token entirely offline. The
// Vertex API host is overridden with api_endpoint + allow_insecure_base_url.
package integration

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func syntheticServiceAccount(t *testing.T, tokenURI string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sa := map[string]any{
		"type":                        "service_account",
		"project_id":                  "test-proj",
		"private_key_id":              "test-key-id",
		"private_key":                 string(pemKey),
		"client_email":                "vertex-adc-test@test-proj.iam.gserviceaccount.com",
		"client_id":                   "123",
		"token_uri":                   tokenURI,
		"quota_project_id":            "quota-proj",
		"auth_uri":                    "https://accounts.google.com/o/oauth2/auth",
		"auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
	}
	raw, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNativePlugin(t *testing.T) {
	binary, library := os.Getenv("CPA_BINARY"), os.Getenv("CPA_PLUGIN_PATH")
	if binary == "" || library == "" {
		t.Skip("set CPA_BINARY and CPA_PLUGIN_PATH")
	}
	id := os.Getenv("CPA_PLUGIN_ID")
	if id == "" {
		id = "vertex-adc"
	}

	var tokenGrants, rawPredict, genContent atomic.Int64
	var quotaHeaderSeen, modelStripped, anthropicVersionSeen atomic.Bool

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/token":
			tokenGrants.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"access_token":"mock-access-token","token_type":"Bearer","expires_in":3600}`)
			return
		case strings.HasSuffix(path, "/publishers/anthropic/models"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"publisherModels":[{"name":"publishers/anthropic/models/claude-sonnet-4","versionId":"20250514"},{"name":"publishers/anthropic/models/text-embed","versionId":"1"}]}`)
			return
		case strings.HasSuffix(path, "/publishers/google/models"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"publisherModels":[{"name":"publishers/google/models/gemini-2.5-pro","versionId":"001"},{"name":"publishers/google/models/imagen-3","versionId":"1"}]}`)
			return
		}

		if r.Header.Get("X-Goog-User-Project") != "" {
			quotaHeaderSeen.Store(true)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if _, ok := body["model"]; !ok {
			modelStripped.Store(true)
		}

		switch {
		case strings.Contains(path, "/publishers/anthropic/models/") && strings.HasSuffix(path, ":rawPredict"):
			rawPredict.Add(1)
			if body["anthropic_version"] == "vertex-2023-10-16" {
				anthropicVersionSeen.Store(true)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		case strings.Contains(path, "/publishers/anthropic/models/") && strings.HasSuffix(path, ":streamRawPredict"):
			rawPredict.Add(1)
			if body["anthropic_version"] == "vertex-2023-10-16" {
				anthropicVersionSeen.Store(true)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			stream(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		case strings.Contains(path, "/publishers/google/models/") && strings.HasSuffix(path, ":generateContent"):
			genContent.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
		case strings.Contains(path, "/publishers/google/models/") && strings.HasSuffix(path, ":streamGenerateContent"):
			genContent.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			stream(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"OK\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n")
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()

	temp := t.TempDir()
	authdir := filepath.Join(temp, "auth")
	plugins := filepath.Join(temp, "plugins", runtime.GOOS, runtime.GOARCH)
	for _, dir := range []string{authdir, plugins} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, b []byte) {
		t.Helper()
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	credsPath := filepath.Join(temp, "adc.json")
	write(credsPath, syntheticServiceAccount(t, upstream.URL+"/token"))

	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	b, err := os.ReadFile(library)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(plugins, id+ext), b)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	cfg := map[string]any{
		"host": "127.0.0.1", "port": port, "auth-dir": authdir,
		"api-keys":          []string{"client-test-key"},
		"remote-management": map[string]any{"secret-key": "management-test-key", "disable-control-panel": true, "disable-auto-update-panel": true},
		"request-retry":     0, "max-retry-interval": 0, "commercial-mode": true, "disable-cooling": true,
		"plugins": map[string]any{"enabled": true, "dir": filepath.Join(temp, "plugins"), "configs": map[string]any{
			id: map[string]any{
				"enabled":                 true,
				"project_id":              "test-proj",
				"location":                "global",
				"api_endpoint":            upstream.URL,
				"allow_insecure_base_url": true,
				"credentials":             "file:" + credsPath,
				"model_cache_ttl_seconds": 30,
			},
		}},
	}
	raw, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(temp, "config.json")
	write(cfgPath, raw)

	logPath := filepath.Join(temp, "host.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config", cfgPath, "--local-model")
	cmd.Dir = temp
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + temp, "TMPDIR=" + os.TempDir()}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		_ = log.Close()
		if t.Failed() {
			b, _ := os.ReadFile(logPath)
			t.Logf("host log:\n%s", b)
		}
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 15 * time.Second}
	request := func(method, path, key string, body any) (int, []byte) {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		r, _ := http.NewRequest(method, base+path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ = io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	await := func(t *testing.T, f func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !f() {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for host state")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	await(t, func() bool {
		_, b := request("GET", "/v1/models", "client-test-key", nil)
		return bytes.Contains(b, []byte("vertex/anthropic/claude-sonnet-4")) && bytes.Contains(b, []byte("vertex/google/gemini-2.5-pro"))
	})

	t.Run("discovery filters non-chat families", func(t *testing.T) {
		_, b := request("GET", "/v1/models", "client-test-key", nil)
		if bytes.Contains(b, []byte("text-embed")) || bytes.Contains(b, []byte("imagen-3")) {
			t.Fatalf("non-chat model exposed: %s", b)
		}
	})

	t.Run("claude passthrough via rawPredict", func(t *testing.T) {
		for _, stream := range []bool{false, true} {
			body := map[string]any{"model": "vertex/anthropic/claude-sonnet-4", "stream": stream, "max_tokens": 32, "messages": []any{map[string]string{"role": "user", "content": "hi"}}}
			code, b := request("POST", "/v1/messages", "client-test-key", body)
			if code != 200 || !bytes.Contains(b, []byte("OK")) {
				t.Fatalf("claude stream=%v: %d %s", stream, code, b)
			}
		}
		if !modelStripped.Load() {
			t.Error("model field was not stripped from the Vertex request body")
		}
		if !anthropicVersionSeen.Load() {
			t.Error("anthropic_version was not injected")
		}
		if !quotaHeaderSeen.Load() {
			t.Error("x-goog-user-project header was not sent")
		}
	})

	t.Run("openai to gemini translation via generateContent", func(t *testing.T) {
		for _, stream := range []bool{false, true} {
			body := map[string]any{"model": "vertex/google/gemini-2.5-pro", "stream": stream, "max_tokens": 32, "messages": []any{map[string]string{"role": "user", "content": "hi"}}}
			code, b := request("POST", "/v1/chat/completions", "client-test-key", body)
			if code != 200 || !bytes.Contains(b, []byte("OK")) {
				t.Fatalf("gemini stream=%v: %d %s", stream, code, b)
			}
		}
	})

	t.Run("unknown model rejected", func(t *testing.T) {
		before := rawPredict.Load() + genContent.Load()
		code, _ := request("POST", "/v1/messages", "client-test-key", map[string]any{"model": "vertex/anthropic/does-not-exist", "max_tokens": 8, "messages": []any{map[string]string{"role": "user", "content": "hi"}}})
		if code == 200 || rawPredict.Load()+genContent.Load() != before {
			t.Fatalf("unknown model reached upstream: %d", code)
		}
	})

	t.Run("management protected and secret-free", func(t *testing.T) {
		code, _ := request("GET", "/v0/management/plugins/"+id+"/status", "", nil)
		if code != 401 && code != 403 {
			t.Fatalf("unprotected status: %d", code)
		}
		code, b := request("GET", "/v0/management/plugins/"+id+"/status", "management-test-key", nil)
		if code != 200 || bytes.Contains(b, []byte("mock-access-token")) || bytes.Contains(b, []byte("PRIVATE KEY")) {
			t.Fatalf("unsafe status: %d %s", code, b)
		}
	})

	if tokenGrants.Load() == 0 {
		t.Error("ADC token exchange never happened")
	}
}

func stream(w http.ResponseWriter, body string) {
	flusher, ok := w.(http.Flusher)
	for i := 0; i < len(body); i += 16 {
		end := i + 16
		if end > len(body) {
			end = len(body)
		}
		_, _ = io.WriteString(w, body[i:end])
		if ok {
			flusher.Flush()
		}
	}
}
