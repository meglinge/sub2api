package admin

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type CodexTurnStateHandler struct {
	gateway *service.OpenAIGatewayService
	settings *service.SettingService
	accounts service.AdminService
}

func NewCodexTurnStateHandler(
	gateway *service.OpenAIGatewayService,
	settings *service.SettingService,
	accounts service.AdminService,
) *CodexTurnStateHandler {
	return &CodexTurnStateHandler{gateway: gateway, settings: settings, accounts: accounts}
}

func (h *CodexTurnStateHandler) GetConfig(c *gin.Context) {
	if h == nil || h.settings == nil {
		response.Error(c, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	cfg, err := h.settings.GetCodexTurnStateCacheConfig(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	response.Success(c, cfg)
}

func (h *CodexTurnStateHandler) UpdateConfig(c *gin.Context) {
	if h == nil || h.settings == nil {
		response.Error(c, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	var req service.CodexTurnStateCacheConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request body")
		return
	}
	cfg, err := h.settings.SetCodexTurnStateCacheConfig(c.Request.Context(), req)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if h.gateway != nil {
		h.gateway.SetCodexTurnStateCacheConfig(cfg)
	}
	response.Success(c, cfg)
}

func (h *CodexTurnStateHandler) Overview(c *gin.Context) {
	if h == nil || h.gateway == nil {
		response.Error(c, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	overview, err := h.gateway.CodexTurnStateOverview(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	response.Success(c, overview)
}

func (h *CodexTurnStateHandler) Events(c *gin.Context) {
	if h == nil || h.gateway == nil {
		response.Error(c, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	response.Success(c, gin.H{"events": h.gateway.CodexTurnStateRefreshEvents(200)})
}

type codexTurnStateCellRequest struct {
	AccountID int64  `json:"account_id"`
	Model     string `json:"model"`
}

func (h *CodexTurnStateHandler) Refresh(c *gin.Context) {
	account, model, ok := h.bindCell(c)
	if !ok {
		return
	}
	err := h.gateway.ForceRefreshCodexTurnState(c.Request.Context(), account, model)
	if err != nil {
		response.Success(c, gin.H{"ok": false, "error": err.Error()})
		return
	}
	response.Success(c, gin.H{
		"ok":     true,
		"health": service.InspectCodexTurnStateHealth(account.GetCodexTurnState(model), account.GetCredential("plan_type")),
	})
}

func (h *CodexTurnStateHandler) ClearCooldown(c *gin.Context) {
	account, model, ok := h.bindCell(c)
	if !ok {
		return
	}
	cleared := h.gateway.ClearCodexTurnStateCooldown(account, model)
	response.Success(c, gin.H{"cleared": cleared})
}

func (h *CodexTurnStateHandler) Invalidate(c *gin.Context) {
	account, model, ok := h.bindCell(c)
	if !ok {
		return
	}
	h.gateway.InvalidateCodexTurnState(c.Request.Context(), account, model)
	response.Success(c, gin.H{"invalidated": true})
}

func (h *CodexTurnStateHandler) bindCell(c *gin.Context) (*service.Account, string, bool) {
	if h == nil || h.gateway == nil || h.accounts == nil {
		response.Error(c, http.StatusServiceUnavailable, "service unavailable")
		return nil, "", false
	}
	var req codexTurnStateCellRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request body")
		return nil, "", false
	}
	model := strings.TrimSpace(req.Model)
	if req.AccountID <= 0 || model == "" {
		response.BadRequest(c, "缺少 account_id 或 model")
		return nil, "", false
	}
	account, err := h.accounts.GetAccount(c.Request.Context(), req.AccountID)
	if err != nil || account == nil {
		response.Error(c, http.StatusNotFound, "账号不存在")
		return nil, "", false
	}
	if !account.UsesOpenAICodexProtocol() {
		response.BadRequest(c, "仅 ChatGPT Codex 协议账号支持 X-Codex-Turn-State")
		return nil, "", false
	}
	return account, model, true
}
