package admin

import (
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// AIPilotHandler exposes admin APIs for account autopilot.
type AIPilotHandler struct {
	pilot    *service.AIPilotService
	settings *service.SettingService
}

func NewAIPilotHandler(pilot *service.AIPilotService, settings *service.SettingService) *AIPilotHandler {
	return &AIPilotHandler{pilot: pilot, settings: settings}
}

func (h *AIPilotHandler) Status(c *gin.Context) {
	if h.pilot == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	response.Success(c, h.pilot.Status())
}

func (h *AIPilotHandler) ListRuns(c *gin.Context) {
	if h.pilot == nil || h.pilot.Repo == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "30"))
	before, _ := strconv.ParseInt(c.Query("before"), 10, 64)
	runs, err := h.pilot.Repo.ListRuns(c.Request.Context(), limit, before)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	response.Success(c, runs)
}

func (h *AIPilotHandler) GetRun(c *gin.Context) {
	if h.pilot == nil || h.pilot.Repo == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	run, err := h.pilot.Repo.GetRun(c.Request.Context(), id)
	if err != nil {
		response.Error(c, http.StatusNotFound, err.Error())
		return
	}
	response.Success(c, run)
}

func (h *AIPilotHandler) Analyze(c *gin.Context) {
	if h.pilot == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	run, err := h.pilot.Analyze(c.Request.Context(), "manual")
	if err != nil {
		response.Error(c, http.StatusConflict, err.Error())
		return
	}
	response.Success(c, run)
}

func (h *AIPilotHandler) RollbackAction(c *gin.Context) {
	if h.pilot == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.pilot.RollbackAction(c.Request.Context(), id); err != nil {
		response.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	response.Success(c, gin.H{"ok": true})
}

func (h *AIPilotHandler) ApproveAction(c *gin.Context) {
	if h.pilot == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.pilot.ApproveAction(c.Request.Context(), id); err != nil {
		response.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	response.Success(c, gin.H{"ok": true})
}

func (h *AIPilotHandler) DismissAction(c *gin.Context) {
	if h.pilot == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.pilot.DismissAction(c.Request.Context(), id); err != nil {
		response.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	response.Success(c, gin.H{"ok": true})
}

func (h *AIPilotHandler) ListSuggestions(c *gin.Context) {
	if h.pilot == nil || h.pilot.Repo == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	items, err := h.pilot.Repo.ListSuggestions(c.Request.Context(), limit)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	response.Success(c, items)
}

func (h *AIPilotHandler) History(c *gin.Context) {
	if h.pilot == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	runs, _ := strconv.Atoi(c.DefaultQuery("runs", "100"))
	hist, err := h.pilot.BuildHistory(c.Request.Context(), runs)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	response.Success(c, hist)
}

func (h *AIPilotHandler) ListScores(c *gin.Context) {
	if h.pilot == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	items, err := h.pilot.ListScoreRows(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	response.Success(c, gin.H{"items": items})
}

func (h *AIPilotHandler) ScoreHistory(c *gin.Context) {
	if h.pilot == nil || h.pilot.Repo == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	items, err := h.pilot.Repo.AccountScoreHistory(c.Request.Context(), id, limit)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	response.Success(c, gin.H{"items": items})
}

func (h *AIPilotHandler) ApproveRunSuggestions(c *gin.Context) {
	if h.pilot == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	response.Success(c, h.pilot.ApproveRunSuggestions(c.Request.Context(), id))
}

func (h *AIPilotHandler) DismissRunSuggestions(c *gin.Context) {
	if h.pilot == nil {
		response.Error(c, http.StatusServiceUnavailable, "autopilot unavailable")
		return
	}
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	response.Success(c, h.pilot.DismissRunSuggestions(c.Request.Context(), id))
}

func (h *AIPilotHandler) GetSettings(c *gin.Context) {
	if h.settings == nil {
		response.Error(c, http.StatusServiceUnavailable, "settings unavailable")
		return
	}
	cfg := h.settings.GetAIAutopilotSettings(c.Request.Context())
	response.Success(c, service.MaskAIAutopilotSettings(cfg))
}

func (h *AIPilotHandler) UpdateSettings(c *gin.Context) {
	if h.settings == nil {
		response.Error(c, http.StatusServiceUnavailable, "settings unavailable")
		return
	}
	var cfg service.AIAutopilotSettings
	if err := c.ShouldBindJSON(&cfg); err != nil {
		response.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.settings.SetAIAutopilotSettings(c.Request.Context(), cfg); err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.pilot != nil {
		h.pilot.Kick()
	}
	response.Success(c, service.MaskAIAutopilotSettings(h.settings.GetAIAutopilotSettings(c.Request.Context())))
}
