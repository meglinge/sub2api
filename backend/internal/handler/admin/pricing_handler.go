package admin

import (
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// PricingHandler 价格管理处理器
type PricingHandler struct {
	billingService *service.BillingService
}

// NewPricingHandler 创建价格管理处理器
func NewPricingHandler(billingService *service.BillingService) *PricingHandler {
	return &PricingHandler{
		billingService: billingService,
	}
}

// ModelPricingItem 模型价格条目（用于列表展示）
type ModelPricingItem struct {
	Model                       string  `json:"model"`
	InputCostPerToken           float64 `json:"input_cost_per_token"`
	OutputCostPerToken          float64 `json:"output_cost_per_token"`
	InputCostPerMTok            float64 `json:"input_cost_per_mtok"`
	OutputCostPerMTok           float64 `json:"output_cost_per_mtok"`
	CacheCreationInputTokenCost float64 `json:"cache_creation_input_token_cost,omitempty"`
	CacheReadInputTokenCost     float64 `json:"cache_read_input_token_cost,omitempty"`
	Provider                    string  `json:"provider"`
	Mode                        string  `json:"mode"`
	SupportsPromptCaching       bool    `json:"supports_prompt_caching"`
	OutputCostPerImage          float64 `json:"output_cost_per_image,omitempty"`
}

// ListPricing 获取所有模型价格列表
// GET /api/v1/admin/pricing
func (h *PricingHandler) ListPricing(c *gin.Context) {
	search := strings.ToLower(strings.TrimSpace(c.Query("search")))
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))

	allPricing := h.billingService.GetAllPricing()

	items := make([]ModelPricingItem, 0, len(allPricing))
	providers := make(map[string]bool)

	for model, pricing := range allPricing {
		providers[pricing.Provider] = true

		if search != "" && !strings.Contains(strings.ToLower(model), search) &&
			!strings.Contains(strings.ToLower(pricing.Provider), search) {
			continue
		}
		if provider != "" && strings.ToLower(pricing.Provider) != provider {
			continue
		}

		items = append(items, ModelPricingItem{
			Model:                       model,
			InputCostPerToken:           pricing.InputCostPerToken,
			OutputCostPerToken:          pricing.OutputCostPerToken,
			InputCostPerMTok:            pricing.InputCostPerToken * 1_000_000,
			OutputCostPerMTok:           pricing.OutputCostPerToken * 1_000_000,
			CacheCreationInputTokenCost: pricing.CacheCreationInputTokenCost,
			CacheReadInputTokenCost:     pricing.CacheReadInputTokenCost,
			Provider:                    pricing.Provider,
			Mode:                        pricing.Mode,
			SupportsPromptCaching:       pricing.SupportsPromptCaching,
			OutputCostPerImage:          pricing.OutputCostPerImage,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Provider != items[j].Provider {
			return items[i].Provider < items[j].Provider
		}
		return items[i].Model < items[j].Model
	})

	providerList := make([]string, 0, len(providers))
	for p := range providers {
		if p != "" {
			providerList = append(providerList, p)
		}
	}
	sort.Strings(providerList)

	response.Success(c, gin.H{
		"items":     items,
		"total":     len(items),
		"providers": providerList,
	})
}

// GetStatus 获取价格服务状态
// GET /api/v1/admin/pricing/status
func (h *PricingHandler) GetStatus(c *gin.Context) {
	status := h.billingService.GetPricingServiceStatus()
	config := h.billingService.GetPricingConfig()

	response.Success(c, gin.H{
		"status": status,
		"config": config,
	})
}

// ForceUpdate 强制更新价格数据
// POST /api/v1/admin/pricing/update
func (h *PricingHandler) ForceUpdate(c *gin.Context) {
	if err := h.billingService.ForceUpdatePricing(); err != nil {
		response.Error(c, http.StatusInternalServerError, "Failed to update pricing: "+err.Error())
		return
	}

	status := h.billingService.GetPricingServiceStatus()
	response.Success(c, gin.H{
		"message": "Pricing data updated successfully",
		"status":  status,
	})
}

// LookupModel 查询单个模型价格
// GET /api/v1/admin/pricing/lookup?model=xxx
func (h *PricingHandler) LookupModel(c *gin.Context) {
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		response.Error(c, http.StatusBadRequest, "model parameter is required")
		return
	}

	pricing, err := h.billingService.GetModelPricing(model)
	if err != nil {
		response.Error(c, http.StatusNotFound, "Model pricing not found: "+err.Error())
		return
	}

	response.Success(c, gin.H{
		"model": model,
		"pricing": gin.H{
			"input_cost_per_token":            pricing.InputPricePerToken,
			"output_cost_per_token":           pricing.OutputPricePerToken,
			"input_cost_per_mtok":             pricing.InputPricePerToken * 1_000_000,
			"output_cost_per_mtok":            pricing.OutputPricePerToken * 1_000_000,
			"cache_creation_input_token_cost": pricing.CacheCreationPricePerToken,
			"cache_read_input_token_cost":     pricing.CacheReadPricePerToken,
		},
	})
}

// UploadPricing 手动上传价格JSON文件
// POST /api/v1/admin/pricing/upload
func (h *PricingHandler) UploadPricing(c *gin.Context) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		response.Error(c, http.StatusBadRequest, "No file uploaded")
		return
	}
	defer func() { _ = file.Close() }()

	// 限制文件大小 50MB
	if header.Size > 50*1024*1024 {
		response.Error(c, http.StatusBadRequest, "File too large (max 50MB)")
		return
	}

	// 检查文件扩展名
	if !strings.HasSuffix(strings.ToLower(header.Filename), ".json") {
		response.Error(c, http.StatusBadRequest, "Only JSON files are accepted")
		return
	}

	body, err := io.ReadAll(file)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "Failed to read file: "+err.Error())
		return
	}

	count, err := h.billingService.ImportPricingData(body)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "Failed to import pricing data: "+err.Error())
		return
	}

	status := h.billingService.GetPricingServiceStatus()
	response.Success(c, gin.H{
		"message":     "Pricing data imported successfully",
		"model_count": count,
		"status":      status,
	})
}
