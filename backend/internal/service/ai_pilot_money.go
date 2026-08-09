package service

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Money / recovery observation tiers (priority: smaller = more preferred).
const (
	// Priority band mirrors UpstreamRouter guidance (≈80–120 分层, 默认 100).
	// Strict layering means a sole account at priority=1 starves everyone else.
	// AI may only set priority inside [AIMinPriority, AIMaxPriority].
	AIMinPriority = 50
	// AIObservationPriority / default rebalance tier (UpstreamRouter default 100).
	AIObservationPriority = 100
	// AIPriorityBuriedThreshold: spare/backup tier. Strict layering means p≥150
	// gets almost no traffic while p=100 is healthy — this is the "sticky 150" trap.
	// Soft unbury (with long-window health) applies at ≥ this value.
	AIPriorityBuriedThreshold = 150
	// AIMaxPriority is the deepest autopilot may set for normal demotion.
	AIMaxPriority = 200
	// AIPriorityDeltaSafety is a hard ceiling on |Δpriority| even when settings
	// are misconfigured (prod had priority_max_delta=1e9 → thrash every minute).
	AIPriorityDeltaSafety = 50
	// AIWeightHealthyFloor: do not crush healthy accounts to weight 0/1 (soft-disable ratchet).
	AIWeightHealthyFloor = 3
	// AIDemotionMinCooldown is a thrash floor when channel_cooldown_minutes > 0 but very low.
	// Explicit channel_cooldown_minutes=0 means "no cooldown" and this floor is skipped.
	AIDemotionMinCooldown = 15 * time.Minute
	// AIBalanceCacheMaxAge: soft cache for wallet/余额 soft-refresh in pilot + moneyView.
	// Operators want ~1m freshness so depleted/low-balance shows up quickly.
	AIBalanceCacheMaxAge = 1 * time.Minute
	// AIRateCacheMaxAge: soft cache for 计费倍率 (newapi/sub2api rate). Rates change
	// rarely; 5m matches the default upstream billing probe cadence and cuts probe load.
	AIRateCacheMaxAge = 5 * time.Minute
	// AISuggestionMaxAge auto-dismisses pending suggestions older than this.
	AISuggestionMaxAge = 2 * time.Hour

	// Cost-pressure gates (when admin raises 性价比 weight):
	// fraction of overall that is cost — ≥ this activates hard demotion / block-promote.
	AICostPressureFraction = 0.20
	// composite > median * ratio → expensive (block soft-unbury / promotion to main).
	AICostExpensiveRatio = 1.45
	// composite > median * ratio → very expensive (auto spare demotion allowed even if healthy).
	AICostVeryExpensiveRatio = 1.75
)

// Account extra keys for money / upstream mgmt (no schema migration).
const (
	ExtraRechargeMultiplier = "recharge_multiplier"
	ExtraUpstreamKind       = "upstream_kind" // newapi|oneapi|sub2api|manual
	ExtraUpstreamMgmtToken  = "upstream_mgmt_token"
	ExtraUpstreamMgmtUserID = "upstream_mgmt_user_id"
	ExtraAIBalanceStatus    = "ai_balance_status"
	ExtraAIBalanceUSD       = "ai_balance_usd"
	ExtraAIBalanceCheckedAt = "ai_balance_checked_at"
	ExtraAIRateMultiplier   = "ai_rate_multiplier" // last resolved group rate
	ExtraAIRateSource       = "ai_rate_source"
	ExtraAIRateCheckedAt    = "ai_rate_checked_at"
	// ExtraExcludeFromSchedule marks control-plane / LLM-only accounts (e.g. GPTX for
	// AI autopilot). They never enter normal gateway traffic scheduling; only
	// requests tagged with X-Sub2API-Client: ai-autopilot may select them.
	ExtraExcludeFromSchedule = "exclude_from_schedule"
)

// CompositeRateMultiplier returns rate/recharge when recharge>0, else rate.
// Example: rate=1, recharge=10 (1:10 充值) → composite=0.1 (10× cheaper).
func CompositeRateMultiplier(rate, recharge float64) float64 {
	if rate <= 0 {
		rate = 1
	}
	if recharge <= 0 {
		recharge = 1
	}
	return rate / recharge
}

// RateConfidence describes how much to trust composite for cost ranking.
type RateConfidence struct {
	Level             int     `json:"level"` // 1 high, 2 medium, 3 low
	Label             string  `json:"label"`
	Primary           string  `json:"primary"`
	TrustedComposite  float64 `json:"trustedComposite"`
	Note              string  `json:"note,omitempty"`
}

// BuildRateConfidence mirrors UpstreamRouter L1–L3 semantics.
// source: imported | custom | estimated | default_one | manual
func BuildRateConfidence(rate, recharge float64, source string) RateConfidence {
	composite := CompositeRateMultiplier(rate, recharge)
	src := strings.ToLower(strings.TrimSpace(source))
	rc := RateConfidence{
		Level:            3,
		Label:            "low",
		Primary:          "default_one",
		TrustedComposite: composite,
		Note:             "设定倍率为默认1且无上游导入,价格信号弱",
	}
	switch src {
	case "imported", "upstream", "billing_probe", "newapi", "oneapi", "sub2api":
		rc.Level = 1
		rc.Label = "high"
		rc.Primary = "configured_imported"
		rc.Note = "上游导入/同步的设定综合倍率(rate/recharge)置信度最高"
	case "custom", "manual":
		if rate != 1 {
			rc.Level = 2
			rc.Label = "medium"
			rc.Primary = "configured_custom"
			rc.Note = "人工非默认设定倍率,置信度中等"
		} else {
			rc.Primary = "default_one"
		}
	case "estimated":
		rc.Level = 3
		rc.Label = "low"
		rc.Primary = "estimated"
		rc.Note = "主要依赖估算倍率,置信度低"
	default:
		if rate != 1 {
			rc.Level = 2
			rc.Label = "medium"
			rc.Primary = "configured_custom"
			rc.Note = "非默认设定倍率,置信度中等"
		}
	}
	return rc
}

func accountRechargeMultiplier(acc *Account) float64 {
	if acc == nil {
		return 1
	}
	v := extraFloat(acc.Extra, ExtraRechargeMultiplier)
	if v <= 0 {
		return 1
	}
	return v
}

// resolveAccountRate returns (rate, source) for pilot money view.
// Prefer auto-imported (billing probe / cached ai_rate) over column RateMultiplier alone.
func resolveAccountRate(acc *Account) (rate float64, source string) {
	if acc == nil {
		return 1, "default_one"
	}
	// 1) Cached AI-resolved rate (new-api group_ratio etc.)
	if r := extraFloat(acc.Extra, ExtraAIRateMultiplier); r > 0 {
		src := extraString(acc.Extra, ExtraAIRateSource)
		if src == "" {
			src = "imported"
		}
		return r, src
	}
	// 2) Upstream billing probe snapshot (sub2api-style)
	if snap := parseBillingProbeRate(acc); snap > 0 {
		return snap, "billing_probe"
	}
	// 3) Account column rate_multiplier
	if acc.RateMultiplier != nil && *acc.RateMultiplier >= 0 {
		r := *acc.RateMultiplier
		if r == 0 {
			return 0, "custom"
		}
		if r == 1 {
			return 1, "default_one"
		}
		return r, "custom"
	}
	return 1, "default_one"
}

func parseBillingProbeRate(acc *Account) float64 {
	if acc == nil || acc.Extra == nil {
		return 0
	}
	raw, ok := acc.Extra[UpstreamBillingProbeExtraKey]
	if !ok || raw == nil {
		return 0
	}
	// Stored as map (sanitized snapshot) with nested data.
	m, ok := raw.(map[string]any)
	if !ok {
		return 0
	}
	// Prefer top-level synced, then data.resolved/effective.
	if v := asMapFloat(m, "synced_rate_multiplier"); v > 0 {
		return v
	}
	if data, ok := m["data"].(map[string]any); ok {
		for _, k := range []string{"resolved_rate_multiplier", "effective_rate_multiplier", "group_rate_multiplier"} {
			if v := asMapFloat(data, k); v > 0 {
				return v
			}
		}
	}
	return 0
}

func asMapFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	return anyToFloat64(m[key])
}

func extraFloat(extra map[string]any, key string) float64 {
	if extra == nil {
		return 0
	}
	return anyToFloat64(extra[key])
}

func extraString(extra map[string]any, key string) string {
	if extra == nil {
		return ""
	}
	switch v := extra[key].(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		if v == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// moneyView builds channels[].money for the snapshot.
func moneyView(acc *Account) map[string]any {
	rate, src := resolveAccountRate(acc)
	recharge := accountRechargeMultiplier(acc)
	composite := CompositeRateMultiplier(rate, recharge)
	rc := BuildRateConfidence(rate, recharge, src)
	balStatus, balUSD, balFresh := balanceViewFromAccount(acc)
	return map[string]any{
		"balanceStatus":            balStatus,
		"balanceUsd":               balUSD,
		"balanceFresh":             balFresh,
		"rateMultiplier":           rate,
		"rechargeMultiplier":       recharge,
		"compositeRateMultiplier":  composite,
		"rateSource":               src,
		"rateConfidence": map[string]any{
			"level":             rc.Level,
			"label":             rc.Label,
			"primary":           rc.Primary,
			"trustedComposite":  rc.TrustedComposite,
			"note":              rc.Note,
		},
	}
}

func balanceViewFromAccount(acc *Account) (status string, usd float64, fresh bool) {
	if acc == nil {
		return "unknown", 0, false
	}
	status = extraString(acc.Extra, ExtraAIBalanceStatus)
	if status == "" {
		status = "unknown"
	}
	usd = extraFloat(acc.Extra, ExtraAIBalanceUSD)
	if ts := extraString(acc.Extra, ExtraAIBalanceCheckedAt); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			fresh = time.Since(t) <= AIBalanceCacheMaxAge
		}
	}
	return status, usd, fresh
}

// RecoveryObservationPriority returns the priority to apply when un-burying.
// Never promotes to 1 (front); caps at AIObservationPriority when buried.
func RecoveryObservationPriority(current int) int {
	if current <= 0 {
		return AIObservationPriority
	}
	if current >= AIPriorityBuriedThreshold {
		return AIObservationPriority
	}
	return current
}

// ShouldUnburyPriority reports deep exile (beyond AIMaxPriority) that always lifts.
// Used after enable so 9000/50000 never stay permanently dark.
func ShouldUnburyPriority(current int) bool {
	return current > AIMaxPriority
}

// ShouldSoftUnburySpareTier is the sticky spare trap: p∈[150,200] under strict
// layering gets near-zero traffic, then the model treats empty recent window as
// "unhealthy" and keeps them buried. Soft-unbury when long window is healthy.
func ShouldSoftUnburySpareTier(current int) bool {
	return current >= AIPriorityBuriedThreshold && current <= AIMaxPriority
}

// longWindowHealthyEnough is true when demotion/burial is not justified by long-window failure.
func longWindowHealthyEnough(st AccountTrafficStats) bool {
	n := st.Requests + st.Errors
	if n <= 0 {
		// No long samples: do not treat as hard-fail (may be newly unburied).
		return true
	}
	sr := float64(st.Successes) / float64(n)
	if n < 10 {
		// Small sample: only healthy if not majority-failing.
		return sr >= 0.80
	}
	return sr >= 0.90
}

// recentWindowHardFail is true when recent samples clearly show breakage.
func recentWindowHardFail(st AccountTrafficStats) bool {
	n := st.Requests + st.Errors
	if n < 5 {
		return false
	}
	sr := float64(st.Successes) / float64(n)
	return sr < 0.85
}

// softUnburyEligible gates automatic 150→100 lift for the classic path.
// Requires real long-window sample health; refuses when recent is hard-failing.
//
// Empty long window (ln<10) is intentionally NOT enough here: just-demoted
// accounts often have empty recent+long under strict layering and would thrash
// 100↔150 every cycle. Cheap accounts with known low composite use
// softUnburyCheapSpareRescue instead (Sy-class sticky-spare bug).
func softUnburyEligible(long, recent AccountTrafficStats) bool {
	if recentWindowHardFail(recent) {
		return false
	}
	ln := long.Requests + long.Errors
	if ln < 10 {
		return false // no evidence → leave in spare tier (unless cheap-rescue path)
	}
	return longWindowHealthyEnough(long)
}

// isCheapOrNearCheapest is true when account has a known trusted composite within
// 25% of the cheapest other known peer (includes "次便宜" like Sy at 0.05 vs 0.04).
func isCheapOrNearCheapest(acc *Account, accounts []Account) bool {
	if acc == nil {
		return false
	}
	sig := accountCostSignalOf(acc)
	if !sig.Known || sig.Composite <= 0 {
		return false
	}
	minC, ok := cheapestKnownComposite(accounts, acc.ID)
	if !ok || minC <= 0 {
		// Sole known-rate account in pool — treat as eligible for rescue.
		return true
	}
	return sig.Composite <= minC*1.25+1e-12
}

// softUnburyCheapSpareRescue breaks the sticky-spare death spiral for known-cheap
// accounts: p≥150 gets zero traffic under strict layering → long window stays empty
// → classic softUnburyEligible never fires → model only bumps weight at p150 (no
// traffic). High 性价比 weight makes this especially wrong.
//
// Requires known near-cheapest composite; unknown-rate accounts stay on classic path
// (avoids auto-lifting mystery Pro tiers). Fresh probe fail still blocks.
func softUnburyCheapSpareRescue(acc *Account, accounts []Account, long, recent AccountTrafficStats, probes map[int64]activationResult) bool {
	if acc == nil || !isCheapOrNearCheapest(acc, accounts) {
		return false
	}
	if recentWindowHardFail(recent) {
		return false
	}
	if pr, ok := probes[acc.ID]; ok && pr.Fresh && pr.Verdict == "fail" {
		return false
	}
	ln := long.Requests + long.Errors
	if ln >= 10 && !longWindowHealthyEnough(long) {
		return false // real long-window failure still blocks
	}
	return true
}

// costPressureActive is true when admin put enough weight on 性价比 that hard cost
// gates should fire (default 10% is soft/LLM-only; ≥20% enables demotion/unbury gates).
func costPressureActive(cfg AIAutopilotSettings) bool {
	return cfg.ScoreWeights().Cost+1e-9 >= AICostPressureFraction
}

// peerKnownComposites collects known trusted composites for active schedulable accounts.
// skipID>0 excludes that account (so a pricey self does not inflate the baseline).
func peerKnownComposites(accounts []Account, skipID int64) []float64 {
	var vals []float64
	for i := range accounts {
		acc := &accounts[i]
		if skipID > 0 && acc.ID == skipID {
			continue
		}
		if acc.Status != StatusActive || !acc.Schedulable || acc.AIDisabled {
			continue
		}
		sig := accountCostSignalOf(acc)
		if !sig.Known || sig.Composite <= 0 {
			continue
		}
		vals = append(vals, sig.Composite)
	}
	return vals
}

// peerMedianKnownComposite returns median trusted composite among pool accounts
// that have a known (non-default) rate. ok=false when fewer than 1 known peer.
func peerMedianKnownComposite(accounts []Account) (float64, bool) {
	vals := peerKnownComposites(accounts, 0)
	if len(vals) == 0 {
		return 0, false
	}
	// insertion sort for tiny n
	for i := 1; i < len(vals); i++ {
		j := i
		for j > 0 && vals[j] < vals[j-1] {
			vals[j], vals[j-1] = vals[j-1], vals[j]
			j--
		}
	}
	mid := len(vals) / 2
	if len(vals)%2 == 0 {
		return (vals[mid-1] + vals[mid]) / 2, true
	}
	return vals[mid], true
}

// cheapestKnownComposite excludes skipID so we compare against better alternatives.
func cheapestKnownComposite(accounts []Account, skipID int64) (float64, bool) {
	vals := peerKnownComposites(accounts, skipID)
	if len(vals) == 0 {
		return 0, false
	}
	minC := vals[0]
	for _, c := range vals[1:] {
		if c < minC {
			minC = c
		}
	}
	return minC, true
}

// isExpensiveVsPeers reports composite > cheapest_peer * ratio.
// Baseline is the cheapest OTHER known-rate account (not pool median including self —
// otherwise a Pro at 0.11 next to 0.06 dilutes the median and never looks expensive).
// Unknown-rate accounts are never "expensive" by this metric (neutral cost score only).
func isExpensiveVsPeers(acc *Account, accounts []Account, ratio float64) bool {
	if acc == nil || ratio <= 0 {
		return false
	}
	sig := accountCostSignalOf(acc)
	if !sig.Known || sig.Composite <= 0 {
		return false
	}
	minC, ok := cheapestKnownComposite(accounts, acc.ID)
	if !ok || minC <= 0 {
		return false
	}
	return sig.Composite > minC*ratio+1e-12
}

// costJustifiedSpareDemotion: high 性价比 weight + very expensive vs peers → allow
// demoting a *healthy* account into spare tier (overrides health demotion gate).
func costJustifiedSpareDemotion(acc *Account, accounts []Account, cfg AIAutopilotSettings) bool {
	if !costPressureActive(cfg) {
		return false
	}
	return isExpensiveVsPeers(acc, accounts, AICostVeryExpensiveRatio)
}

// costBlocksMainPromotion: expensive account must not be lifted into main observation
// tier (≤100) while cheaper peers exist and cost pressure is on.
func costBlocksMainPromotion(acc *Account, accounts []Account, cfg AIAutopilotSettings) bool {
	if !costPressureActive(cfg) {
		return false
	}
	return isExpensiveVsPeers(acc, accounts, AICostExpensiveRatio)
}

// priorityCostPromotionGateReason rejects promoting expensive accounts into main tier.
// Demotion/equal is not covered here (health gates + cost-justified demotion handle that).
func priorityCostPromotionGateReason(acc *Account, next int, accounts []Account, cfg AIAutopilotSettings) string {
	if acc == nil {
		return ""
	}
	cur := acc.Priority
	// promotion = smaller priority number, into/within main band
	if next >= cur {
		return ""
	}
	if next > AIObservationPriority {
		return "" // still spare-ish; not main promotion
	}
	if !costBlocksMainPromotion(acc, accounts, cfg) {
		return ""
	}
	sig := accountCostSignalOf(acc)
	minC, _ := cheapestKnownComposite(accounts, acc.ID)
	return fmt.Sprintf(
		"性价比权重高:禁止把贵号(composite=%.3f > 最便宜peer×%.2f≈%.3f)抬回主层≤%d;请用更便宜 peer 或 set_weight",
		sig.Composite, AICostExpensiveRatio, minC*AICostExpensiveRatio, AIObservationPriority,
	)
}

// priorityDemotionGateReason rejects set_priority that re-creates the burial ratchet.
// Note: smaller priority = more preferred; demotion means next > current (e.g. 100→150).
// costJustified=true allows healthy→spare demotion when cost pressure marks the account
// very expensive (otherwise 性价比 weight can never remove a stable but pricey Pro tier).
func priorityDemotionGateReason(acc *Account, next int, long, recent AccountTrafficStats) string {
	return priorityDemotionGateReasonEx(acc, next, long, recent, false)
}

func priorityDemotionGateReasonEx(acc *Account, next int, long, recent AccountTrafficStats, costJustified bool) string {
	if acc == nil {
		return ""
	}
	cur := acc.Priority
	if next <= cur {
		return "" // not demotion (equal or promotion/unbury)
	}
	// p=1 (or any front-of-band) → into [50,200] is a safety clamp, not thrash demotion.
	if isOutOfBandPriorityClamp(cur, next) {
		return ""
	}
	// Cost-pressure spare demotion: allow healthy expensive → ≥150.
	if costJustified && next >= AIPriorityBuriedThreshold {
		return ""
	}
	// Sinking into/within spare tier (≥150) without hard failure evidence.
	if next >= AIPriorityBuriedThreshold {
		if !recentWindowHardFail(recent) && longWindowHealthyEnough(long) {
			return "禁止无硬故障证据沉到备援层(≥150):长窗健康或近窗样本不足时请用 set_weight;空白近窗是严格分层的后果不是故障"
		}
	}
	// From main observation layer, empty recent + healthy long → demotion forbidden.
	if cur <= AIObservationPriority && (recent.Requests+recent.Errors) == 0 && longWindowHealthyEnough(long) {
		if costJustified {
			return ""
		}
		return "近窗0请求且长窗健康,禁止下沉(分层后近窗空白≠不健康);用 set_weight 调同层比例"
	}
	// Soft demotion (100→120 etc.) without hard fail: require clear recent weakness.
	if next > cur && next < AIPriorityBuriedThreshold && !recentWindowHardFail(recent) && longWindowHealthyEnough(long) {
		// Allow small rebalance only if recent has enough samples and is worse — else weight only.
		rn := recent.Requests + recent.Errors
		if rn < 5 {
			return "近窗样本不足,禁止微调下沉 priority;请用 set_weight"
		}
	}
	return ""
}

// weightCrushGateReason rejects crushing healthy accounts toward weight 0/1.
// costJustified allows lowering expensive accounts to floor under cost pressure.
func weightCrushGateReason(acc *Account, next int, long, recent AccountTrafficStats) string {
	return weightCrushGateReasonEx(acc, next, long, recent, false)
}

func weightCrushGateReasonEx(acc *Account, next int, long, recent AccountTrafficStats, costJustified bool) string {
	if acc == nil {
		return ""
	}
	cur := acc.EffectiveScheduleWeight()
	if next >= cur {
		return ""
	}
	// Under cost pressure, allow expensive accounts down to healthy floor (not below).
	if costJustified && next >= AIWeightHealthyFloor {
		return ""
	}
	if next < AIWeightHealthyFloor && longWindowHealthyEnough(long) && !recentWindowHardFail(recent) {
		return fmt.Sprintf("禁止把健康号 weight 压到 <%d(软停死循环);保留最低分流或 set_priority", AIWeightHealthyFloor)
	}
	return ""
}

// disableHealthyGateReason rejects disable without hard failure (disable/enable thrash).
// Allowed even when long-window looks healthy when:
//   - wallet is depleted (balance hard gate — no reason to keep serving)
//   - recent traffic hard-fails
//   - fresh activation probe is fail (cannot recover traffic path)
func disableHealthyGateReason(acc *Account, long, recent AccountTrafficStats) string {
	return disableHealthyGateReasonEx(acc, long, recent, nil)
}

func disableHealthyGateReasonEx(acc *Account, long, recent AccountTrafficStats, probe *activationResult) string {
	if acc == nil {
		return ""
	}
	// Depleted accounts must be disableable regardless of historical SR.
	if st, _, _ := balanceViewFromAccount(acc); strings.EqualFold(st, "depleted") {
		return ""
	}
	if recentWindowHardFail(recent) {
		return ""
	}
	if probe != nil && probe.Fresh && strings.EqualFold(probe.Verdict, "fail") {
		return ""
	}
	if longWindowHealthyEnough(long) {
		return "长窗健康且近窗无硬失败,禁止 disable;请 set_weight/set_priority"
	}
	return ""
}

// isBalanceDepleted is a convenience for cooldown / apply bypasses.
func isBalanceDepleted(acc *Account) bool {
	if acc == nil {
		return false
	}
	st, _, _ := balanceViewFromAccount(acc)
	return strings.EqualFold(st, "depleted")
}

// isPriorityDemotion reports next > current (deeper tier).
func isPriorityDemotion(cur, next int) bool { return next > cur }

// isWeightCrush reports next < current.
func isWeightCrush(cur, next int) bool { return next < cur }

// isDemotionLike is true for ops that shrink traffic share (need floor cooldown).
func isDemotionLike(act decisionAction, acc *Account) bool {
	if acc == nil {
		return false
	}
	switch act.Op {
	case AIOpDisable:
		return true
	case AIOpSetPriority:
		n, err := strconv.Atoi(strings.TrimSpace(act.Value))
		if err != nil {
			return false
		}
		return isPriorityDemotion(acc.Priority, n)
	case AIOpSetWeight:
		n, err := strconv.Atoi(strings.TrimSpace(act.Value))
		if err != nil {
			return false
		}
		return isWeightCrush(acc.EffectiveScheduleWeight(), n)
	default:
		return false
	}
}

// filterDeathSpiralDemotions drops priority demotions that fail health gates so
// soft-unbury inject is not blocked by havePri on the same account in one run.
// Cost-justified spare demotions (very expensive under high 性价比 weight) are kept.
func filterDeathSpiralDemotions(
	actions []decisionAction,
	accounts []Account,
	longTraffic, recentTraffic map[int64]AccountTrafficStats,
	cfg AIAutopilotSettings,
) []decisionAction {
	if len(actions) == 0 {
		return actions
	}
	byID := map[int64]*Account{}
	for i := range accounts {
		byID[accounts[i].ID] = &accounts[i]
	}
	out := make([]decisionAction, 0, len(actions))
	for _, a := range actions {
		if a.Op != AIOpSetPriority {
			out = append(out, a)
			continue
		}
		acc := byID[a.AccountID]
		next, err := strconv.Atoi(strings.TrimSpace(a.Value))
		if acc == nil || err != nil {
			out = append(out, a)
			continue
		}
		var long, recent AccountTrafficStats
		if longTraffic != nil {
			long = longTraffic[a.AccountID]
		}
		if recentTraffic != nil {
			recent = recentTraffic[a.AccountID]
		}
		// Keep monopoly-front clamps (p=1→100) and cost-justified demotions.
		if isOutOfBandPriorityClamp(acc.Priority, next) {
			out = append(out, a)
			continue
		}
		costOK := costJustifiedSpareDemotion(acc, accounts, cfg)
		if reason := priorityDemotionGateReasonEx(acc, next, long, recent, costOK); reason != "" {
			continue // drop — inject soft-unbury may lift instead
		}
		out = append(out, a)
	}
	return out
}

// injectCostSpareDemotions sinks very expensive main-tier accounts to spare when
// 性价比 weight is high. Models often keep pricey-but-healthy Pro on priority=100;
// health-only gates then refuse demotion and soft-unbury re-lifts them.
func injectCostSpareDemotions(decision *decision, accounts []Account, cfg AIAutopilotSettings) int {
	if decision == nil || !costPressureActive(cfg) || !cfg.OpAllowed(AIOpSetPriority) {
		return 0
	}
	havePri := map[int64]bool{}
	for _, a := range decision.Actions {
		if a.Op == AIOpSetPriority {
			havePri[a.AccountID] = true
		}
	}
	injected := 0
	for i := range accounts {
		acc := &accounts[i]
		if acc.AIDisabled || acc.Status != StatusActive || !acc.Schedulable {
			continue
		}
		if acc.Priority > AIObservationPriority {
			continue // already spare or deeper
		}
		if havePri[acc.ID] {
			continue
		}
		if !costJustifiedSpareDemotion(acc, accounts, cfg) {
			continue
		}
		// Keep at least one known-cheap main-tier peer before sinking.
		if countKnownCheapMainTier(accounts, acc.ID) < 1 {
			continue
		}
		sig := accountCostSignalOf(acc)
		minC, _ := cheapestKnownComposite(accounts, acc.ID)
		decision.Actions = append(decision.Actions, decisionAction{
			AccountID: acc.ID,
			Op:        AIOpSetPriority,
			Value:     strconv.Itoa(AIPriorityBuriedThreshold),
			Reason: fmt.Sprintf(
				"性价比硬门禁: composite=%.3f > 最便宜peer×%.2f(最便宜=%.3f),从主层 priority=%d 沉到备援 %d;健康但过贵",
				sig.Composite, AICostVeryExpensiveRatio, minC, acc.Priority, AIPriorityBuriedThreshold,
			),
			Confidence: 0.9,
		})
		havePri[acc.ID] = true
		injected++
	}
	return injected
}

// countKnownCheapMainTier counts active main-tier accounts that are not expensive
// vs the cheapest peer (excluding skipID). Used so we never sink the last cheap option.
func countKnownCheapMainTier(accounts []Account, skipID int64) int {
	n := 0
	for i := range accounts {
		acc := &accounts[i]
		if acc.ID == skipID || acc.AIDisabled || acc.Status != StatusActive || !acc.Schedulable {
			continue
		}
		if acc.Priority > AIObservationPriority {
			continue
		}
		sig := accountCostSignalOf(acc)
		if !sig.Known {
			continue
		}
		// not expensive at the soft threshold
		if !isExpensiveVsPeers(acc, accounts, AICostExpensiveRatio) {
			n++
		}
	}
	return n
}

// balanceGateReason rejects enable when balance is known depleted.
func balanceGateReason(op string, acc *Account) string {
	if op != AIOpEnable && op != AIOpRelease && op != AIOpUnlock {
		return ""
	}
	if acc == nil {
		return ""
	}
	st, _, _ := balanceViewFromAccount(acc)
	if strings.EqualFold(st, "depleted") {
		return "余额耗尽(depleted),拒绝恢复流量类动作"
	}
	return ""
}

// rpmLoosenGateReason rejects unbounded RPM widen without evidence.
// cur=current base rpm (0=unlimited), next=proposed.
const loosenStepMax = 4

func rpmLoosenGateReason(cur, next int, hasRecent429 bool, healthy bool) string {
	// tighten or no-op always ok here
	if next == cur {
		return ""
	}
	// 0 means unlimited
	if cur == 0 && next > 0 {
		// setting a limit from unlimited is tighten — ok
		return ""
	}
	if next > 0 && cur > 0 && next < cur {
		// tighten
		return ""
	}
	// loosen path
	if !healthy {
		return "渠道不健康,拒绝放宽 RPM"
	}
	if hasRecent429 {
		return "近窗仍有 429 证据,拒绝放宽 RPM"
	}
	if cur > 0 && next > cur*loosenStepMax {
		return fmt.Sprintf("单轮放宽不超过 %d 倍(cur=%d next=%d)", loosenStepMax, cur, next)
	}
	// to unlimited (0) only when already not binding — conservative: allow if no 429 and healthy
	if next == 0 && cur > 0 {
		// allowed when healthy && !429 (checked above)
		return ""
	}
	return ""
}

func parseIntValue(s string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(s))
}
