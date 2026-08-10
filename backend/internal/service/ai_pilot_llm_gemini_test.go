package service

import (
	"strings"
	"testing"
)

func TestIsGeminiPilotModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"gemini-3.6-flash", true},
		{"models/gemini-2.5-flash", true},
		{"Gemini-3-Flash", true},
		{"gpt-5.5", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isGeminiPilotModel(tc.in); got != tc.want {
			t.Fatalf("isGeminiPilotModel(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestGeminiGenerateContentURL(t *testing.T) {
	t.Parallel()
	got := geminiGenerateContentURL("http://172.22.0.2:23001", "gemini-3.6-flash", false)
	want := "http://172.22.0.2:23001/v1beta/models/gemini-3.6-flash:generateContent"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	got = geminiGenerateContentURL("http://x/v1/", "models/gemini-3.6-flash", true)
	want = "http://x/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse"
	if got != want {
		t.Fatalf("stream got %q want %q", got, want)
	}
}

func TestMessagesToGeminiBody_SystemAndUser(t *testing.T) {
	t.Parallel()
	body := messagesToGeminiBody([]map[string]string{
		{"role": "system", "content": "you are pilot"},
		{"role": "user", "content": `{"accounts":[]}`},
	}, 4096, 0.2)
	if body["systemInstruction"] == nil {
		t.Fatal("expected systemInstruction")
	}
	contents, _ := body["contents"].([]map[string]any)
	if len(contents) != 1 {
		t.Fatalf("contents=%v", contents)
	}
	gc, _ := body["generationConfig"].(map[string]any)
	tc, _ := gc["thinkingConfig"].(map[string]any)
	if tc["thinkingBudget"] != 0 {
		t.Fatalf("thinkingBudget=%v", tc["thinkingBudget"])
	}
}

func TestParseGeminiGenerateContentBody(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
	  "candidates":[{"content":{"role":"model","parts":[{"text":"{\"summary\":\"ok\"}"}]},"finishReason":"STOP"}],
	  "usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}
	}`)
	c, in, out, err := parseGeminiGenerateContentBody(raw)
	if err != nil || !strings.Contains(c, `"summary":"ok"`) || in != 10 || out != 5 {
		t.Fatalf("c=%q in=%d out=%d err=%v", c, in, out, err)
	}
}
