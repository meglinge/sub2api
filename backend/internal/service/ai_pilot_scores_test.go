package service

import (
	"strings"
	"testing"
)

func TestNormalizeAccountScore_RecomputesOverall(t *testing.T) {
	t.Parallel()
	s := NormalizeAccountScore(AIAccountScore{
		AccountID: 1, Stability: 100, Latency: 100, Throughput: 100, Cost: 100,
		Overall: 10, Confidence: 0.9, Note: "n",
	})
	// confidence 0.9 should scale to 90
	if s.Confidence < 80 || s.Confidence > 100 {
		t.Fatalf("confidence scale: %v", s.Confidence)
	}
	// weighted: 0.4*100+0.3*100+0.2*100+0.1*100 = 100
	if s.Overall < 99 {
		t.Fatalf("overall should recompute near 100, got %v note=%q", s.Overall, s.Note)
	}
	if !strings.Contains(s.Note, "overall 由后端") {
		t.Fatalf("note should mention recompute: %q", s.Note)
	}
}

func TestNormalizeAccountScoreWith_CostHeavy(t *testing.T) {
	t.Parallel()
	// cheap (cost=100) but mediocre stability should beat expensive perfect when cost weight is high
	w := ScoreWeightsFromPercents(20, 20, 10, 50) // 50% cost
	cheap := NormalizeAccountScoreWith(AIAccountScore{
		Stability: 60, Latency: 60, Throughput: 60, Cost: 100, Overall: 0,
	}, w)
	expensive := NormalizeAccountScoreWith(AIAccountScore{
		Stability: 90, Latency: 90, Throughput: 90, Cost: 20, Overall: 0,
	}, w)
	if cheap.Overall <= expensive.Overall {
		t.Fatalf("cost-heavy: cheap overall=%v should beat expensive=%v", cheap.Overall, expensive.Overall)
	}
	// settings helper
	cfg := DefaultAIAutopilotSettings()
	cfg.ScoreWeightStability = 20
	cfg.ScoreWeightLatency = 20
	cfg.ScoreWeightThroughput = 10
	cfg.ScoreWeightCost = 50
	sw := cfg.ScoreWeights()
	if sw.Cost < 0.45 || sw.Cost > 0.55 {
		t.Fatalf("cost fraction=%v", sw.Cost)
	}
}

func TestApplyDeterministicCostScores_OverridesLLM(t *testing.T) {
	t.Parallel()
	pool := []Account{
		{ID: 1, Name: "cheap", Status: StatusActive, Schedulable: true, Extra: map[string]any{
			ExtraAIRateMultiplier: 0.05, ExtraAIRateSource: "newapi",
		}},
		{ID: 2, Name: "pricey", Status: StatusActive, Schedulable: true, Extra: map[string]any{
			ExtraAIRateMultiplier: 0.20, ExtraAIRateSource: "sub2api",
		}},
		{ID: 3, Name: "mystery", Status: StatusActive, Schedulable: true},
	}
	// LLM hallucinated cost=95 for everyone including mystery and pricey
	scores := []AIAccountScore{
		{AccountID: 1, Stability: 80, Latency: 80, Throughput: 80, Cost: 95, Overall: 90, Note: "llm"},
		{AccountID: 2, Stability: 90, Latency: 90, Throughput: 90, Cost: 95, Overall: 92, Note: "llm lied"},
		{AccountID: 3, Stability: 85, Latency: 85, Throughput: 85, Cost: 95, Overall: 91, Note: "invented 0.06"},
	}
	w := ScoreWeightsFromPercents(30, 20, 15, 35)
	applyDeterministicCostScores(scores, pool, w)
	if scores[1].Cost >= scores[0].Cost {
		t.Fatalf("pricey cost %v should be < cheap %v", scores[1].Cost, scores[0].Cost)
	}
	if scores[2].Cost != costScoreUnknownRate {
		t.Fatalf("mystery cost=%v want unknown %v", scores[2].Cost, costScoreUnknownRate)
	}
	if scores[1].Overall >= scores[0].Overall {
		// pricey has better stab/lat/thr but much worse cost under 35% weight
		// may or may not win overall depending on numbers — at least cost dim fixed
		t.Logf("overall cheap=%v pricey=%v (cost fixed)", scores[0].Overall, scores[1].Overall)
	}
	if !strings.Contains(scores[1].Note, "cost 由后端") {
		t.Fatalf("note should mark backend cost: %q", scores[1].Note)
	}
}

func TestNormalizeAccountScore_KeepsConsistentOverall(t *testing.T) {
	t.Parallel()
	s := NormalizeAccountScore(AIAccountScore{
		AccountID: 2, Stability: 50, Latency: 50, Throughput: 50, Cost: 50,
		Overall: 50, Confidence: 80,
	})
	if s.Overall != 50 {
		t.Fatalf("overall=%v", s.Overall)
	}
	if strings.Contains(s.Note, "overall 由后端") {
		t.Fatalf("should not recompute: %q", s.Note)
	}
}

func TestParseDecision_ScoresAndProbeRequests(t *testing.T) {
	t.Parallel()
	raw := `{
	  "summary":"round",
	  "actions":[{"accountId":9,"op":"enable","value":"","reason":"ok","confidence":0.9}],
	  "probeRequests":[{"accountId":9,"reason":"check","juice":false}],
	  "scores":[{"accountId":9,"stability":80,"latency":70,"throughput":60,"cost":50,"overall":68,"confidence":0.8,"note":"fine"}],
	  "observations":[]
	}`
	d, err := parseDecision(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Actions) != 1 || d.Actions[0].AccountID != 9 {
		t.Fatalf("actions=%+v", d.Actions)
	}
	if len(d.ProbeRequests) != 1 || d.ProbeRequests[0].AccountID != 9 {
		t.Fatalf("probes=%+v", d.ProbeRequests)
	}
	if len(d.Scores) != 1 || d.Scores[0].AccountID != 9 {
		t.Fatalf("scores=%+v", d.Scores)
	}
	if d.Scores[0].Stability != 80 {
		t.Fatalf("stability=%v", d.Scores[0].Stability)
	}
}

func TestActivationGateReason(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	// no probe data
	if reason := activationGateReason(AIOpEnable, "", 10, nil, 1, cfg); reason == "" {
		t.Fatal("enable without probe should reject")
	}
	probes := map[int64]activationResult{
		1: {AccountID: 1, Verdict: "pass", Fresh: true},
		2: {AccountID: 2, Verdict: "fail", Fresh: true, Error: "boom"},
	}
	if reason := activationGateReason(AIOpEnable, "", 10, probes, 1, cfg); reason != "" {
		t.Fatalf("pass should allow: %s", reason)
	}
	if reason := activationGateReason(AIOpEnable, "", 10, probes, 2, cfg); reason == "" {
		t.Fatal("fail should reject")
	}
	// weight 0 -> 10 needs probe
	if reason := activationGateReason(AIOpSetWeight, "10", 0, probes, 2, cfg); reason == "" {
		t.Fatal("0->weight with fail probe should reject")
	}
	// weight 10 -> 20 does not need probe
	if reason := activationGateReason(AIOpSetWeight, "20", 10, probes, 2, cfg); reason != "" {
		t.Fatalf("non-zero weight change should skip probe: %s", reason)
	}
	// probe off
	off := false
	cfg.ActivationProbeEnabled = &off
	if reason := activationGateReason(AIOpEnable, "", 10, nil, 1, cfg); reason != "" {
		t.Fatalf("probe off should allow: %s", reason)
	}
}

func TestLatencyScoreFromTTFB_AbsoluteNotRank(t *testing.T) {
	t.Parallel()
	fast, ok := latencyScoreFromTTFB(500)
	if !ok || fast != 100 {
		t.Fatalf("500ms=%v ok=%v want 100", fast, ok)
	}
	ok2k, ok := latencyScoreFromTTFB(2000)
	if !ok {
		t.Fatal("2000ms should be known")
	}
	// Old log-rank mapped 500 vs 2000 (4x) to 100 vs 5. Absolute: 2s is still "good".
	if ok2k < 80 || ok2k > 90 {
		t.Fatalf("2000ms=%v want ~85", ok2k)
	}
	slow, ok := latencyScoreFromTTFB(15000)
	if !ok || slow < 10 || slow > 20 {
		t.Fatalf("15s=%v want ~15", slow)
	}
	if _, ok := latencyScoreFromTTFB(0); ok {
		t.Fatal("0ms should be unknown")
	}
}

func TestThroughputScoreFromTPS_IgnoresVolume(t *testing.T) {
	t.Parallel()
	mid, ok := throughputScoreFromTPS(20)
	if !ok || mid < 55 || mid > 65 {
		t.Fatalf("20 tok/s=%v want ~60", mid)
	}
	fast, ok := throughputScoreFromTPS(80)
	if !ok || fast != 100 {
		t.Fatalf("80 tok/s=%v want 100", fast)
	}
	if _, ok := throughputScoreFromTPS(0); ok {
		t.Fatal("0 tps should be unknown")
	}
}

func TestApplyDeterministicScores_AbsoluteAndNoVolumeLoop(t *testing.T) {
	t.Parallel()
	pool := []Account{
		{ID: 1, Name: "busy-slow"},
		{ID: 2, Name: "quiet-fast"},
		{ID: 3, Name: "idle"},
	}
	long := map[int64]AccountTrafficStats{
		1: {
			AccountID: 1, Requests: 1000, Successes: 980, Errors: 20,
			AvgFirstToken: 8000, P50FirstToken: 7500, AvgDuration: 40000, AvgGenerationTPS: 8,
		},
		2: {
			AccountID: 2, Requests: 12, Successes: 12, Errors: 0,
			AvgFirstToken: 900, P50FirstToken: 800, AvgDuration: 3000, AvgGenerationTPS: 45,
		},
	}
	scores := applyDeterministicScores(nil, pool, long, nil, DefaultScoreWeights())
	byID := map[int64]AIAccountScore{}
	for _, s := range scores {
		byID[s.AccountID] = s
	}
	busy, quiet, idle := byID[1], byID[2], byID[3]
	if quiet.Latency <= busy.Latency {
		t.Fatalf("quiet TTFB 800ms latency=%v should beat busy 7.5s %v", quiet.Latency, busy.Latency)
	}
	if quiet.Throughput <= busy.Throughput {
		t.Fatalf("quiet 45t/s throughput=%v should beat busy 8t/s %v (volume must not win)", quiet.Throughput, busy.Throughput)
	}
	if busy.Throughput > 50 {
		t.Fatalf("busy 8t/s throughput=%v should be modest, not inflated by 1000 requests", busy.Throughput)
	}
	if idle.Overall != 0 {
		t.Fatalf("idle overall=%v want 0 so cost-only cannot promote it", idle.Overall)
	}
	if !strings.Contains(idle.Note, "稳—") || !strings.Contains(idle.Note, "延迟—") || !strings.Contains(idle.Note, "流畅—") {
		t.Fatalf("idle note should mark missing dims: %q", idle.Note)
	}
	if busy.Stability < 90 {
		t.Fatalf("busy stability=%v want ~98", busy.Stability)
	}
	if quiet.Overall <= busy.Overall {
		t.Fatalf("quiet overall=%v should beat busy %v", quiet.Overall, busy.Overall)
	}
}

func TestApplyDeterministicScores_UnknownDimsSkippedInOverall(t *testing.T) {
	t.Parallel()
	pool := []Account{{ID: 1, Name: "errors-only"}}
	long := map[int64]AccountTrafficStats{
		1: {AccountID: 1, Requests: 0, Successes: 0, Errors: 10},
	}
	scores := applyDeterministicScores(nil, pool, long, nil, DefaultScoreWeights())
	if len(scores) != 1 {
		t.Fatalf("scores=%d", len(scores))
	}
	s := scores[0]
	if s.Stability != 0 || !strings.Contains(s.Note, "稳0") {
		t.Fatalf("all-error stability=%v note=%q", s.Stability, s.Note)
	}
	if !strings.Contains(s.Note, "延迟—") || !strings.Contains(s.Note, "流畅—") {
		t.Fatalf("missing ttfb/tps should be dashed: %q", s.Note)
	}
	// Stability 0 + cost, latency/throughput omitted. Must stay low, not get unknown=40 drag to ~24.
	if s.Overall > 15 {
		t.Fatalf("overall=%v should stay near 0 after skipping unknown dims", s.Overall)
	}
}

func TestApplyDeterministicScores_PrefersP50TTFB(t *testing.T) {
	t.Parallel()
	pool := []Account{{ID: 1, Name: "skewed"}}
	long := map[int64]AccountTrafficStats{
		1: {
			AccountID: 1, Requests: 20, Successes: 20, Errors: 0,
			AvgFirstToken: 12000, P50FirstToken: 1500, AvgGenerationTPS: 30,
		},
	}
	scores := applyDeterministicScores(nil, pool, long, nil, DefaultScoreWeights())
	lat := scores[0].Latency
	want, _ := latencyScoreFromTTFB(1500)
	if lat != want {
		t.Fatalf("latency=%v want p50-based %v (not mean 12s)", lat, want)
	}
}

func TestClampPersistedAccountScore_KeepsOverall(t *testing.T) {
	t.Parallel()
	s := ClampPersistedAccountScore(AIAccountScore{
		Stability: 90, Latency: 80, Throughput: 70, Cost: 40,
		Overall: 51.2, Confidence: 0.9, Note: "keep",
	})
	if s.Overall != 51.2 {
		t.Fatalf("overall clobbered: %v", s.Overall)
	}
	if s.Confidence < 80 {
		t.Fatalf("confidence should scale 0.9→90, got %v", s.Confidence)
	}
}

func TestInjectScoreDrivenWeights_SkipsIdleOverall(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	accounts := []Account{
		{ID: 1, Name: "idle", Status: StatusActive, Schedulable: true, AIManaged: true, ScheduleWeight: 50},
		{ID: 2, Name: "live", Status: StatusActive, Schedulable: true, AIManaged: true, ScheduleWeight: 50},
	}
	d := decision{
		Scores: []AIAccountScore{
			{AccountID: 1, Overall: 0, Note: "后端绝对分 稳— 延迟— 流畅— 性价比90"},
			{AccountID: 2, Overall: 82, Stability: 98, Latency: 85, Throughput: 70, Cost: 60, Note: "后端绝对分 稳98(20/20)"},
		},
	}
	n := injectScoreDrivenWeights(&d, accounts, cfg, nil)
	if n != 1 {
		t.Fatalf("injected=%d want 1 (idle skipped) actions=%v", n, d.Actions)
	}
	if d.Actions[0].AccountID != 2 {
		t.Fatalf("expected live account, got %+v", d.Actions)
	}
}
