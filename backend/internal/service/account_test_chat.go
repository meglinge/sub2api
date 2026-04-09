package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ChatGPT conversation API constants
const (
	chatgptConversationAPIURL = "https://chatgpt.com/backend-api/f/conversation"
	sentinelReqURL            = "https://sentinel.openai.com/backend-api/sentinel/req"
	chatRequirementsURL       = "https://chatgpt.com/backend-api/sentinel/chat-requirements"
	chatTestUserAgent         = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
)

// ---------- FNV-1a 32-bit (matches sentinel SDK) ----------

func fnv1a32(text string) string {
	h := uint32(2166136261)
	for i := 0; i < len(text); i++ {
		h ^= uint32(text[i])
		h = uint32(uint64(h) * 16777619 & 0xFFFFFFFF)
	}
	h ^= h >> 16
	h = uint32(uint64(h) * 2246822507 & 0xFFFFFFFF)
	h ^= h >> 13
	h = uint32(uint64(h) * 3266489909 & 0xFFFFFFFF)
	h ^= h >> 16
	return fmt.Sprintf("%08x", h)
}

// ---------- Sentinel config / token generation ----------

func sentinelBase64Encode(data any) string {
	jsonBytes, _ := json.Marshal(data)
	return base64.StdEncoding.EncodeToString(jsonBytes)
}

func sentinelGetConfig(sid string) []any {
	perfNow := 1000 + rand.Float64()*49000
	timeOrigin := float64(time.Now().UnixMilli()) - perfNow
	now := time.Now().UTC()
	dateStr := now.Format("Mon, 02 Jan 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)"

	navProps := []string{"vendorSub", "productSub", "vendor", "maxTouchPoints", "hardwareConcurrency", "cookieEnabled"}
	docKeys := []string{"location", "implementation", "URL", "documentURI", "compatMode"}
	winKeys := []string{"Object", "Function", "Array", "Number", "parseFloat", "undefined"}

	rc := func(arr []string) string { return arr[rand.Intn(len(arr))] }
	rcInt := func(arr []int) int { return arr[rand.Intn(len(arr))] }

	return []any{
		"1920x1080", dateStr, float64(4294705152), rand.Float64(), chatTestUserAgent,
		"https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js",
		nil, nil, "en-US", "en-US,en", rand.Float64(),
		rc(navProps) + "\u2212" + "undefined", rc(docKeys), rc(winKeys),
		perfNow, sid, "", rcInt([]int{4, 8, 12, 16, 32}), timeOrigin,
	}
}

func generateRequirementsToken(sid string) string {
	config := sentinelGetConfig(sid)
	config[3] = float64(1)
	config[9] = math.Round(5 + rand.Float64()*45)
	return "gAAAAAC" + sentinelBase64Encode(config)
}

func generateProofToken(sid, seed, difficulty string) string {
	config := sentinelGetConfig(sid)
	startTime := time.Now()
	for i := 0; i < 500000; i++ {
		config[3] = float64(i)
		config[9] = float64(time.Since(startTime).Milliseconds())
		data := sentinelBase64Encode(config)
		hash := fnv1a32(seed + data)
		if hash[:len(difficulty)] <= difficulty {
			return "gAAAAAB" + data + "~S"
		}
	}
	return "gAAAAABwQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + sentinelBase64Encode("null")
}

// ---------- Sentinel challenge flow ----------

type sentinelChallenge struct {
	Token       string `json:"token"`
	ProofOfWork struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
		Difficulty string `json:"difficulty"`
	} `json:"proofofwork"`
}

func (s *AccountTestService) fetchSentinelChallenge(ctx context.Context, deviceID, proxyURL string, accountID int64, concurrency int) (*sentinelChallenge, error) {
	sid := uuid.New().String()
	pToken := generateRequirementsToken(sid)

	body, _ := json.Marshal(map[string]string{
		"p":    pToken,
		"id":   deviceID,
		"flow": "chat_conversation",
	})

	req, err := http.NewRequestWithContext(ctx, "POST", sentinelReqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Referer", "https://sentinel.openai.com/backend-api/sentinel/frame.html")
	req.Header.Set("User-Agent", chatTestUserAgent)
	req.Header.Set("Origin", "https://sentinel.openai.com")

	resp, err := s.httpUpstream.Do(req, proxyURL, accountID, concurrency)
	if err != nil {
		return nil, fmt.Errorf("sentinel request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sentinel returned %d: %s", resp.StatusCode, string(respBody))
	}

	var challenge sentinelChallenge
	if err := json.NewDecoder(resp.Body).Decode(&challenge); err != nil {
		return nil, fmt.Errorf("sentinel decode error: %w", err)
	}
	return &challenge, nil
}

func (s *AccountTestService) buildSentinelToken(ctx context.Context, deviceID, proxyURL string, accountID int64, concurrency int) (string, error) {
	challenge, err := s.fetchSentinelChallenge(ctx, deviceID, proxyURL, accountID, concurrency)
	if err != nil {
		return "", err
	}

	sid := uuid.New().String()
	var pValue string
	if challenge.ProofOfWork.Required && challenge.ProofOfWork.Seed != "" {
		diff := challenge.ProofOfWork.Difficulty
		if diff == "" {
			diff = "0"
		}
		pValue = generateProofToken(sid, challenge.ProofOfWork.Seed, diff)
	} else {
		pValue = generateRequirementsToken(sid)
	}

	tokenJSON, _ := json.Marshal(map[string]string{
		"p":    pValue,
		"t":    "",
		"c":    challenge.Token,
		"id":   deviceID,
		"flow": "chat_conversation",
	})
	return string(tokenJSON), nil
}

type chatRequirementsResponse struct {
	Token       string `json:"token"`
	ProofOfWork struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
		Difficulty string `json:"difficulty"`
	} `json:"proofofwork"`
}

func (s *AccountTestService) getChatRequirements(ctx context.Context, accessToken, deviceID, sentinelToken, proxyURL string, accountID int64, concurrency int) (*chatRequirementsResponse, error) {
	body, _ := json.Marshal(map[string]string{"p": sentinelToken})

	req, err := http.NewRequestWithContext(ctx, "POST", chatRequirementsURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OAI-Device-Id", deviceID)
	req.Header.Set("OAI-Language", "en-US")
	req.Header.Set("User-Agent", chatTestUserAgent)
	req.Header.Set("OpenAI-Sentinel-Chat-Requirements-Token", sentinelToken)

	resp, err := s.httpUpstream.Do(req, proxyURL, accountID, concurrency)
	if err != nil {
		return nil, fmt.Errorf("chat-requirements request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("chat-requirements returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result chatRequirementsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("chat-requirements decode error: %w", err)
	}
	return &result, nil
}

// ---------- Chat conversation test ----------

func createChatConversationPayload(model string) map[string]any {
	return map[string]any{
		"action": "next",
		"messages": []map[string]any{
			{
				"id":          uuid.New().String(),
				"author":      map[string]string{"role": "user"},
				"create_time": float64(time.Now().Unix()),
				"content": map[string]any{
					"content_type": "text",
					"parts":        []string{"Hello! What is 3 + 7? Reply in one short sentence."},
				},
				"metadata": map[string]any{},
			},
		},
		"parent_message_id":        "client-created-root",
		"model":                    model,
		"timezone_offset_min":      -480,
		"timezone":                 "Asia/Shanghai",
		"conversation_mode":        map[string]string{"kind": "primary_assistant"},
		"enable_message_followups": true,
		"system_hints":             []string{},
		"supports_buffering":       true,
		"supported_encodings":      []string{"v1"},
	}
}

// TestChatAccountConnection tests an OpenAI OAuth account via ChatGPT conversation API
func (s *AccountTestService) TestChatAccountConnection(c *gin.Context, accountID int64, modelID string) error {
	ctx := c.Request.Context()

	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return s.sendErrorAndEnd(c, "Account not found")
	}

	if !account.IsOpenAI() {
		return s.sendErrorAndEnd(c, "Chat test only supports OpenAI accounts")
	}

	if !account.IsOAuth() {
		return s.sendErrorAndEnd(c, "Chat test only supports OAuth accounts (not API key)")
	}

	return s.testOpenAIChatConnection(c, account, modelID)
}

func (s *AccountTestService) testOpenAIChatConnection(c *gin.Context, account *Account, modelID string) error {
	ctx := c.Request.Context()

	accessToken := account.GetOpenAIAccessToken()
	if accessToken == "" {
		return s.sendErrorAndEnd(c, "No access token available")
	}

	testModel := modelID
	if testModel == "" {
		testModel = "auto"
	}

	// Get proxy URL
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	deviceID := uuid.New().String()
	sessionID := uuid.New().String()

	// Set SSE headers
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	// Send test_start event
	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModel})

	// Step 1: Build sentinel token
	s.sendEvent(c, TestEvent{Type: "content", Text: "[sentinel] fetching challenge...\n"})
	sentinelToken, err := s.buildSentinelToken(ctx, deviceID, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Sentinel token failed: %s", err.Error()))
	}
	s.sendEvent(c, TestEvent{Type: "content", Text: "[sentinel] challenge OK\n"})

	// Step 2: Get chat-requirements
	s.sendEvent(c, TestEvent{Type: "content", Text: "[chat-requirements] fetching...\n"})
	chatReqs, err := s.getChatRequirements(ctx, accessToken, deviceID, sentinelToken, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Chat requirements failed: %s", err.Error()))
	}
	chatReqToken := chatReqs.Token
	if chatReqToken == "" {
		chatReqToken = sentinelToken
	}
	s.sendEvent(c, TestEvent{Type: "content", Text: "[chat-requirements] OK\n"})

	// Step 3: Compute conversation proof token if needed
	var proofToken string
	if chatReqs.ProofOfWork.Required && chatReqs.ProofOfWork.Seed != "" {
		s.sendEvent(c, TestEvent{Type: "content", Text: "[pow] computing proof-of-work...\n"})
		diff := chatReqs.ProofOfWork.Difficulty
		if diff == "" {
			diff = "0"
		}
		sid := uuid.New().String()
		proofToken = generateProofToken(sid, chatReqs.ProofOfWork.Seed, diff)
		s.sendEvent(c, TestEvent{Type: "content", Text: "[pow] computed\n"})
	}

	// Step 4: Send conversation request
	s.sendEvent(c, TestEvent{Type: "content", Text: "[conversation] sending request...\n"})
	payload := createChatConversationPayload(testModel)
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", chatgptConversationAPIURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create request")
	}
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OAI-Device-Id", deviceID)
	req.Header.Set("OAI-Session-Id", sessionID)
	req.Header.Set("OAI-Language", "en-US")
	req.Header.Set("User-Agent", chatTestUserAgent)
	req.Header.Set("OpenAI-Sentinel-Chat-Requirements-Token", chatReqToken)
	if proofToken != "" {
		req.Header.Set("OpenAI-Sentinel-Proof-Token", proofToken)
	}
	req.Header.Set("OpenAI-Sentinel-Turnstile-Token", "")

	if chatgptAccountID := account.GetChatGPTAccountID(); chatgptAccountID != "" {
		req.Header.Set("chatgpt-account-id", chatgptAccountID)
	}

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Conversation request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		errMsg := fmt.Sprintf("API returned %d: %s", resp.StatusCode, string(body))
		if resp.StatusCode == http.StatusForbidden {
			_ = s.accountRepo.SetError(ctx, account.ID, errMsg)
		}
		return s.sendErrorAndEnd(c, errMsg)
	}

	// Process conversation SSE stream
	return s.processChatConversationStream(c, resp.Body)
}

// processChatConversationStream parses the ChatGPT conversation SSE response.
// It handles both full message objects and delta v1 incremental patches.
func (s *AccountTestService) processChatConversationStream(c *gin.Context, body io.Reader) error {
	scanner := newSSEScanner(body)
	var assembled strings.Builder
	var resolvedModel string

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !sseDataPrefix.MatchString(line) {
			continue
		}

		jsonStr := sseDataPrefix.ReplaceAllString(line, "")
		if jsonStr == "[DONE]" {
			break
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			continue
		}

		// Delta v1 append: {"o":"append","v":"text","p":"...parts/0"}
		if op, _ := data["o"].(string); op == "append" {
			if v, _ := data["v"].(string); v != "" {
				if p, _ := data["p"].(string); strings.Contains(p, "parts") {
					_, _ = assembled.WriteString(v)
					s.sendEvent(c, TestEvent{Type: "content", Text: v})
				}
			}
			continue
		}

		// Full message objects: check v.message or message
		msg := extractMessage(data)
		if msg == nil {
			continue
		}

		role, _ := msg["role"].(string)
		if role != "assistant" {
			// Extract model from input_message metadata
			if metadata, ok := msg["metadata"].(map[string]any); ok {
				if m, ok := metadata["resolved_model_slug"].(string); ok && m != "" {
					resolvedModel = m
				}
			}
			continue
		}

		// Extract assistant content
		if content, ok := msg["content"].(map[string]any); ok {
			if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
				if text, ok := parts[0].(string); ok && len(text) > assembled.Len() {
					// Full replacement (non-incremental)
					delta := text[assembled.Len():]
					if delta != "" {
						s.sendEvent(c, TestEvent{Type: "content", Text: delta})
					}
					assembled.Reset()
					_, _ = assembled.WriteString(text)
				}
			}
		}

		// Extract model
		if metadata, ok := msg["metadata"].(map[string]any); ok {
			if m, ok := metadata["resolved_model_slug"].(string); ok && m != "" {
				resolvedModel = m
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Stream read error: %s", err.Error()))
	}

	if resolvedModel != "" {
		s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("\n[model] %s\n", resolvedModel)})
	}

	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// extractMessage extracts the message object from various SSE data formats
func extractMessage(data map[string]any) map[string]any {
	// Try v.message (delta v1 wrapped)
	if v, ok := data["v"].(map[string]any); ok {
		if msg, ok := v["message"].(map[string]any); ok {
			if author, ok := msg["author"].(map[string]any); ok {
				role, _ := author["role"].(string)
				result := map[string]any{"role": role, "content": msg["content"], "metadata": msg["metadata"]}
				if status, ok := msg["status"].(string); ok {
					result["status"] = status
				}
				return result
			}
		}
	}

	// Try message directly
	if msg, ok := data["message"].(map[string]any); ok {
		if author, ok := msg["author"].(map[string]any); ok {
			role, _ := author["role"].(string)
			result := map[string]any{"role": role, "content": msg["content"], "metadata": msg["metadata"]}
			if status, ok := msg["status"].(string); ok {
				result["status"] = status
			}
			return result
		}
	}

	// Try input_message
	if inputMsg, ok := data["input_message"].(map[string]any); ok {
		return map[string]any{"metadata": inputMsg["metadata"]}
	}

	return nil
}

// newSSEScanner returns a line scanner for SSE streams
func newSSEScanner(r io.Reader) *lineScanner {
	return &lineScanner{reader: r, buf: make([]byte, 0, 4096)}
}

type lineScanner struct {
	reader io.Reader
	buf    []byte
	line   string
	err    error
}

func (ls *lineScanner) Scan() bool {
	for {
		// Check for newline in buffer
		if idx := strings.Index(string(ls.buf), "\n"); idx >= 0 {
			ls.line = string(ls.buf[:idx])
			ls.buf = ls.buf[idx+1:]
			return true
		}
		// Read more data
		tmp := make([]byte, 4096)
		n, err := ls.reader.Read(tmp)
		if n > 0 {
			ls.buf = append(ls.buf, tmp[:n]...)
		}
		if err != nil {
			if len(ls.buf) > 0 {
				ls.line = string(ls.buf)
				ls.buf = nil
				return true
			}
			if err != io.EOF {
				ls.err = err
			}
			return false
		}
	}
}

func (ls *lineScanner) Text() string { return ls.line }
func (ls *lineScanner) Err() error   { return ls.err }
