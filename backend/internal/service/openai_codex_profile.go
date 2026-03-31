package service

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
)

const (
	openAICodexOriginator    = "codex_cli_rs"
	openAICodexStableVersion = "0.117.0"
)

func getOpenAIGroupFromContext(c *gin.Context) *Group {
	if c == nil {
		return nil
	}
	if value, exists := c.Get("api_key"); exists {
		if apiKey, ok := value.(*APIKey); ok && apiKey != nil && IsGroupContextValid(apiKey.Group) {
			return apiKey.Group
		}
	}
	if c.Request == nil {
		return nil
	}
	return groupFromRequestContext(c.Request.Context())
}

func groupFromRequestContext(ctx context.Context) *Group {
	if ctx == nil {
		return nil
	}
	group, ok := ctx.Value(ctxkey.Group).(*Group)
	if !ok || !IsGroupContextValid(group) {
		return nil
	}
	return group
}

func (s *OpenAIGatewayService) shouldForceOpenAICodexProfile(c *gin.Context) bool {
	if s != nil && s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		return true
	}
	group := getOpenAIGroupFromContext(c)
	return group != nil && group.OpenAIForceCodex
}

func applyForcedOpenAICodexHeaders(headers http.Header) {
	if headers == nil {
		return
	}
	headers.Set("user-agent", codexCLIUserAgent)
	headers.Set("originator", openAICodexOriginator)
}
