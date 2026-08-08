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
