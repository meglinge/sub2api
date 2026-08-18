package usagestats

// AccountStats 账号使用统计
//
// cost: 账号口径费用（使用 total_cost * account_rate_multiplier）
// standard_cost: 标准费用（使用 total_cost，不含倍率）
// user_cost: 用户/API Key 口径费用（使用 actual_cost，受分组倍率影响）
type AccountStats struct {
	Requests            int64    `json:"requests"`
	Tokens              int64    `json:"tokens"`
	Cost                float64  `json:"cost"`
	StandardCost        float64  `json:"standard_cost"`
	UserCost            float64  `json:"user_cost"`
	InputTokens         int64    `json:"input_tokens,omitempty"`
	CacheCreationTokens int64    `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int64    `json:"cache_read_tokens,omitempty"`
	CacheHitRate        *float64 `json:"cache_hit_rate,omitempty"` // 0-100, nil when no prompt tokens
}

// ComputeCacheHitRate is cache_read / (input + cache_creation + cache_read) * 100.
// Output tokens are excluded: they are not part of the prompt cache.
func ComputeCacheHitRate(inputTokens, cacheCreationTokens, cacheReadTokens int64) *float64 {
	den := inputTokens + cacheCreationTokens + cacheReadTokens
	if den <= 0 {
		return nil
	}
	rate := float64(cacheReadTokens) * 100 / float64(den)
	return &rate
}

// FillCacheHitRate derives CacheHitRate from the token sums.
func (s *AccountStats) FillCacheHitRate() {
	if s == nil {
		return
	}
	s.CacheHitRate = ComputeCacheHitRate(s.InputTokens, s.CacheCreationTokens, s.CacheReadTokens)
}
