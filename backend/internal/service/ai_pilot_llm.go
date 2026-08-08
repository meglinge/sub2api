package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// parseLLMChatCompletionsBody decodes an OpenAI-style non-stream chat completion body.
func parseLLMChatCompletionsBody(respBody []byte) (content string, inTok, outTok int, err error) {
	trimmed := bytes.TrimSpace(respBody)
	if len(trimmed) == 0 {
		return "", 0, 0, fmt.Errorf("LLM 响应体为空")
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &parsed); err != nil {
		if isTruncatedJSONError(err) {
			return "", 0, 0, fmt.Errorf("LLM 响应截断或不完整 JSON: %w", err)
		}
		return "", 0, 0, fmt.Errorf("LLM 响应解析失败: %s", err.Error())
	}
	if parsed.Error != nil && strings.TrimSpace(parsed.Error.Message) != "" {
		return "", 0, 0, fmt.Errorf("LLM error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", 0, 0, fmt.Errorf("LLM 无 choices")
	}
	content = parsed.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		return "", parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, fmt.Errorf("LLM choices 内容为空")
	}
	return content, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, nil
}

// parseLLMChatCompletionsStream reads OpenAI SSE (data: {...}\\n\\n / data: [DONE]).
// Matches normal client traffic on CCH better than non-stream whole-body waits.
func parseLLMChatCompletionsStream(r io.Reader) (content string, inTok, outTok int, err error) {
	sc := bufio.NewScanner(r)
	// Autopilot JSON can have long single SSE lines.
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 2<<20)

	var b strings.Builder
	sawDone := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				sawDone = true
			}
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content any `json:"content"`
				} `json:"delta"`
				Message struct {
					Content any `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Error != nil && strings.TrimSpace(chunk.Error.Message) != "" {
			return "", 0, 0, fmt.Errorf("LLM error: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			inTok = chunk.Usage.PromptTokens
			outTok = chunk.Usage.CompletionTokens
		}
		for _, ch := range chunk.Choices {
			if s := anyContentToString(ch.Delta.Content); s != "" {
				b.WriteString(s)
			}
			if s := anyContentToString(ch.Message.Content); s != "" {
				b.WriteString(s)
			}
		}
	}
	if err := sc.Err(); err != nil {
		partial := b.String()
		if strings.TrimSpace(partial) != "" {
			// Prefer partial content over hard fail if we already have usable JSON text.
			return partial, inTok, outTok, nil
		}
		return "", inTok, outTok, fmt.Errorf("读 LLM stream 失败: %w", err)
	}
	content = b.String()
	if strings.TrimSpace(content) == "" {
		if sawDone {
			return "", inTok, outTok, fmt.Errorf("LLM stream 完成但 content 为空")
		}
		return "", inTok, outTok, fmt.Errorf("LLM stream 响应体为空")
	}
	return content, inTok, outTok, nil
}

func anyContentToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		// content parts: [{"type":"text","text":"..."}]
		var b strings.Builder
		for _, part := range t {
			m, _ := part.(map[string]any)
			if m == nil {
				continue
			}
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}

func isTruncatedJSONError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "unexpected end of JSON") ||
		strings.Contains(s, "unexpected EOF") ||
		strings.Contains(s, "EOF")
}

func isRetriableLLMError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if isTruncatedJSONError(err) {
		return true
	}
	return strings.Contains(s, "响应体为空") ||
		strings.Contains(s, "HTTP 429") ||
		strings.Contains(s, "HTTP 502") ||
		strings.Contains(s, "HTTP 503") ||
		strings.Contains(s, "HTTP 504")
}

// callLLMMessagesOnce calls CCH with stream=true (OpenAI chat SSE) and assembles content.
// Non-stream whole-body waits frequently hit 0-TTFB hangs on this CCH+Codex path;
// stream matches normal user traffic and surfaces first tokens earlier.
func (p *AIPilotService) callLLMMessagesOnce(ctx context.Context, cfg AIAutopilotSettings, messages []map[string]string) (content string, inTok, outTok int, err error) {
	url := chatCompletionsURL(cfg.BaseURL)
	body := map[string]any{
		"model":       cfg.Model,
		"messages":    messages,
		"temperature": 0.2,
		"stream":      true,
		// Cap runaway completions (prod saw 11k tokens / 3min). Full decision JSON fits.
		"max_tokens": 4096,
		// Ask providers that support it to include usage on the final SSE chunk.
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, _ := json.Marshal(body)
	// UpstreamRouter floor: TimeoutSeconds < 30 → 120s.
	// Stream can still take minutes for large decisions; allow up to settings value.
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout < 30*time.Second {
		timeout = 120 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Sub2API-Client", "ai-autopilot")
	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", 0, 0, fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, truncateStr(string(respBody), 400))
	}
	// Some gateways ignore stream=true and return a full JSON body.
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") && !strings.Contains(ct, "event-stream") {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if readErr != nil {
			return "", 0, 0, fmt.Errorf("读 LLM 响应失败(已读 %d 字节,超时上限 %s): %w",
				len(respBody), timeout, readErr)
		}
		return parseLLMChatCompletionsBody(respBody)
	}
	content, inTok, outTok, err = parseLLMChatCompletionsStream(resp.Body)
	if err != nil {
		// Attach timeout budget for ops.
		if reqCtx.Err() != nil {
			return "", inTok, outTok, fmt.Errorf("%v (超时上限 %s)", err, timeout)
		}
		return "", inTok, outTok, err
	}
	return content, inTok, outTok, nil
}

// callLLMMessages single attempt (no empty-body retry storm).
func (p *AIPilotService) callLLMMessages(ctx context.Context, cfg AIAutopilotSettings, messages []map[string]string) (content string, inTok, outTok int, err error) {
	return p.callLLMMessagesOnce(ctx, cfg, messages)
}
