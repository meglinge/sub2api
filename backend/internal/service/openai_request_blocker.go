package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type openAIRequestBlockRule struct {
	name string
	re   *regexp.Regexp
}

var openAIRequestBlockRules = []openAIRequestBlockRule{
	{
		name: "break_limit",
		re:   regexp.MustCompile(`(?is)(jailbreak|破限|越狱|脱限|绕过限制|规避限制|绕过审核|规避审核|绕过监测|规避监测|绕过风控|规避风控|绕过审计|规避审计|绕过封禁|规避封禁|封禁绕过|风控绕过|账号滥用|外挂|恶意脚本)`),
	},
	{
		name: "system_prompt_exfil",
		re:   regexp.MustCompile(`(?is)((reveal|extract|dump|leak|show|display|print|repeat|restate|quote|guess|infer|predict|输出|提取|泄露|复述|推测|猜测|显示|打印).{0,80}(system\s*prompt|developer\s*prompt|hidden\s*(rules|prompt)|internal\s*(rules|policy)|系统提示词|开发者提示词|隐藏规则|内部策略))|((system\s*prompt|developer\s*prompt|hidden\s*(rules|prompt)|internal\s*(rules|policy)|系统提示词|开发者提示词|隐藏规则|内部策略).{0,80}(reveal|extract|dump|leak|show|display|print|repeat|restate|quote|guess|infer|predict|输出|提取|泄露|复述|推测|猜测|显示|打印))`),
	},
	{
		name: "ignore_rules",
		re:   regexp.MustCompile(`(?is)(ignore|bypass|override|disable|forget|绕过|规避|忽略|无视|解除).{0,80}(rule|policy|guardrail|restriction|limit|moderation|filter|detection|risk.?control|audit|instruction|规则|策略|限制|审核|风控|监测|审计|指令)`),
	},
	{
		name: "ctf_excuse",
		re:   regexp.MustCompile(`(?is)(ctf|sandbox|role\s*play|just\s+testing|just\s+for\s+research|for\s+research\s+only|只做测试|只做研究|角色扮演).{0,100}(ignore|bypass|jailbreak|绕过|规避|system\s*prompt|developer\s*prompt|隐藏规则|内部策略|系统提示词|开发者提示词)`),
	},
}

var openAIRequestBlockScanKeys = map[string]bool{
	"instructions": true,
	"prompt":       true,
	"text":         true,
	"content":      true,
	"input":        true,
	"query":        true,
	"question":     true,
}

func shouldBlockOpenAIRequest(account *Account, body []byte) (bool, string) {
	if account == nil || !account.IsCodexHardBlockEnabled() || len(body) == 0 {
		return false, ""
	}
	text := buildOpenAIRequestPolicyScanText(body)
	if strings.TrimSpace(text) == "" {
		return false, ""
	}
	for _, rule := range openAIRequestBlockRules {
		if rule.re.MatchString(text) {
			return true, rule.name
		}
	}
	return false, ""
}

func buildOpenAIRequestPolicyScanText(body []byte) string {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	var fragments []string
	collectOpenAIRequestPolicyScanText(payload, "", &fragments)
	return strings.Join(fragments, "\n")
}

func collectOpenAIRequestPolicyScanText(node any, key string, out *[]string) {
	switch v := node.(type) {
	case map[string]any:
		for childKey, child := range v {
			collectOpenAIRequestPolicyScanText(child, strings.ToLower(strings.TrimSpace(childKey)), out)
		}
	case []any:
		for _, child := range v {
			collectOpenAIRequestPolicyScanText(child, key, out)
		}
	case string:
		if !openAIRequestBlockScanKeys[key] {
			return
		}
		if text := strings.TrimSpace(v); text != "" {
			*out = append(*out, text)
		}
	}
}

func (s *OpenAIGatewayService) writeLocalOpenAIHardBlockResponse(
	c *gin.Context,
	requestModel string,
	stream bool,
	reply string,
) *OpenAIForwardResult {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		reply = defaultCodexHardBlockReply
	}
	model := strings.TrimSpace(requestModel)
	if model == "" {
		model = openai.DefaultTestModel
	}
	if stream {
		writeLocalOpenAIHardBlockSSE(c, model, reply)
	} else {
		writeLocalOpenAIHardBlockJSON(c, model, reply)
	}
	return &OpenAIForwardResult{
		RequestID:     "",
		Usage:         OpenAIUsage{},
		Model:         model,
		UpstreamModel: model,
		Stream:        stream,
		OpenAIWSMode:  false,
		Duration:      0,
	}
}

func writeLocalOpenAIHardBlockJSON(c *gin.Context, model, reply string) {
	response := buildLocalOpenAIHardBlockResponse(model, reply)
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.JSON(http.StatusOK, response)
}

func writeLocalOpenAIHardBlockSSE(c *gin.Context, model, reply string) {
	if c == nil {
		return
	}
	flusher, _ := c.Writer.(http.Flusher)
	finalResponse := buildLocalOpenAIHardBlockResponse(model, reply)
	responseID, _ := finalResponse["id"].(string)
	createdAt, _ := finalResponse["created_at"].(int64)
	if createdAt == 0 {
		if v, ok := finalResponse["created_at"].(float64); ok {
			createdAt = int64(v)
		}
	}
	createdEvent := map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         responseID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "in_progress",
			"model":      model,
			"output":     []any{},
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
				"total_tokens":  0,
			},
		},
	}
	events := []any{
		createdEvent,
		map[string]any{
			"type":          "response.output_text.delta",
			"response_id":   responseID,
			"output_index":  0,
			"content_index": 0,
			"delta":         reply,
		},
		map[string]any{
			"type":          "response.output_text.done",
			"response_id":   responseID,
			"output_index":  0,
			"content_index": 0,
			"text":          reply,
		},
		map[string]any{
			"type":     "response.completed",
			"response": finalResponse,
		},
	}

	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			continue
		}
		_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func buildLocalOpenAIHardBlockResponse(model, reply string) map[string]any {
	respID := "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	msgID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	return map[string]any{
		"id":         respID,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      model,
		"output": []any{
			map[string]any{
				"id":     msgID,
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
				"content": []any{
					map[string]any{
						"type":        "output_text",
						"text":        reply,
						"annotations": []any{},
					},
				},
			},
		},
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
			"total_tokens":  0,
		},
	}
}

func logOpenAIRequestBlocked(ctx context.Context, account *Account, requestModel, reason string) {
	accountID := int64(0)
	accountName := ""
	if account != nil {
		accountID = account.ID
		accountName = strings.TrimSpace(account.Name)
	}
	logger.FromContext(ctx).With(
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.String("account_name", accountName),
		zap.String("request_model", strings.TrimSpace(requestModel)),
		zap.String("block_reason", strings.TrimSpace(reason)),
	).Warn("codex.request_blocked")
}

func maybeHandleOpenAIHardBlockedRequest(
	ctx context.Context,
	s *OpenAIGatewayService,
	c *gin.Context,
	account *Account,
	body []byte,
	requestModel string,
	stream bool,
) (*OpenAIForwardResult, bool, error) {
	blocked, reason := shouldBlockOpenAIRequest(account, body)
	if !blocked {
		return nil, false, nil
	}
	if s == nil {
		return nil, true, fmt.Errorf("openai hard block triggered without gateway service")
	}
	logOpenAIRequestBlocked(ctx, account, requestModel, reason)
	result := s.writeLocalOpenAIHardBlockResponse(c, requestModel, stream, account.GetCodexHardBlockReply())
	return result, true, nil
}
