package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCallLLMMessages_StreamOK(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"stream":true`) {
			t.Errorf("expected stream=true body=%s", raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"summary\\\"\"}}]}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\":\\\"ok\\\"}\"}}]}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	p := &AIPilotService{HTTP: srv.Client()}
	cfg := DefaultAIAutopilotSettings()
	cfg.BaseURL = srv.URL
	cfg.APIKey = "sk-test"
	cfg.TimeoutSeconds = 30
	content, in, out, err := p.callLLMMessages(context.Background(), cfg, []map[string]string{
		{"role": "user", "content": "hi"},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(content, "summary") || !strings.Contains(content, "ok") {
		t.Fatalf("content=%q", content)
	}
	if in != 3 || out != 4 {
		t.Fatalf("tokens in=%d out=%d", in, out)
	}
}

func TestCallLLMMessages_NonStreamJSONFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Gateway ignored stream and returned full JSON.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"summary\":\"ok\"}"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}))
	defer srv.Close()
	p := &AIPilotService{HTTP: srv.Client()}
	cfg := DefaultAIAutopilotSettings()
	cfg.BaseURL = srv.URL
	cfg.APIKey = "sk"
	cfg.TimeoutSeconds = 30
	content, in, out, err := p.callLLMMessages(context.Background(), cfg, []map[string]string{{"role": "user", "content": "x"}})
	if err != nil || !strings.Contains(content, "ok") || in != 1 || out != 2 {
		t.Fatalf("content=%q in=%d out=%d err=%v", content, in, out, err)
	}
}

func TestCallLLMMessages_EmptyStreamFailsNoRetry(t *testing.T) {
	t.Parallel()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	p := &AIPilotService{HTTP: srv.Client()}
	cfg := DefaultAIAutopilotSettings()
	cfg.BaseURL = srv.URL
	cfg.APIKey = "sk"
	cfg.TimeoutSeconds = 30
	_, _, _, err := p.callLLMMessages(context.Background(), cfg, []map[string]string{{"role": "user", "content": "x"}})
	if err == nil || !strings.Contains(err.Error(), "空") {
		t.Fatalf("err=%v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("expected 1 attempt, got %d", n.Load())
	}
}

func TestPilotHTTPClientHasNoOwnTimeout(t *testing.T) {
	t.Parallel()
	p := NewAIPilotService(nil, nil, nil, nil)
	if p.HTTP != nil && p.HTTP.Timeout != 0 {
		t.Fatalf("HTTP client must not set Timeout (got %s); per-request cfg.TimeoutSeconds only", p.HTTP.Timeout)
	}
}

func TestIsRetriableLLMError(t *testing.T) {
	t.Parallel()
	if !isRetriableLLMError(errTrunc()) {
		t.Fatal("truncated retriable")
	}
	if isRetriableLLMError(errHard()) {
		t.Fatal("auth not retriable")
	}
}

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

func errTrunc() error { return simpleErr("LLM 响应截断或不完整 JSON: unexpected end of JSON input") }
func errHard() error  { return simpleErr("LLM HTTP 401: unauthorized") }
