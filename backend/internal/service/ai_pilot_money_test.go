package service

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
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

func TestSoftUnburyCheapSpareRescue_EmptyLongWindow(t *testing.T) {
	t.Parallel()
	// Sy-class bug: known-cheap at p=150, zero long samples under strict layering.
	// Classic softUnburyEligible requires ln≥10 and would leave them forever at p150.
	rateSy := 0.05
	ratePeer := 0.10
	sy := Account{
		ID: 6399, Name: "Sy", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, AIManaged: true,
		Priority: 150, ScheduleWeight: 2940, RateMultiplier: &rateSy,
		Extra: map[string]any{
			ExtraAIRateMultiplier:   0.05,
			ExtraAIRateSource:       "newapi",
			ExtraRechargeMultiplier: 1.0,
		},
	}
	main := Account{
		ID: 1, Name: "main", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, AIManaged: true,
		Priority: 100, ScheduleWeight: 100, RateMultiplier: &ratePeer,
		Extra: map[string]any{
			ExtraAIRateMultiplier: 0.10,
			ExtraAIRateSource:     "newapi",
		},
	}
	// expensive spare — must NOT be rescued by cheap path
	ratePro := 1.0
	pro := Account{
		ID: 2, Name: "Pro", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, AIManaged: true,
		Priority: 150, ScheduleWeight: 10, RateMultiplier: &ratePro,
		Extra: map[string]any{
			ExtraAIRateMultiplier: 1.0,
			ExtraAIRateSource:     "newapi",
		},
	}
	accounts := []Account{sy, main, pro}
	empty := map[int64]AccountTrafficStats{
		6399: {}, // 0 samples — the death spiral
		2:    {},
	}
	// classic path rejects empty long
	if softUnburyEligible(empty[6399], empty[6399]) {
		t.Fatal("classic softUnburyEligible must reject empty long window")
	}
	if !softUnburyCheapSpareRescue(&sy, accounts, empty[6399], empty[6399], nil) {
		t.Fatal("cheap rescue must allow Sy with empty long + known cheap composite")
	}
	if softUnburyCheapSpareRescue(&pro, accounts, empty[2], empty[2], nil) {
		t.Fatal("expensive Pro must not use cheap rescue")
	}

	cfg := DefaultAIAutopilotSettings()
	// high 性价比 so cost pressure is on (still unburies cheap, blocks expensive)
	cfg.ScoreWeightCost = 40
	cfg.ScoreWeightStability = 25
	cfg.ScoreWeightLatency = 20
	cfg.ScoreWeightThroughput = 15
	d := decision{}
	// model only bumps weight at p150 (the wrong action user saw)
	d.Actions = []decisionAction{{
		AccountID: 6399, Op: AIOpSetWeight, Value: "2960",
		Reason: "同层+20(勿抬p100主层已3满池)", Confidence: 0.7,
	}}
	n := injectRecoveryEnables(&d, accounts, nil, empty, empty, cfg, nil, time.Time{})
	if n < 1 {
		t.Fatalf("expected cheap spare unbury inject, n=%d acts=%+v", n, d.Actions)
	}
	wantPri := strconv.Itoa(AIObservationPriority)
	var sawSyPri, sawProPri bool
	for _, a := range d.Actions {
		if a.AccountID == 6399 && a.Op == AIOpSetPriority && a.Value == wantPri {
			sawSyPri = true
			if !strings.Contains(a.Reason, "便宜") && !strings.Contains(a.Reason, "分层") &&
				!strings.Contains(a.Reason, "软调度") && !strings.Contains(a.Reason, "非极贵") {
				t.Fatalf("reason should mention cheap/layering/soft-schedule fix: %s", a.Reason)
			}
		}
		if a.AccountID == 2 && a.Op == AIOpSetPriority {
			sawProPri = true
		}
	}
	if !sawSyPri {
		t.Fatalf("Sy must be lifted to p%d, acts=%+v", AIObservationPriority, d.Actions)
	}
	if sawProPri {
		t.Fatalf("expensive Pro must not be auto-lifted, acts=%+v", d.Actions)
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
	n := injectRecoveryEnables(&d, accounts, probes, long, nil, cfg, nil, time.Time{})
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
		t.Fatalf("majority-fail recent should allow disable: %s", reason)
	}
	// 3-minute blip: 34 billed + 12 errors = 73.9% — NOT disable-worthy.
	recentBlip := AccountTrafficStats{Requests: 34, Successes: 34, Errors: 12}
	if reason := disableHealthyGateReason(acc, long, recentBlip); reason == "" {
		t.Fatal("expected reject disable on 74% recent SR (transient 502/504)")
	}
	// Depleted must allow disable even with healthy long/recent windows.
	accDep := &Account{
		ID: 2, Priority: 150, ScheduleWeight: 10, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{ExtraAIBalanceStatus: "depleted", ExtraAIBalanceUSD: 0.0},
	}
	if reason := disableHealthyGateReason(accDep, long, recent); reason != "" {
		t.Fatalf("depleted should allow disable: %s", reason)
	}
	// Transient probe (timeout/502) must NOT open disable.
	timeoutProbe := activationResult{AccountID: 1, Verdict: "fail", Fresh: true, Error: "deadline exceeded"}
	if reason := disableHealthyGateReasonEx(acc, long, recent, &timeoutProbe); reason == "" {
		t.Fatal("timeout probe must not allow disable")
	}
	slowProbe := activationResult{AccountID: 1, Verdict: "slow", Fresh: true, Error: "HTTP 503 responses model=gpt-5.6-sol"}
	if reason := disableHealthyGateReasonEx(acc, long, recent, &slowProbe); reason == "" {
		t.Fatal("503 probe must not allow disable")
	}
	// Fatal auth/quota probe may disable.
	quotaProbe := activationResult{AccountID: 1, Verdict: "fail", Fresh: true, Error: "HTTP 403 额度不足"}
	if reason := disableHealthyGateReasonEx(acc, long, recent, &quotaProbe); reason != "" {
		t.Fatalf("quota 403 should allow disable: %s", reason)
	}
}

func TestCooldownZeroHonorsNoFloor(t *testing.T) {
	t.Parallel()
	// With channel_cooldown_minutes=0, demotion-like ops must not get the 15m floor.
	cfg := DefaultAIAutopilotSettings()
	cfg.ChannelCooldownMinutes = 0
	cool := time.Duration(cfg.ChannelCooldownMinutes) * time.Minute
	acc := &Account{Priority: 100, ScheduleWeight: 100}
	act := decisionAction{Op: AIOpSetWeight, Value: "80"}
	if isDemotionLike(act, acc) && cfg.ChannelCooldownMinutes > 0 && cool < AIDemotionMinCooldown {
		cool = AIDemotionMinCooldown
	}
	if cool != 0 {
		t.Fatalf("explicit cooldown 0 must stay 0, got %v", cool)
	}
	// When cooldown>0 but tiny, floor applies for demotion-like.
	cfg.ChannelCooldownMinutes = 1
	cool = time.Duration(cfg.ChannelCooldownMinutes) * time.Minute
	if isDemotionLike(act, acc) && cfg.ChannelCooldownMinutes > 0 && cool < AIDemotionMinCooldown {
		cool = AIDemotionMinCooldown
	}
	if cool != AIDemotionMinCooldown {
		t.Fatalf("tiny positive cooldown should floor to %v, got %v", AIDemotionMinCooldown, cool)
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
	if !isExpensiveVsPeers(&pro, pool, AICostExpensiveRatio, nil) {
		t.Fatal("pro should be expensive vs cheapest peer")
	}
	if !costJustifiedSpareDemotion(&pro, pool, cfg, nil) {
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
	if reason := priorityCostPromotionGateReason(&pro, 100, pool, cfg, nil); reason == "" {
		t.Fatal("expected block promote expensive to main")
	}
	// Unknown is not "expensive" by ratio (no known rate)
	if isExpensiveVsPeers(&unknown, pool, AICostExpensiveRatio, nil) {
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
	n := injectCostSpareDemotions(&d, pool, cfg, nil)
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
	if !costBlocksMainPromotion(&pro, pool, cfg, nil) {
		t.Fatal("expected cost block main promotion")
	}
}

func TestCostIsolationDisableAndBlockEnable(t *testing.T) {
	t.Parallel()
	// Wawapi-class: 0.11 vs cheapest 0.04/0.06, spare tier, still schedulable.
	cheap := Account{
		ID: 1, Name: "cheap", Priority: 100, Status: StatusActive, Schedulable: true,
		GroupIDs: []int64{2},
		Extra: map[string]any{
			ExtraAIRateMultiplier: 0.06,
			ExtraAIRateSource:     "sub2api",
		},
	}
	pro := Account{
		ID: 2, Name: "Wawapi (Pro)", Priority: 150, ScheduleWeight: 10,
		Status: StatusActive, Schedulable: true, GroupIDs: []int64{2},
		Extra: map[string]any{
			ExtraAIRateMultiplier: 0.11,
			ExtraAIRateSource:     "sub2api",
		},
	}
	pool := []Account{cheap, pro}
	cfg := DefaultAIAutopilotSettings()
	cfg.ScoreWeightCost = 40
	cfg.ScoreWeightStability = 25
	cfg.ScoreWeightLatency = 20
	cfg.ScoreWeightThroughput = 15
	recentOK := map[int64]AccountTrafficStats{
		1: {Requests: 50, Successes: 48, Errors: 2},
		2: {Requests: 20, Successes: 19, Errors: 1},
	}
	if !costJustifiedIsolation(&pro, pool, cfg, recentOK) {
		t.Fatal("pro should justify isolation")
	}
	// Soft quarantine: p200 + weight 0, NOT disable.
	d := decision{}
	n := injectCostIsolations(&d, pool, cfg, recentOK)
	if n < 1 {
		t.Fatalf("expected soft quarantine inject, n=%d acts=%+v", n, d.Actions)
	}
	var sawPri, sawW, sawDis bool
	for _, a := range d.Actions {
		if a.AccountID != pro.ID {
			continue
		}
		switch a.Op {
		case AIOpDisable:
			sawDis = true
		case AIOpSetPriority:
			if a.Value == strconv.Itoa(AIMaxPriority) {
				sawPri = true
			}
		case AIOpSetWeight:
			if a.Value == "0" {
				sawW = true
			}
		}
	}
	if sawDis {
		t.Fatalf("cost isolation must not disable, acts=%+v", d.Actions)
	}
	if !sawPri || !sawW {
		t.Fatalf("expected p200+weight0 soft quarantine, acts=%+v", d.Actions)
	}
	// Strip model cost-disable rewrite path: disable with 性价比 reason dropped, soft inject added.
	d2 := decision{Actions: []decisionAction{
		{AccountID: pro.ID, Op: AIOpDisable, Reason: "性价比过贵 composite=0.11", Confidence: 0.9},
	}}
	_ = injectCostIsolations(&d2, pool, cfg, recentOK)
	for _, a := range d2.Actions {
		if a.Op == AIOpDisable {
			t.Fatalf("model cost-disable should be stripped, acts=%+v", d2.Actions)
		}
	}
	// Sole expensive account (no affordable peer) must not isolate.
	only := []Account{pro}
	if costJustifiedIsolation(&pro, only, cfg, recentOK) {
		t.Fatal("sole expensive account must not self-isolate")
	}

	// Over-disable cleanup: re-enable expensive without recent hard-fail.
	pool[1].AIDisabled = true
	pool[1].ScheduleWeight = 10
	d3 := decision{}
	n3 := injectCostIsolationReleases(&d3, pool, cfg, recentOK)
	if n3 < 1 || d3.Actions[0].Op != AIOpEnable {
		t.Fatalf("expected re-enable over-disable, n=%d acts=%+v", n3, d3.Actions)
	}
	// Hard-fail stays disabled.
	pool[1].AIDisabled = true
	hard := map[int64]AccountTrafficStats{
		2: {Requests: 20, Successes: 2, Errors: 20},
	}
	d4 := decision{}
	if n := injectCostIsolationReleases(&d4, pool, cfg, hard); n != 0 {
		t.Fatalf("hard-fail must stay disabled, n=%d acts=%+v", n, d4.Actions)
	}

	// 麻豆-class 0.06 vs 0.04 should soft-unbury from p200.
	madou := Account{
		ID: 3, Name: "麻豆", Priority: 200, ScheduleWeight: 5240,
		Status: StatusActive, Schedulable: true, GroupIDs: []int64{2, 5},
		Extra: map[string]any{ExtraAIRateMultiplier: 0.06, ExtraAIRateSource: "sub2api"},
	}
	pool2 := []Account{cheap, madou}
	if !softUnburyAffordableSpareRescue(&madou, pool2, AccountTrafficStats{}, AccountTrafficStats{}, nil) {
		t.Fatal("madou 0.06 must be affordable spare rescue eligible")
	}
}

func TestCostBaselineIgnoresDeadCheapPeer(t *testing.T) {
	t.Parallel()
	deadCheap := Account{
		ID: 1, Name: "梦幻", Priority: 100, ScheduleWeight: 8000,
		Status: StatusActive, Schedulable: true, AIManaged: true,
		Extra: map[string]any{ExtraAIRateMultiplier: 0.045, ExtraAIRateSource: "sub2api"},
	}
	healthyCheap := Account{
		ID: 2, Name: "saozhao", Priority: 100, ScheduleWeight: 4000,
		Status: StatusActive, Schedulable: true, AIManaged: true,
		Extra: map[string]any{ExtraAIRateMultiplier: 0.05, ExtraAIRateSource: "sub2api"},
	}
	twochat := Account{
		ID: 3, Name: "2chat", Priority: 200, ScheduleWeight: 0,
		Status: StatusActive, Schedulable: true, AIManaged: true,
		Extra: map[string]any{ExtraAIRateMultiplier: 0.08, ExtraAIRateSource: "sub2api"},
	}
	pool := []Account{deadCheap, healthyCheap, twochat}
	cfg := DefaultAIAutopilotSettings()
	cfg.ScoreWeightCost = 40
	cfg.ScoreWeightStability = 25
	cfg.ScoreWeightLatency = 20
	cfg.ScoreWeightThroughput = 15
	recent := map[int64]AccountTrafficStats{
		1: {Requests: 20, Successes: 1, Errors: 20}, // dead cheap
		2: {Requests: 50, Successes: 48, Errors: 2},
		3: {Requests: 10, Successes: 10, Errors: 0},
	}
	// vs 0.045 → 1.78× very expensive; vs healthy 0.05 → 1.60× not.
	if !isExpensiveVsPeers(&twochat, pool, AICostVeryExpensiveRatio, nil) {
		t.Fatal("without health filter 2chat looks 1.75x vs 0.045")
	}
	if isExpensiveVsPeers(&twochat, pool, AICostVeryExpensiveRatio, recent) {
		t.Fatal("healthy cheapest is 0.05; 0.08 must not be 1.75x")
	}
	if costJustifiedIsolation(&twochat, pool, cfg, recent) {
		t.Fatal("must not isolate 2chat against a dead 0.045")
	}

	d := decision{}
	n := injectCostIsolationReleases(&d, pool, cfg, recent)
	if n < 1 {
		t.Fatalf("expected w0/p200 release, n=%d acts=%+v", n, d.Actions)
	}
	var sawW, sawP bool
	for _, a := range d.Actions {
		if a.AccountID != twochat.ID {
			continue
		}
		if a.Op == AIOpSetWeight && a.Value == "10" {
			sawW = true
		}
		if a.Op == AIOpSetPriority && a.Value == strconv.Itoa(AIPriorityBuriedThreshold) {
			sawP = true
		}
	}
	if !sawW || !sawP {
		t.Fatalf("2chat should restore weight=10 and p150, acts=%+v", d.Actions)
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

func mainLayerTestAccount(id int64, name string, priority, weight int, composite float64) Account {
	rate := composite
	return Account{
		ID:             id,
		Name:           name,
		Platform:       PlatformOpenAI,
		Type:           AccountTypeAPIKey,
		Status:         StatusActive,
		Schedulable:    true,
		AIManaged:      true,
		Priority:       priority,
		ScheduleWeight: weight,
		RateMultiplier: &rate,
		Extra: map[string]any{
			ExtraAIRateMultiplier: composite,
			ExtraAIRateSource:     "newapi",
		},
	}
}

func TestInjectMainLayerCap_KeepsCheapestThree(t *testing.T) {
	t.Parallel()
	accounts := []Account{
		mainLayerTestAccount(1, "Sy", 100, 20, 0.05),
		mainLayerTestAccount(2, "麻豆", 100, 15, 0.08),
		mainLayerTestAccount(3, "哈吉米", 100, 12, 0.10),
		mainLayerTestAccount(4, "maok", 100, 10, 0.50),
		mainLayerTestAccount(5, "梦幻", 100, 10, 0.80),
		mainLayerTestAccount(6, "lyy", 100, 8, 1.00),
		mainLayerTestAccount(7, "backup", 150, 10, 0.06),
	}
	d := decision{}
	n := injectMainLayerCap(&d, accounts, nil, DefaultAIAutopilotSettings())
	if n != 3 {
		t.Fatalf("want 3 sinks, n=%d acts=%+v", n, d.Actions)
	}
	sunk := map[int64]bool{}
	for _, a := range d.Actions {
		if a.Op != AIOpSetPriority || a.Value != strconv.Itoa(AIPriorityBuriedThreshold) {
			t.Fatalf("unexpected action %+v", a)
		}
		sunk[a.AccountID] = true
	}
	for _, keep := range []int64{1, 2, 3} {
		if sunk[keep] {
			t.Fatalf("cheap keeper %d should stay on main layer, acts=%+v", keep, d.Actions)
		}
	}
	for _, extra := range []int64{4, 5, 6} {
		if !sunk[extra] {
			t.Fatalf("expensive extra %d should sink, acts=%+v", extra, d.Actions)
		}
	}
	if sunk[7] {
		t.Fatal("already-spare backup must not be touched")
	}
}

func TestInjectMainLayerCap_NoopAtCap(t *testing.T) {
	t.Parallel()
	accounts := []Account{
		mainLayerTestAccount(1, "a", 100, 10, 0.05),
		mainLayerTestAccount(2, "b", 100, 10, 0.06),
		mainLayerTestAccount(3, "c", 100, 10, 0.07),
		mainLayerTestAccount(4, "d", 150, 10, 0.08),
	}
	d := decision{}
	if n := injectMainLayerCap(&d, accounts, nil, DefaultAIAutopilotSettings()); n != 0 || len(d.Actions) != 0 {
		t.Fatalf("want noop at cap, n=%d acts=%+v", n, d.Actions)
	}
}

func TestInjectMainLayerCap_SinksHardFailFirst(t *testing.T) {
	t.Parallel()
	accounts := []Account{
		mainLayerTestAccount(1, "cheap-fail", 100, 20, 0.04),
		mainLayerTestAccount(2, "ok2", 100, 10, 0.08),
		mainLayerTestAccount(3, "ok3", 100, 10, 0.09),
		mainLayerTestAccount(4, "ok4", 100, 10, 0.10),
	}
	recent := map[int64]AccountTrafficStats{
		1: {Requests: 20, Successes: 4, Errors: 16},
	}
	d := decision{}
	n := injectMainLayerCap(&d, accounts, recent, DefaultAIAutopilotSettings())
	if n != 1 || len(d.Actions) != 1 || d.Actions[0].AccountID != 1 {
		t.Fatalf("hard-fail cheap account should be the overflow extra, n=%d acts=%+v", n, d.Actions)
	}
}

func TestInjectMainLayerCap_OverridesPendingKeep(t *testing.T) {
	t.Parallel()
	accounts := []Account{
		mainLayerTestAccount(1, "a", 100, 10, 0.05),
		mainLayerTestAccount(2, "b", 100, 10, 0.06),
		mainLayerTestAccount(3, "c", 100, 10, 0.07),
		mainLayerTestAccount(4, "d", 100, 10, 0.80),
	}
	d := decision{Actions: []decisionAction{{
		AccountID: 4, Op: AIOpSetPriority, Value: "80", Reason: "model wants 4th main",
	}}}
	n := injectMainLayerCap(&d, accounts, nil, DefaultAIAutopilotSettings())
	if n != 1 {
		t.Fatalf("want override sink, n=%d acts=%+v", n, d.Actions)
	}
	var pri4 int
	for _, a := range d.Actions {
		if a.AccountID == 4 && a.Op == AIOpSetPriority {
			pri4++
			if a.Value != strconv.Itoa(AIPriorityBuriedThreshold) {
				t.Fatalf("cap must last-write 150, got %+v", a)
			}
		}
	}
	if pri4 != 1 {
		t.Fatalf("want single priority action for extra, got %d in %+v", pri4, d.Actions)
	}
}

func TestMainLayerPromotionCapReason(t *testing.T) {
	t.Parallel()
	accounts := []Account{
		mainLayerTestAccount(1, "a", 100, 10, 0.05),
		mainLayerTestAccount(2, "b", 100, 10, 0.06),
		mainLayerTestAccount(3, "c", 100, 10, 0.07),
		mainLayerTestAccount(4, "d", 150, 10, 0.08),
	}
	if reason := mainLayerPromotionCapReason(&accounts[3], 100, accounts, nil); reason == "" {
		t.Fatal("4th promotion must be blocked when 3 mains occupy")
	}
	if reason := mainLayerPromotionCapReason(&accounts[0], 80, accounts, nil); reason != "" {
		t.Fatalf("existing main must still rebalance, got %s", reason)
	}
	// Swap: pending sink of a keeper frees a slot for the spare.
	pending := []decisionAction{{AccountID: 3, Op: AIOpSetPriority, Value: "150"}}
	if reason := mainLayerPromotionCapReason(&accounts[3], 100, accounts, pending); reason != "" {
		t.Fatalf("promotion should pass when a pending extra frees a slot: %s", reason)
	}
}

func TestMainLayerOverflowDemotion_OnlyExtras(t *testing.T) {
	t.Parallel()
	accounts := []Account{
		mainLayerTestAccount(1, "cheap", 100, 20, 0.05),
		mainLayerTestAccount(2, "mid", 100, 15, 0.08),
		mainLayerTestAccount(3, "ok", 100, 12, 0.10),
		mainLayerTestAccount(4, "pricey", 100, 10, 0.80),
	}
	if !mainLayerOverflowDemotion(&accounts[3], 150, accounts, nil) {
		t.Fatal("expensive extra 100→150 must be allowed")
	}
	if mainLayerOverflowDemotion(&accounts[0], 150, accounts, nil) {
		t.Fatal("cheap keeper must not use overflow gate")
	}
	if mainLayerOverflowDemotion(&accounts[3], 150, accounts[:3], nil) {
		t.Fatal("at cap, overflow must be off")
	}
}

func TestFilterDeathSpiral_KeepsOverflowExtraOnly(t *testing.T) {
	t.Parallel()
	accounts := []Account{
		mainLayerTestAccount(1, "cheap", 100, 20, 0.05),
		mainLayerTestAccount(2, "mid", 100, 15, 0.08),
		mainLayerTestAccount(3, "ok", 100, 12, 0.10),
		mainLayerTestAccount(4, "pricey", 100, 10, 0.80),
	}
	long := map[int64]AccountTrafficStats{
		1: {Requests: 80, Successes: 76, Errors: 4},
		4: {Requests: 80, Successes: 76, Errors: 4},
	}
	recent := map[int64]AccountTrafficStats{1: {}, 4: {}}
	cfg := DefaultAIAutopilotSettings()
	// Keeper healthy sink must still be dropped.
	out := filterDeathSpiralDemotions([]decisionAction{
		{AccountID: 1, Op: AIOpSetPriority, Value: "150", Reason: "sink keeper"},
	}, accounts, long, recent, cfg)
	if len(out) != 0 {
		t.Fatalf("keeper sink must drop, got %+v", out)
	}
	out = filterDeathSpiralDemotions([]decisionAction{
		{AccountID: 4, Op: AIOpSetPriority, Value: "150", Reason: "sink extra"},
	}, accounts, long, recent, cfg)
	if len(out) != 1 || out[0].AccountID != 4 {
		t.Fatalf("extra sink must keep, got %+v", out)
	}
}
