package service

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestCompositeRateMultiplier_Recharge10(t *testing.T) {
	t.Parallel()
	// rate=1, 1:10 recharge → composite 0.1
	got := CompositeRateMultiplier(1, 10)
	if got != 0.1 {
		t.Fatalf("composite=%v want 0.1", got)
	}
	// rate=0.5, recharge=10 → 0.05
	if CompositeRateMultiplier(0.5, 10) != 0.05 {
		t.Fatalf("got %v", CompositeRateMultiplier(0.5, 10))
	}
	// defaults
	if CompositeRateMultiplier(0, 0) != 1 {
		t.Fatalf("defaults should yield 1, got %v", CompositeRateMultiplier(0, 0))
	}
}

func TestBuildRateConfidence_Levels(t *testing.T) {
	t.Parallel()
	high := BuildRateConfidence(0.5, 1, "newapi")
	if high.Level != 1 || high.Label != "high" || high.TrustedComposite != 0.5 {
		t.Fatalf("imported high: %+v", high)
	}
	med := BuildRateConfidence(0.8, 1, "custom")
	if med.Level != 2 {
		t.Fatalf("custom non-1: %+v", med)
	}
	low := BuildRateConfidence(1, 1, "default_one")
	if low.Level != 3 || low.Label != "low" {
		t.Fatalf("default: %+v", low)
	}
	// 1:10 recharge with imported
	rc := BuildRateConfidence(1, 10, "imported")
	if rc.TrustedComposite != 0.1 || rc.Level != 1 {
		t.Fatalf("1:10 imported: %+v", rc)
	}
}

func TestMoneyView_BillingProbeAndRecharge(t *testing.T) {
	t.Parallel()
	r := 0.6
	acc := &Account{
		RateMultiplier: &r,
		Extra: map[string]any{
			ExtraRechargeMultiplier: 10.0,
			UpstreamBillingProbeExtraKey: map[string]any{
				"status": "ok",
				"data": map[string]any{
					"resolved_rate_multiplier": 0.4,
				},
			},
		},
	}
	m := moneyView(acc)
	// billing probe wins over column
	if m["rateMultiplier"].(float64) != 0.4 {
		t.Fatalf("rate=%v", m["rateMultiplier"])
	}
	if m["rechargeMultiplier"].(float64) != 10 {
		t.Fatalf("recharge=%v", m["rechargeMultiplier"])
	}
	comp := m["compositeRateMultiplier"].(float64)
	if comp != 0.04 {
		t.Fatalf("composite=%v want 0.04", comp)
	}
	rc := m["rateConfidence"].(map[string]any)
	if rc["level"].(int) != 1 {
		t.Fatalf("confidence level=%v", rc["level"])
	}
	if rc["trustedComposite"].(float64) != 0.04 {
		t.Fatalf("trusted=%v", rc["trustedComposite"])
	}
}

func TestParseOneAPIGroupRatioMap_Shapes(t *testing.T) {
	t.Parallel()
	// self/groups shape
	body1 := []byte(`{"success":true,"data":{"vip":{"ratio":0.5,"desc":"VIP"},"default":{"ratio":1,"desc":"d"}}}`)
	g1 := ParseOneAPIGroupRatioMap(body1)
	if len(g1) != 2 {
		t.Fatalf("g1=%+v", g1)
	}
	m := map[string]float64{}
	for _, g := range g1 {
		m[g.Name] = g.Ratio
	}
	if m["vip"] != 0.5 {
		t.Fatalf("vip=%v", m["vip"])
	}
	// pricing group_ratio
	body2 := []byte(`{"group_ratio":{"vip":0.5,"std":1.2}}`)
	g2 := ParseOneAPIGroupRatioMap(body2)
	if len(g2) != 2 {
		t.Fatalf("g2=%+v", g2)
	}
}

func TestResolveRateFromTokenAndGroups(t *testing.T) {
	t.Parallel()
	groups := []OneAPIGroup{{Name: "vip", Ratio: 0.5}, {Name: "default", Ratio: 1}}
	tokens := []OneAPIToken{{ID: "1", Key: "sk-abc12345deadbeef", Group: "vip", Enabled: true, RemainQuota: 2_500_000}}
	r, g, remain, ok := ResolveRateFromTokenAndGroups("sk-abc12345deadbeef", tokens, groups)
	if !ok || r != 0.5 || g != "vip" || remain != 2_500_000 {
		t.Fatalf("r=%v g=%s remain=%d ok=%v", r, g, remain, ok)
	}
	// missing mgmt / no match
	_, _, _, ok = ResolveRateFromTokenAndGroups("sk-other", tokens, groups)
	if ok {
		t.Fatal("should not match other key")
	}
}

func TestParseOneAPIBalanceSelf_Depleted(t *testing.T) {
	t.Parallel()
	st, usd, err := ParseOneAPIBalanceSelf([]byte(`{"success":true,"data":{"quota":0,"used_quota":100}}`))
	if err != nil || st != "depleted" || usd != 0 {
		t.Fatalf("st=%s usd=%v err=%v", st, usd, err)
	}
	st, usd, err = ParseOneAPIBalanceSelf([]byte(`{"success":true,"data":{"quota":500000}}`))
	if err != nil || st != "ok" || usd != 1 {
		t.Fatalf("st=%s usd=%v err=%v", st, usd, err)
	}
	st, usd, err = ParseOneAPIBalanceSelf([]byte(`{"success":true,"data":{"quota":0,"unlimited_quota":true}}`))
	if err != nil || st != "unlimited" {
		t.Fatalf("unlimited st=%s usd=%v err=%v", st, usd, err)
	}
}

func TestParseAPIKeyTokenUsageBalance(t *testing.T) {
	t.Parallel()
	// 小刀-style: unlimited
	st, usd, ok := ParseAPIKeyTokenUsageBalance([]byte(`{"code":true,"data":{"object":"token_usage","total_available":-1,"unlimited_quota":true},"message":"ok"}`))
	if !ok || st != "unlimited" {
		t.Fatalf("unlimited st=%s usd=%v ok=%v", st, usd, ok)
	}
	// finite remaining
	st, usd, ok = ParseAPIKeyTokenUsageBalance([]byte(`{"code":true,"data":{"object":"token_usage","total_available":1000000,"unlimited_quota":false}}`))
	if !ok || st != "ok" || usd != 2 {
		t.Fatalf("finite st=%s usd=%v ok=%v", st, usd, ok)
	}
	// depleted
	st, usd, ok = ParseAPIKeyTokenUsageBalance([]byte(`{"code":true,"data":{"object":"token_usage","total_available":0,"unlimited_quota":false}}`))
	if !ok || st != "depleted" {
		t.Fatalf("depleted st=%s usd=%v ok=%v", st, usd, ok)
	}
}

func TestParseSub2APIBillingRate(t *testing.T) {
	t.Parallel()
	r, ok := ParseSub2APIBillingRate([]byte(`{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":0.05,"resolved_rate_multiplier":0.05,"effective_rate_multiplier":0.05}`))
	if !ok || r != 0.05 {
		t.Fatalf("r=%v ok=%v", r, ok)
	}
	_, ok = ParseSub2APIBillingRate([]byte(`{"object":"other"}`))
	if ok {
		t.Fatal("should reject non-sub2api")
	}
}



func TestBalanceGateReason_Depleted(t *testing.T) {
	t.Parallel()
	acc := &Account{Extra: map[string]any{ExtraAIBalanceStatus: "depleted", ExtraAIBalanceUSD: 0.0}}
	if reason := balanceGateReason(AIOpEnable, acc); reason == "" {
		t.Fatal("expected depleted reject")
	}
	if reason := balanceGateReason(AIOpSetWeight, acc); reason != "" {
		t.Fatalf("weight should not use balance gate: %s", reason)
	}
	acc.Extra[ExtraAIBalanceStatus] = "ok"
	if reason := balanceGateReason(AIOpEnable, acc); reason != "" {
		t.Fatalf("ok should pass: %s", reason)
	}
}

func TestRecoveryObservationPriority(t *testing.T) {
	t.Parallel()
	if !ShouldUnburyPriority(9000) {
		t.Fatal("9000 should unbury")
	}
	if !ShouldUnburyPriority(AIMaxPriority + 1) {
		t.Fatal("above max band should unbury")
	}
	if ShouldUnburyPriority(AIObservationPriority) {
		t.Fatal("observation tier should not unbury")
	}
	if ShouldUnburyPriority(120) {
		t.Fatal("in-band 120 should not unbury")
	}
	if RecoveryObservationPriority(9000) != AIObservationPriority {
		t.Fatalf("got %d", RecoveryObservationPriority(9000))
	}
	if RecoveryObservationPriority(100) != 100 {
		t.Fatalf("keep 100")
	}
}

func TestApplyAIOp_SetPriorityClampsToMax(t *testing.T) {
	t.Parallel()
	acc := &Account{
		ID: 91, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true,
		AIManaged: true, Priority: 10, ScheduleWeight: 10,
	}
	repo := newMemoryAccountRepo(acc)
	p := &AIPilotService{Accounts: repo}
	_, after, err := p.ApplyAIOp(context.Background(), 91, AIOpSetPriority, "50000")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetByID(context.Background(), 91)
	if got.Priority != AIMaxPriority {
		t.Fatalf("priority=%d after=%s want clamp %d", got.Priority, after, AIMaxPriority)
	}
	_, _, err = p.ApplyAIOp(context.Background(), 91, AIOpSetPriority, "1")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = repo.GetByID(context.Background(), 91)
	if got.Priority != AIMinPriority {
		t.Fatalf("priority=%d want floor %d", got.Priority, AIMinPriority)
	}
}

func TestRPMLoosenGate(t *testing.T) {
	t.Parallel()
	if reason := rpmLoosenGateReason(10, 100, false, true); reason == "" {
		t.Fatal("10→100 is >4x, should reject")
	}
	if reason := rpmLoosenGateReason(10, 20, false, true); reason != "" {
		t.Fatalf("2x should pass: %s", reason)
	}
	if reason := rpmLoosenGateReason(10, 20, true, true); reason == "" {
		t.Fatal("429 should block loosen")
	}
	if reason := rpmLoosenGateReason(100, 50, false, true); reason != "" {
		t.Fatalf("tighten ok: %s", reason)
	}
}

func TestParseLLMChatCompletionsBody(t *testing.T) {
	t.Parallel()
	// empty
	if _, _, _, err := parseLLMChatCompletionsBody(nil); err == nil {
		t.Fatal("empty should error")
	}
	// truncated
	if _, _, _, err := parseLLMChatCompletionsBody([]byte(`{"choices":[{"message":{"content":"hi`)); err == nil || !strings.Contains(err.Error(), "截断") {
		t.Fatalf("truncated err=%v", err)
	}
	// valid
	body, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": `{"summary":"ok"}`}}},
		"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 2},
	})
	c, in, out, err := parseLLMChatCompletionsBody(body)
	if err != nil || c == "" || in != 1 || out != 2 {
		t.Fatalf("c=%q in=%d out=%d err=%v", c, in, out, err)
	}
}

func TestInjectRecovery_UnburiesPriority(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	accounts := []Account{
		{ID: 1, AIDisabled: true, Priority: 9000, ScheduleWeight: 1},
		{ID: 2, AIDisabled: false, Priority: 9500, ScheduleWeight: 10}, // enabled but deep exile
		// sticky spare tier (麻豆类): p=150 healthy long window → soft unbury
		{ID: 3, AIDisabled: false, Priority: 150, ScheduleWeight: 3, Status: StatusActive, Schedulable: true},
	}
	probes := map[int64]activationResult{
		1: {AccountID: 1, Verdict: "pass", Fresh: true, Source: "upstream"},
	}
	long := map[int64]AccountTrafficStats{
		3: {Requests: 100, Successes: 95, Errors: 5},
	}
	d := decision{}
	n := injectRecoveryEnables(&d, accounts, probes, long, nil, cfg)
	if n < 3 {
		t.Fatalf("expected enable+priority, deep unbury, soft spare unbury, n=%d acts=%+v", n, d.Actions)
	}
	wantPri := strconv.Itoa(AIObservationPriority) // observation layer = 100 in [50,200] band
	var sawEnable, sawPri1, sawPri2, sawPri3 bool
	for _, a := range d.Actions {
		if a.AccountID == 1 && a.Op == AIOpEnable {
			sawEnable = true
		}
		if a.AccountID == 1 && a.Op == AIOpSetPriority && a.Value == wantPri {
			sawPri1 = true
		}
		if a.AccountID == 2 && a.Op == AIOpSetPriority && a.Value == wantPri {
			sawPri2 = true
		}
		if a.AccountID == 3 && a.Op == AIOpSetPriority && a.Value == wantPri {
			sawPri3 = true
		}
	}
	if !sawEnable || !sawPri1 || !sawPri2 || !sawPri3 {
		t.Fatalf("enable=%v pri1=%v pri2=%v pri3=%v wantPri=%s acts=%+v", sawEnable, sawPri1, sawPri2, sawPri3, wantPri, d.Actions)
	}
}

func TestPriorityDemotionGate_BlocksStickySpare(t *testing.T) {
	t.Parallel()
	acc := &Account{ID: 6154, Priority: 100, Status: StatusActive, Schedulable: true}
	long := AccountTrafficStats{Requests: 100, Successes: 95, Errors: 5}
	recent := AccountTrafficStats{Requests: 0, Successes: 0, Errors: 0}
	if reason := priorityDemotionGateReason(acc, 150, long, recent); reason == "" {
		t.Fatal("expected reject demotion to 150 with empty recent + healthy long")
	}
	// hard recent fail allows demotion
	recentBad := AccountTrafficStats{Requests: 20, Successes: 10, Errors: 12}
	if reason := priorityDemotionGateReason(acc, 150, long, recentBad); reason != "" {
		t.Fatalf("hard recent fail should allow demotion: %s", reason)
	}
}

func TestWeightCrushAndDisableGates(t *testing.T) {
	t.Parallel()
	acc := &Account{ID: 1, Priority: 100, ScheduleWeight: 10, Status: StatusActive, Schedulable: true}
	long := AccountTrafficStats{Requests: 50, Successes: 48, Errors: 2}
	recent := AccountTrafficStats{Requests: 0}
	if reason := weightCrushGateReason(acc, 1, long, recent); reason == "" {
		t.Fatal("expected reject weight crush")
	}
	if reason := disableHealthyGateReason(acc, long, recent); reason == "" {
		t.Fatal("expected reject disable on healthy")
	}
	recentBad := AccountTrafficStats{Requests: 10, Successes: 2, Errors: 10}
	if reason := disableHealthyGateReason(acc, long, recentBad); reason != "" {
		t.Fatalf("hard fail should allow disable: %s", reason)
	}
}

func TestIsRecoveryUnburyPriority_BypassHelpers(t *testing.T) {
	t.Parallel()
	// deep exile → 100
	if !isRecoveryUnburyPriority(9000, 100) {
		t.Fatal("deep unbury should count")
	}
	// soft spare → 100
	if !isRecoveryUnburyPriority(150, 100) {
		t.Fatal("soft spare unbury should count")
	}
	// demotion should not
	if isRecoveryUnburyPriority(100, 150) {
		t.Fatal("demotion is not recovery")
	}
}

func TestFilterDeathSpiralDemotions(t *testing.T) {
	t.Parallel()
	accounts := []Account{{ID: 1, Priority: 100, Status: StatusActive, Schedulable: true}}
	long := map[int64]AccountTrafficStats{1: {Requests: 80, Successes: 76, Errors: 4}}
	recent := map[int64]AccountTrafficStats{1: {}}
	acts := []decisionAction{
		{AccountID: 1, Op: AIOpSetPriority, Value: "150", Reason: "sink"},
		{AccountID: 1, Op: AIOpSetWeight, Value: "8", Reason: "ok"},
	}
	out := filterDeathSpiralDemotions(acts, accounts, long, recent, DefaultAIAutopilotSettings())
	if len(out) != 1 || out[0].Op != AIOpSetWeight {
		t.Fatalf("expected only weight kept, got %+v", out)
	}
}

func TestCostPressureDemotionAndPromotionGates(t *testing.T) {
	t.Parallel()
	rate := func(v float64) *float64 { return &v }
	// cheap median ≈0.06; Pro 0.11 is very expensive (>1.75×)
	cheap := Account{
		ID: 1, Name: "cheap", Priority: 100, Status: StatusActive, Schedulable: true,
		RateMultiplier: rate(0.06),
	}
	// store as ai_rate so resolveAccountRate finds imported source
	cheap.Extra = map[string]any{
		ExtraAIRateMultiplier: 0.06,
		ExtraAIRateSource:     "newapi",
	}
	pro := Account{
		ID: 2, Name: "pro", Priority: 100, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{
			ExtraAIRateMultiplier: 0.11,
			ExtraAIRateSource:     "sub2api",
		},
	}
	unknown := Account{
		ID: 3, Name: "unknown", Priority: 100, Status: StatusActive, Schedulable: true,
		// default rate=1 no import → unknown
	}
	pool := []Account{cheap, pro, unknown}

	cfg := DefaultAIAutopilotSettings()
	cfg.ScoreWeightStability = 30
	cfg.ScoreWeightLatency = 20
	cfg.ScoreWeightThroughput = 15
	cfg.ScoreWeightCost = 35
	if !costPressureActive(cfg) {
		t.Fatal("35% cost should activate pressure")
	}
	// 0.11 / 0.06 ≈ 1.83 → above 1.45 and 1.75 vs cheapest peer
	if !isExpensiveVsPeers(&pro, pool, AICostExpensiveRatio) {
		t.Fatal("pro should be expensive vs cheapest peer")
	}
	if !costJustifiedSpareDemotion(&pro, pool, cfg) {
		t.Fatal("pro should be cost-justified spare demotion")
	}
	// Healthy demotion normally blocked…
	long := AccountTrafficStats{Requests: 100, Successes: 98, Errors: 2}
	recent := AccountTrafficStats{Requests: 10, Successes: 10, Errors: 0}
	if reason := priorityDemotionGateReason(&pro, 150, long, recent); reason == "" {
		t.Fatal("without cost, healthy demotion should block")
	}
	// …but allowed under cost justification
	if reason := priorityDemotionGateReasonEx(&pro, 150, long, recent, true); reason != "" {
		t.Fatalf("cost justified should allow demotion: %s", reason)
	}
	// Promotion blocked
	pro.Priority = 150
	if reason := priorityCostPromotionGateReason(&pro, 100, pool, cfg); reason == "" {
		t.Fatal("expected block promote expensive to main")
	}
	// Unknown is not "expensive" by ratio (no known rate)
	if isExpensiveVsPeers(&unknown, pool, AICostExpensiveRatio) {
		t.Fatal("unknown rate must not count as expensive")
	}

	// Deterministic cost scores: pro low, unknown mid, cheap high
	scores := deriveCostScoreMap(pool)
	if scores[cheap.ID] <= scores[pro.ID] {
		t.Fatalf("cheap cost score %v should beat pro %v", scores[cheap.ID], scores[pro.ID])
	}
	if scores[unknown.ID] != costScoreUnknownRate {
		t.Fatalf("unknown cost=%v want %v", scores[unknown.ID], costScoreUnknownRate)
	}

	// inject demotes pro
	d := decision{}
	n := injectCostSpareDemotions(&d, pool, cfg)
	if n != 1 || len(d.Actions) != 1 || d.Actions[0].AccountID != pro.ID {
		t.Fatalf("inject cost demotion: n=%d acts=%+v", n, d.Actions)
	}

	// filter keeps cost demotion under pressure
	pro.Priority = 100
	acts := []decisionAction{{AccountID: pro.ID, Op: AIOpSetPriority, Value: "150"}}
	kept := filterDeathSpiralDemotions(acts, pool, map[int64]AccountTrafficStats{pro.ID: long}, map[int64]AccountTrafficStats{pro.ID: recent}, cfg)
	if len(kept) != 1 {
		t.Fatalf("cost demotion should be kept, got %+v", kept)
	}

	// soft-unbury blocked for expensive under pressure
	if !costBlocksMainPromotion(&pro, pool, cfg) {
		t.Fatal("expected cost block main promotion")
	}
}

func TestBuildGroupPeers_HasComposite(t *testing.T) {
	t.Parallel()
	chs := []map[string]any{
		{
			"id": int64(1), "groups": []int64{5}, "weight": 10, "priority": 1,
			"state":   map[string]any{"aiDisabled": false, "schedulable": true, "status": StatusActive},
			"traffic": map[string]any{"requests": 10, "successRate": 0.9, "avgTtfbMs": 100.0},
			"money": map[string]any{
				"compositeRateMultiplier": 0.5,
				"rateConfidence":          map[string]any{"level": 1, "trustedComposite": 0.5},
			},
		},
		{
			"id": int64(2), "groups": []int64{5}, "weight": 5, "priority": 2,
			"state":   map[string]any{"aiDisabled": false, "schedulable": true, "status": StatusActive},
			"traffic": map[string]any{"requests": 5, "successRate": 1.0, "avgTtfbMs": 80.0},
			"money": map[string]any{
				"compositeRateMultiplier": 1.0,
				"rateConfidence":          map[string]any{"level": 3, "trustedComposite": 1.0},
			},
		},
	}
	ga := map[int64]map[string]any{5: {"id": int64(5), "channelCount": 2, "availableCount": 2}}
	groups := buildGroupPeers(chs, ga)
	if len(groups) != 1 {
		t.Fatalf("groups=%+v", groups)
	}
	peers, _ := groups[0]["peers"].([]map[string]any)
	if len(peers) != 2 {
		t.Fatalf("peers=%+v", peers)
	}
	if peers[0]["compositeRate"].(float64) != 0.5 {
		t.Fatalf("peer0 composite=%v", peers[0]["compositeRate"])
	}
	avg := groups[0]["peerAvg"].(map[string]any)
	if _, ok := avg["compositeRate"]; !ok {
		t.Fatalf("peerAvg missing compositeRate: %+v", avg)
	}
}
