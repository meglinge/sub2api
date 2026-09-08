package service

import (
	"strconv"
	"strings"
	"testing"
)

func TestCacheSQLThresholdsMatchUsageQuery(t *testing.T) {
	t.Parallel()
	if cacheEligibleMinTokens != 8192 || cacheBigMissMinInput != 20000 || cacheBigMissMaxRead != 8192 {
		t.Fatalf("SQL thresholds drifted: eligible=%d bigMissInput=%d bigMissRead=%d",
			cacheEligibleMinTokens, cacheBigMissMinInput, cacheBigMissMaxRead)
	}
}

func TestCacheQualityOf_InsufficientSamples(t *testing.T) {
	t.Parallel()
	if _, ok := cacheQualityOf(AccountTrafficStats{CacheEligibleRequests: 3, CacheEligibleTokens: 100000, CacheReadTokens: 90000}); ok {
		t.Fatal("want ok=false when eligible requests below floor")
	}
	if _, ok := cacheQualityOf(AccountTrafficStats{}); ok {
		t.Fatal("empty stats should not be eligible")
	}
}

func TestCacheQualityOf_HighHit(t *testing.T) {
	t.Parallel()
	q, ok := cacheQualityOf(AccountTrafficStats{
		CacheEligibleRequests: 40,
		CacheEligibleTokens:   5_000_000,
		CacheReadTokens:       4_800_000,
		CacheBigMissRequests:  0,
	})
	if !ok {
		t.Fatal("expected enough samples")
	}
	if q < 0.94 {
		t.Fatalf("quality=%v want >=0.94", q)
	}
	if cachePriorityForQuality(q) != AIObservationPriority {
		t.Fatalf("band=%d want main %d", cachePriorityForQuality(q), AIObservationPriority)
	}
}

func TestCacheQualityOf_FloorPattern(t *testing.T) {
	t.Parallel()
	// 2chat-like: some prefix cache, many 20k+ fully uncached rows.
	q, ok := cacheQualityOf(AccountTrafficStats{
		CacheEligibleRequests: 50,
		CacheEligibleTokens:   4_000_000,
		CacheReadTokens:       400_000,
		CacheBigMissRequests:  22,
	})
	if !ok {
		t.Fatal("expected enough samples")
	}
	if q >= cacheOverflowQuality {
		t.Fatalf("quality=%v should be spare-tier (< %.2f)", q, cacheOverflowQuality)
	}
	if cachePriorityForQuality(q) != AIPriorityBuriedThreshold {
		t.Fatalf("band=%d want spare %d", cachePriorityForQuality(q), AIPriorityBuriedThreshold)
	}
}

func TestApplyDeterministicCacheScores_ScalesOverall(t *testing.T) {
	t.Parallel()
	accounts := []Account{{ID: 1, Name: "good"}, {ID: 2, Name: "bad"}}
	scores := []AIAccountScore{
		{AccountID: 1, Overall: 80},
		{AccountID: 2, Overall: 80},
	}
	long := map[int64]AccountTrafficStats{
		1: {CacheEligibleRequests: 30, CacheEligibleTokens: 3_000_000, CacheReadTokens: 2_900_000},
		2: {CacheEligibleRequests: 30, CacheEligibleTokens: 3_000_000, CacheReadTokens: 200_000, CacheBigMissRequests: 20},
	}
	applyDeterministicCacheScores(scores, accounts, long, nil)
	if scores[1].Overall >= scores[0].Overall {
		t.Fatalf("bad cache overall=%v should be < good %v", scores[1].Overall, scores[0].Overall)
	}
	if !strings.Contains(scores[0].Note, "[cache quality=") {
		t.Fatalf("good note missing cache tag: %q", scores[0].Note)
	}
}

func TestInjectCachePriorityBands_KeepsBestTwoMain(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	cfg.OpSetPriority = boolPtr(true)
	accounts := []Account{
		{ID: 1, Name: "luky", Status: StatusActive, Schedulable: true, AIManaged: true, Priority: 100},
		{ID: 2, Name: "madou", Status: StatusActive, Schedulable: true, AIManaged: true, Priority: 100},
		{ID: 3, Name: "pool", Status: StatusActive, Schedulable: true, AIManaged: true, Priority: 100},
		{ID: 4, Name: "idle", Status: StatusActive, Schedulable: true, AIManaged: true, Priority: 100},
	}
	long := map[int64]AccountTrafficStats{
		1: {CacheEligibleRequests: 40, CacheEligibleTokens: 4_000_000, CacheReadTokens: 3_900_000},
		2: {CacheEligibleRequests: 40, CacheEligibleTokens: 4_000_000, CacheReadTokens: 3_700_000},
		3: {CacheEligibleRequests: 40, CacheEligibleTokens: 4_000_000, CacheReadTokens: 300_000, CacheBigMissRequests: 25},
		4: {CacheEligibleRequests: 2, CacheEligibleTokens: 10_000, CacheReadTokens: 1000}, // too few samples
	}
	d := decision{}
	n := injectCachePriorityBands(&d, accounts, cfg, long, nil)
	if n < 1 {
		t.Fatalf("expected at least the poor-cache sink, got %d actions %#v", n, d.Actions)
	}
	got := map[int64]int{}
	for _, a := range d.Actions {
		if a.Op != AIOpSetPriority {
			continue
		}
		v, _ := strconv.Atoi(a.Value)
		got[a.AccountID] = v
		if !isCachePriorityBandReason(a.Reason) {
			t.Fatalf("reason should be cache band: %q", a.Reason)
		}
	}
	if got[3] != AIPriorityBuriedThreshold {
		t.Fatalf("poor cache account priority=%d want %d (actions=%v)", got[3], AIPriorityBuriedThreshold, got)
	}
	if p, ok := got[1]; ok && p != AIObservationPriority {
		t.Fatalf("best account should stay/lift to 100, got %d", p)
	}
	if _, ok := got[4]; ok {
		t.Fatalf("undersampled account should not be moved: %v", got)
	}
}

func TestInjectCachePriorityBands_ForcesTwoMainsWhenAllPoor(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	cfg.OpSetPriority = boolPtr(true)
	accounts := []Account{
		{ID: 10, Name: "a", Status: StatusActive, Schedulable: true, AIManaged: true, Priority: 150},
		{ID: 11, Name: "b", Status: StatusActive, Schedulable: true, AIManaged: true, Priority: 150},
		{ID: 12, Name: "c", Status: StatusActive, Schedulable: true, AIManaged: true, Priority: 150},
	}
	// All poor, but 10 and 11 are least-poor.
	long := map[int64]AccountTrafficStats{
		10: {CacheEligibleRequests: 20, CacheEligibleTokens: 2_000_000, CacheReadTokens: 400_000, CacheBigMissRequests: 12},
		11: {CacheEligibleRequests: 20, CacheEligibleTokens: 2_000_000, CacheReadTokens: 350_000, CacheBigMissRequests: 13},
		12: {CacheEligibleRequests: 20, CacheEligibleTokens: 2_000_000, CacheReadTokens: 100_000, CacheBigMissRequests: 18},
	}
	d := decision{}
	n := injectCachePriorityBands(&d, accounts, cfg, long, nil)
	if n < 2 {
		t.Fatalf("want lifts for two least-poor, got %d %#v", n, d.Actions)
	}
	got := map[int64]int{}
	for _, a := range d.Actions {
		v, _ := strconv.Atoi(a.Value)
		got[a.AccountID] = v
	}
	if got[10] != AIObservationPriority || got[11] != AIObservationPriority {
		t.Fatalf("two best should be forced main, got %v", got)
	}
	if got[12] != 0 && got[12] != AIPriorityBuriedThreshold {
		t.Fatalf("worst should stay spare or be sunk, got %v", got)
	}
}

func TestInjectCachePriorityBands_SinksRecentFailoverStorm(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	cfg.OpSetPriority = boolPtr(true)
	accounts := []Account{
		{ID: 6506, Name: "mh-2key", Status: StatusActive, Schedulable: true, AIManaged: true, Priority: 100},
		{ID: 2, Name: "healthy", Status: StatusActive, Schedulable: true, AIManaged: true, Priority: 110},
	}
	long := map[int64]AccountTrafficStats{
		6506: {CacheEligibleRequests: 40, CacheEligibleTokens: 4_000_000, CacheReadTokens: 3_900_000},
		2:    {CacheEligibleRequests: 40, CacheEligibleTokens: 4_000_000, CacheReadTokens: 3_500_000},
	}
	recent := map[int64]AccountTrafficStats{
		6506: {Requests: 0, Successes: 0, Errors: 20},
	}
	d := decision{}
	n := injectCachePriorityBands(&d, accounts, cfg, long, recent)
	if n < 1 {
		t.Fatalf("expected 502-storm sink, got %d %#v", n, d.Actions)
	}
	got := map[int64]int{}
	for _, a := range d.Actions {
		if a.Op != AIOpSetPriority {
			continue
		}
		v, _ := strconv.Atoi(a.Value)
		got[a.AccountID] = v
	}
	if got[6506] != AIPriorityBuriedThreshold {
		t.Fatalf("502-storm account priority=%d want spare %d (actions=%v)", got[6506], AIPriorityBuriedThreshold, got)
	}
	if p, ok := got[2]; ok && p != AIObservationPriority {
		t.Fatalf("healthy cache account should be kept/lifted main, got %d", p)
	}
}

func TestFilterDeathSpiralKeepsCacheBandDemotion(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	acc := Account{ID: 7, Name: "pool", Status: StatusActive, Schedulable: true, AIManaged: true, Priority: 100}
	long := map[int64]AccountTrafficStats{7: {Requests: 80, Successes: 78, Errors: 2}}
	recent := map[int64]AccountTrafficStats{7: {Requests: 10, Successes: 10, Errors: 0}}
	acts := []decisionAction{{
		AccountID: 7, Op: AIOpSetPriority, Value: "150",
		Reason: cachePriorityBandPrefix + " quality=20%",
	}}
	kept := filterDeathSpiralDemotions(acts, []Account{acc}, long, recent, cfg)
	if len(kept) != 1 {
		t.Fatalf("cache-band demotion should survive health gate, kept=%d", len(kept))
	}
}
