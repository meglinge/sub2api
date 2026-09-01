package service

import "time"

const (
	// AccountPerfWindow is the live lookback for account-table TTFB/TPS (UpstreamRouter channel table analog).
	AccountPerfWindow = 15 * time.Minute
	// AccountPerfSampleCap is per-account max recent successes used for percentiles.
	AccountPerfSampleCap = 500
)

// AccountPerfStats is TTFB/TPS percentiles for one account, matching UpstreamRouter
// channel-table ttfbP50/ttfbP99 and tpsP50/tpsP1.
//
// TPS = output_tokens / max(duration_ms - first_token_ms, duration_ms) seconds
// (generation segment; same formula as UpstreamRouter metrics.RecordFinish).
type AccountPerfStats struct {
	AccountID int64   `json:"account_id"`
	Samples   int     `json:"samples"`
	TtfbP50Ms float64 `json:"ttfb_p50_ms"`
	TtfbP99Ms float64 `json:"ttfb_p99_ms"`
	TpsP50    float64 `json:"tps_p50"`
	TpsP1     float64 `json:"tps_p1"`
}

// GenerationTPS is output tokens per second of the generation segment.
func GenerationTPS(outputTokens, durationMs, ttfbMs int) float64 {
	if outputTokens <= 0 {
		return 0
	}
	genMs := durationMs - ttfbMs
	if genMs > 0 {
		return float64(outputTokens) / (float64(genMs) / 1000.0)
	}
	if durationMs > 0 {
		return float64(outputTokens) / (float64(durationMs) / 1000.0)
	}
	return 0
}
