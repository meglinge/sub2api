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

// isGeminiPilotModel detects Gemini models that must use generateContent (not OpenAI chat).
// CCH routes gemini-* only on /v1beta/models/*:generateContent; /v1/chat/completions returns model_not_supported.
func isGeminiPilotModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "models/")
	return strings.HasPrefix(m, "gemini")
}

func isGrokPilotModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(m, "grok")
}

// geminiGenerateContentURL builds CCH/Google Gemini generateContent endpoint.
// baseURL may be bare host, .../v1, or .../v1beta.
func geminiGenerateContentURL(baseURL, model string, stream bool) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, suf := range []string{"/v1beta", "/v1"} {
		if strings.HasSuffix(base, suf) {
			base = strings.TrimSuffix(base, suf)
			break
		}
	}
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, "models/")
	if stream {
		return base + "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
	}
	return base + "/v1beta/models/" + model + ":generateContent"
}

// messagesToGeminiBody maps OpenAI-style chat messages to Gemini generateContent body.
// system/developer → systemInstruction; assistant → model role.
// thinkingBudget=0 avoids multi-second "thinking" token burn that was emptying short maxOutputTokens.
func messagesToGeminiBody(messages []map[string]string, maxTokens int, temperature float64) map[string]any {
	if maxTokens <= 0 {
		// Cap completions: pilot decisions are structured JSON; 15k–25k outs were common
		// and dominated wall-clock on gpt-5.5 (UR still fast when single-turn + shorter out).
		maxTokens = 2048
	}
	var systemParts []map[string]string
	var contents []map[string]any
	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m["role"]))
		content := m["content"]
		if strings.TrimSpace(content) == "" {
			continue
		}
		switch role {
		case "system", "developer":
			systemParts = append(systemParts, map[string]string{"text": content})
		case "assistant", "model":
			contents = append(contents, map[string]any{
				"role":  "model",
				"parts": []map[string]string{{"text": content}},
			})
		default:
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": []map[string]string{{"text": content}},
			})
		}
	}
	// Gemini requires at least one user turn; fold system into user if needed.
	if len(contents) == 0 && len(systemParts) > 0 {
		var b strings.Builder
		for _, p := range systemParts {
			b.WriteString(p["text"])
			b.WriteString("\n")
		}
		contents = append(contents, map[string]any{
			"role":  "user",
			"parts": []map[string]string{{"text": strings.TrimSpace(b.String())}},
		})
		systemParts = nil
	}
	body := map[string]any{
		"contents": contents,
		"generationConfig": map[string]any{
			"temperature":     temperature,
			"maxOutputTokens": maxTokens,
			"thinkingConfig": map[string]any{
				"thinkingBudget": 0,
			},
		},
	}
	if len(systemParts) > 0 {
		body["systemInstruction"] = map[string]any{"parts": systemParts}
	}
	return body
}

func parseGeminiGenerateContentBody(respBody []byte) (content string, inTok, outTok int, err error) {
	trimmed := bytes.TrimSpace(respBody)
	if len(trimmed) == 0 {
		return "", 0, 0, fmt.Errorf("LLM 响应体为空")
	}
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
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
	var b strings.Builder
	for _, c := range parsed.Candidates {
		for _, part := range c.Content.Parts {
			if s := strings.TrimSpace(part.Text); s != "" {
				// Keep original spacing for JSON decision payloads.
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(part.Text)
			}
		}
	}
	content = b.String()
	inTok = parsed.UsageMetadata.PromptTokenCount
	outTok = parsed.UsageMetadata.CandidatesTokenCount
	if outTok == 0 && parsed.UsageMetadata.TotalTokenCount > inTok {
		outTok = parsed.UsageMetadata.TotalTokenCount - inTok
	}
	if strings.TrimSpace(content) == "" {
		return "", inTok, outTok, fmt.Errorf("LLM Gemini 内容为空")
	}
	return content, inTok, outTok, nil
}

// parseGeminiGenerateContentStream reads Gemini SSE (data: {...} per chunk).
func parseGeminiGenerateContentStream(r io.Reader) (content string, inTok, outTok int, err error) {
	sc := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 2<<20)

	var b strings.Builder
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// Some gateways emit raw JSON lines without data: prefix.
			if !strings.HasPrefix(line, "{") {
				continue
			}
		} else {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if line == "" || line == "[DONE]" {
				continue
			}
		}
		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
				TotalTokenCount      int `json:"totalTokenCount"`
			} `json:"usageMetadata"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &chunk) != nil {
			continue
		}
		if chunk.Error != nil && strings.TrimSpace(chunk.Error.Message) != "" {
			return "", 0, 0, fmt.Errorf("LLM error: %s", chunk.Error.Message)
		}
		if chunk.UsageMetadata != nil {
			inTok = chunk.UsageMetadata.PromptTokenCount
			outTok = chunk.UsageMetadata.CandidatesTokenCount
			if outTok == 0 && chunk.UsageMetadata.TotalTokenCount > inTok {
				outTok = chunk.UsageMetadata.TotalTokenCount - inTok
			}
		}
		for _, c := range chunk.Candidates {
			for _, part := range c.Content.Parts {
				if part.Text != "" {
					b.WriteString(part.Text)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		partial := b.String()
		if strings.TrimSpace(partial) != "" {
			return partial, inTok, outTok, nil
		}
		return "", inTok, outTok, fmt.Errorf("读 LLM stream 失败: %w", err)
	}
	content = b.String()
	if strings.TrimSpace(content) == "" {
		return "", inTok, outTok, fmt.Errorf("LLM stream 响应体为空")
	}
	return content, inTok, outTok, nil
}

// callLLMMessagesOnce calls CCH with stream=true (OpenAI chat SSE) and assembles content.
// Non-stream whole-body waits frequently hit 0-TTFB hangs on this CCH+Codex path;
// stream matches normal user traffic and surfaces first tokens earlier.
// Gemini models use /v1beta generateContent (stream) instead of OpenAI chat.
func (p *AIPilotService) callLLMMessagesOnce(ctx context.Context, cfg AIAutopilotSettings, messages []map[string]string) (content string, inTok, outTok int, err error) {
	// UpstreamRouter floor: TimeoutSeconds < 30 → 120s.
	// Stream can still take minutes for large decisions; allow up to settings value.
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout < 30*time.Second {
		timeout = 120 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var url string
	var raw []byte
	useGemini := isGeminiPilotModel(cfg.Model)
	if useGemini {
		url = geminiGenerateContentURL(cfg.BaseURL, cfg.Model, true)
		body := messagesToGeminiBody(messages, 2048, 0.2)
		raw, _ = json.Marshal(body)
	} else {
		url = chatCompletionsURL(cfg.BaseURL)
		body := map[string]any{
			"model":       cfg.Model,
			"messages":    messages,
			"temperature": 0.2,
			"stream":      true,
			// Compact actions JSON only — backend overwrites scores. Prod grok-4.6
			// was emitting 13–19k tokens (incl. reasoning) and taking 4–6 minutes.
			"max_tokens": 2048,
			// Ask providers that support it to include usage on the final SSE chunk.
			"stream_options": map[string]any{"include_usage": true},
		}
		if isGrokPilotModel(cfg.Model) {
			body["reasoning_effort"] = "low"
			body["max_completion_tokens"] = 2048
		}
		raw, _ = json.Marshal(body)
	}
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
		if useGemini {
			return parseGeminiGenerateContentBody(respBody)
		}
		return parseLLMChatCompletionsBody(respBody)
	}
	if useGemini {
		content, inTok, outTok, err = parseGeminiGenerateContentStream(resp.Body)
	} else {
		content, inTok, outTok, err = parseLLMChatCompletionsStream(resp.Body)
	}
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
