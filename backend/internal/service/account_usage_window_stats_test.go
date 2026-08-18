package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

func TestWindowStatsFromAccountStats_CacheHitRate(t *testing.T) {
	t.Parallel()
	if got := windowStatsFromAccountStats(nil); got == nil || got.CacheHitRate != nil {
		t.Fatalf("nil stats should be empty: %+v", got)
	}

	src := &usagestats.AccountStats{
		Requests:            3,
		Tokens:              200,
		InputTokens:         70,
		CacheCreationTokens: 10,
		CacheReadTokens:     20,
	}
	src.FillCacheHitRate()
	got := windowStatsFromAccountStats(src)
	if got.CacheReadTokens != 20 {
		t.Fatalf("cache_read=%d", got.CacheReadTokens)
	}
	if got.CacheHitRate == nil {
		t.Fatal("expected cache hit rate")
	}
	// 20 / (70+10+20) = 20%
	if *got.CacheHitRate < 19.999 || *got.CacheHitRate > 20.001 {
		t.Fatalf("rate=%v", *got.CacheHitRate)
	}
}
