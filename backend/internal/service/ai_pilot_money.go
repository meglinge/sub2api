package service

import (
	"fmt"
	"sort"
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
	// AIMainLayerMaxAccounts was a hard p≤100 cap. Disabled: it blocked
	// failover when mains blew up and made the pool unusable.
	AIMainLayerMaxAccounts = 3
	AIMainLayerCapEnabled  = false
	// AIPriorityDeltaSafety is a hard ceiling on |Δpriority| even when settings
	// are misconfigured (prod had priority_max_delta=1e9 → thrash every minute).
	AIPriorityDeltaSafety = 50
	// AIWeightHealthyFloor: do not crush healthy accounts to weight 0/1 (soft-disable ratchet).
	AIWeightHealthyFloor = 3
	// AIDemotionMinCooldown is a thrash floor when channel_cooldown_minutes > 0 but very low.
	// Explicit channel_cooldown_minutes=0 means "no cooldown" and this floor is skipped.
	AIDemotionMinCooldown = 15 * time.Minute
	// AISoftUnburyDwell blocks soft spare unbury (150→100) for this long after an
	// applied demotion into the spare tier. Without it, channel_cooldown=0 lets
	// soft-unbury re-lift 429-demoted accounts next cycle (100↔150 thrash).
	// 5m ≈ a few 1m pilot ticks — enough to confirm hard-fail, not half-hour parking.
	// Deep-exile unbury (>200) is unaffected.
	AISoftUnburyDwell = 5 * time.Minute
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
	// composite > cheapest_peer * ratio → expensive (block soft-unbury / promotion to main).
	AICostExpensiveRatio = 1.45
	// composite > cheapest_peer * ratio → very expensive (auto spare demotion + cost isolation).
	// Soft OpenAI scoring still sends overflow/sticky traffic to p150, so spare alone
	// does not stop burn — isolation uses ai_disabled when cheaper peers cover groups.
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
	Level            int     `json:"level"` // 1 high, 2 medium, 3 low
	Label            string  `json:"label"`
	Primary          string  `json:"primary"`
	TrustedComposite float64 `json:"trustedComposite"`
	Note             string  `json:"note,omitempty"`
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
	case "imported", "upstream", "billing_probe", "newapi", "oneapi", "sub2api", "sub2api_usage":
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
// Prefer the *newer* of cached ai_rate vs official billing-probe snapshot;
// a 10-day-old extra.ai_rate_multiplier must not beat a probe from this morning
// (麻豆传媒 0.06 cache vs 0.065 probe). Column RateMultiplier is last resort.
func resolveAccountRate(acc *Account) (rate float64, source string) {
	if acc == nil {
		return 1, "default_one"
	}
	aiRate, aiSrc, aiAt := extraAIRateSignal(acc)
	probeRate, probeAt := billingProbeRateSignal(acc)
	if aiRate > 0 && probeRate > 0 {
		if !probeAt.IsZero() && (aiAt.IsZero() || probeAt.After(aiAt)) {
			return probeRate, "billing_probe"
		}
		return aiRate, aiSrc
	}
	if probeRate > 0 {
		return probeRate, "billing_probe"
	}
	if aiRate > 0 {
		return aiRate, aiSrc
	}
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

func extraAIRateSignal(acc *Account) (rate float64, source string, at time.Time) {
	if acc == nil {
		return 0, "", time.Time{}
	}
	rate = extraFloat(acc.Extra, ExtraAIRateMultiplier)
	if rate <= 0 {
		return 0, "", time.Time{}
	}
	source = extraString(acc.Extra, ExtraAIRateSource)
	if source == "" {
		source = "imported"
	}
	at = parseExtraTime(acc.Extra[ExtraAIRateCheckedAt])
	return rate, source, at
}

func billingProbeRateSignal(acc *Account) (rate float64, at time.Time) {
	rate = parseBillingProbeRate(acc)
	if rate <= 0 {
		return 0, time.Time{}
	}
	if acc == nil || acc.Extra == nil {
		return rate, time.Time{}
	}
	raw, _ := acc.Extra[UpstreamBillingProbeExtraKey].(map[string]any)
	if raw == nil {
		return rate, time.Time{}
	}
	if v, ok := raw["received_at"].(time.Time); ok {
		at = v
	} else {
		at = parseExtraTime(raw["received_at"])
	}
	return rate, at
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

// shouldRefreshSub2APIBilling is true when a stale rate should be re-probed via
// GET /v1/sub2api/billing. Gated on account *kind*, never on the last source
// label — manual_rollback / sub2api_group_switch used to skip forever.
func shouldRefreshSub2APIBilling(kind string, alreadyGotLive bool) bool {
	if alreadyGotLive {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "newapi", "oneapi":
		return false
	default:
		return true
	}
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
		"balanceStatus":           balStatus,
		"balanceUsd":              balUSD,
		"balanceFresh":            balFresh,
		"rateMultiplier":          rate,
		"rechargeMultiplier":      recharge,
		"compositeRateMultiplier": composite,
		"rateSource":              src,
		"rateConfidence": map[string]any{
			"level":            rc.Level,
			"label":            rc.Label,
			"primary":          rc.Primary,
			"trustedComposite": rc.TrustedComposite,
			"note":             rc.Note,
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

// recentWindowHardFail is true when the recent window is a live outage:
// failover-only (no usage_log successes, recovered 5xx) or majority-failing.
// After recovered 5xx count as Errors, healthy relays sit at 70–90% SR for
// a few minutes; SR<85% n≥5 used to yo-yo them 100↔150. Stability score
// still counts those 5xx. This is slightly looser than disable (3–9 error
// storms with 0 successes bury/block unbury but do not disable).
func recentWindowHardFail(st AccountTrafficStats) bool {
	if st.Requests == 0 && st.Errors >= 3 {
		return true
	}
	return recentWindowDisableWorthy(st)
}

// recentWindowDisableWorthy is true only when the recent window is actually dead.
// Prod LLM relays routinely sit at 70–90% for a few minutes (502/503/504/timeout).
// Treating SR<85% over 3 minutes as "hard fail" mass-disabled healthy cheap accounts
// (CoCo/kedaya ~97% over 3h) that still look "active/schedulable" in the UI.
func recentWindowDisableWorthy(st AccountTrafficStats) bool {
	n := st.Requests + st.Errors
	if n < 10 {
		return false
	}
	sr := float64(st.Successes) / float64(n)
	return sr < 0.50
}

// longWindowDisableWorthy is a 60-minute corpse: enough samples and SR still
// below half. CoCo-class relays sat at long SR 20% n=41 / 168 errors while
// the 3-minute recent window had n<10 after a disable, so
// recentWindowDisableWorthy was false and disableHealthyGate rejected the
// stop — then injectRecoveryEnables revived them on a slow ping.
func longWindowDisableWorthy(st AccountTrafficStats) bool {
	n := st.Requests + st.Errors
	if n < 20 {
		return false
	}
	sr := float64(st.Successes) / float64(n)
	return sr < 0.50
}

// accountLooksDead is the shared "do not put this back on the main path"
// signal: live outage, recent majority-fail, or a clearly dead long window.
func accountLooksDead(long, recent AccountTrafficStats) bool {
	return recentWindowHardFail(recent) || recentWindowDisableWorthy(recent) || longWindowDisableWorthy(long)
}

// softUnburyEligible gates automatic 150→100 lift for the classic path.
// Requires real long-window sample health; refuses when recent is hard-failing.
//
// Empty long window (ln<10) is intentionally NOT enough here: just-demoted
// accounts often have empty recent+long under strict layering and would thrash
// 100↔150 every cycle. Cheap accounts with known low composite use
// softUnburyCheapSpareRescue instead (Sy-class sticky-spare bug).
//
// Callers must also enforce softUnburyDwell (recent spare demotion) — eligibility
// alone is insufficient when recent window goes empty right after demotion.
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

// softUnburyInDwell is true when a spare-tier demotion was applied recently enough
// that soft unbury (or LLM 150→100) should wait.
func softUnburyInDwell(lastSpareDemotion time.Time, now time.Time) bool {
	if lastSpareDemotion.IsZero() {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	return now.Sub(lastSpareDemotion) < AISoftUnburyDwell
}

// softUnburyDwellGateReason rejects spare→main priority lifts during demotion dwell.
// Deep exile unbury (priority>200) is not gated here.
func softUnburyDwellGateReason(current, next int, lastSpareDemotion time.Time, now time.Time) string {
	if !ShouldSoftUnburySpareTier(current) {
		return ""
	}
	// Soft unbury / re-promote into main observation band only.
	if next >= current || next > AIObservationPriority {
		return ""
	}
	if !softUnburyInDwell(lastSpareDemotion, now) {
		return ""
	}
	if now.IsZero() {
		now = time.Now()
	}
	remain := AISoftUnburyDwell - now.Sub(lastSpareDemotion)
	if remain < 0 {
		remain = 0
	}
	return fmt.Sprintf(
		"刚下沉到备援,%.0fm 内不立刻拉回主层(防刚因429/硬失败又拉回);剩余约%.0fm",
		AISoftUnburyDwell.Minutes(), remain.Minutes()+0.5,
	)
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
	minC, ok := cheapestKnownComposite(accounts, acc.ID, nil)
	if !ok || minC <= 0 {
		// Sole known-rate account in pool — treat as eligible for rescue.
		return true
	}
	return sig.Composite <= minC*1.25+1e-12
}

// softUnburyCheapSpareRescue breaks the sticky-spare death spiral for known-cheap
// accounts: when main tier is healthy, soft scoring still prefers lower priority
// but spare-only numbers can starve for samples → classic softUnburyEligible never
// fires → model only bumps weight at p150. High 性价比 weight makes this wrong.
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
	if spareRescueDeadWindow(long, recent) {
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

// spareRescueDeadWindow is a 502-starved spare: zero successes and several
// errors. Empty 0/0 windows (Sy-class) must still be rescuable; lyy/梦幻
// 0/8 must not be lifted to p100 just because they are cheap.
func spareRescueDeadWindow(long, recent AccountTrafficStats) bool {
	dead := func(st AccountTrafficStats) bool {
		succ, n := trafficSuccessCount(st)
		return succ == 0 && st.Errors >= 3 && n >= 3
	}
	return dead(recent) || dead(long)
}

// costPressureActive is true when admin put enough weight on 性价比 that hard cost
// gates should fire (default 10% is soft/LLM-only; ≥20% enables demotion/unbury gates).
func costPressureActive(cfg AIAutopilotSettings) bool {
	return cfg.ScoreWeights().Cost+1e-9 >= AICostPressureFraction
}

// peerKnownComposites collects known trusted composites for active schedulable accounts.
// skipID>0 excludes that account (so a pricey self does not inflate the baseline).
// peerUsableCostBaseline is a peer that can actually take cheap traffic right now.
// Disabled / w=0 / temp-unsched / recent hard-fail names must not set the "cheapest"
// bar — otherwise a dead 0.045 (梦幻) brands a healthy 0.08 (2chat) as 究极备用.
func peerUsableCostBaseline(acc *Account, recent map[int64]AccountTrafficStats) bool {
	if acc == nil || acc.Status != StatusActive || !acc.Schedulable || acc.AIDisabled {
		return false
	}
	if acc.IsExcludedFromSchedule() || acc.IsSoftWeightStopped() {
		return false
	}
	if acc.IsRateLimited() || acc.IsOverloaded() {
		return false
	}
	if acc.TempUnschedulableUntil != nil && time.Now().Before(*acc.TempUnschedulableUntil) {
		return false
	}
	if recent != nil {
		st := recent[acc.ID]
		// Empty recent = not actually covering cheap traffic this window
		// (just-enabled 梦幻 0.045 would otherwise re-brand 2chat as 1.75×).
		if st.Requests+st.Errors < 5 || recentWindowHardFail(st) {
			return false
		}
	}
	return true
}

func peerKnownComposites(accounts []Account, skipID int64, recent map[int64]AccountTrafficStats) []float64 {
	var vals []float64
	for i := range accounts {
		acc := &accounts[i]
		if skipID > 0 && acc.ID == skipID {
			continue
		}
		if !peerUsableCostBaseline(acc, recent) {
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
	vals := peerKnownComposites(accounts, 0, nil)
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
func cheapestKnownComposite(accounts []Account, skipID int64, recent map[int64]AccountTrafficStats) (float64, bool) {
	c1, _, n := cheapestKnownComposites(accounts, skipID, recent)
	return c1, n > 0
}

// cheapestKnownComposites returns (cheapest, secondCheapest, count).
// Isolation (1.75×) uses the second cheapest when it exists so a single
// ultra-cheap outlier (梦幻 0.045) cannot brand a normal 0.08 中转 as 究极备用.
func cheapestKnownComposites(accounts []Account, skipID int64, recent map[int64]AccountTrafficStats) (c1, c2 float64, n int) {
	vals := peerKnownComposites(accounts, skipID, recent)
	if len(vals) == 0 {
		return 0, 0, 0
	}
	c1 = vals[0]
	c2 = 0
	for _, c := range vals[1:] {
		if c < c1 {
			c2 = c1
			c1 = c
		} else if c2 == 0 || c < c2 {
			c2 = c
		}
	}
	return c1, c2, len(vals)
}

// isExpensiveVsPeers reports composite > cheapest_peer * ratio.
// Baseline is the cheapest OTHER known-rate account (not pool median including self —
// otherwise a Pro at 0.11 next to 0.06 dilutes the median and never looks expensive).
// Unknown-rate accounts are never "expensive" by this metric (neutral cost score only).
func isExpensiveVsPeers(acc *Account, accounts []Account, ratio float64, recent map[int64]AccountTrafficStats) bool {
	if acc == nil || ratio <= 0 {
		return false
	}
	sig := accountCostSignalOf(acc)
	if !sig.Known || sig.Composite <= 0 {
		return false
	}
	c1, c2, n := cheapestKnownComposites(accounts, acc.ID, recent)
	if n == 0 || c1 <= 0 {
		return false
	}
	bar := c1
	// Very-expensive (究极备用) needs two cheaper healthy peers; one 0.045
	// outlier must not lock every 0.08 account at weight=0.
	if ratio >= AICostVeryExpensiveRatio-1e-12 && n >= 2 && c2 > 0 {
		bar = c2
	}
	return sig.Composite > bar*ratio+1e-12
}

// costJustifiedSpareDemotion: high 性价比 weight + very expensive vs peers → allow
// demoting a *healthy* account into spare tier (overrides health demotion gate).
func costJustifiedSpareDemotion(acc *Account, accounts []Account, cfg AIAutopilotSettings, recent map[int64]AccountTrafficStats) bool {
	if !costPressureActive(cfg) {
		return false
	}
	return isExpensiveVsPeers(acc, accounts, AICostVeryExpensiveRatio, recent)
}

// costBlocksMainPromotion: expensive account must not be lifted into main observation
// tier (≤100) while cheaper peers exist and cost pressure is on.
func costBlocksMainPromotion(acc *Account, accounts []Account, cfg AIAutopilotSettings, recent map[int64]AccountTrafficStats) bool {
	if !costPressureActive(cfg) {
		return false
	}
	return isExpensiveVsPeers(acc, accounts, AICostExpensiveRatio, recent)
}

// priorityCostPromotionGateReason rejects promoting expensive accounts into main tier.
// Demotion/equal is not covered here (health gates + cost-justified demotion handle that).
func priorityCostPromotionGateReason(acc *Account, next int, accounts []Account, cfg AIAutopilotSettings, recent map[int64]AccountTrafficStats) string {
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
	if !costBlocksMainPromotion(acc, accounts, cfg, recent) {
		return ""
	}
	sig := accountCostSignalOf(acc)
	minC, _ := cheapestKnownComposite(accounts, acc.ID, recent)
	return fmt.Sprintf(
		"性价比权重高:禁止把贵号(composite=%.3f > 最便宜peer×%.2f≈%.3f)抬回主层≤%d;请用更便宜 peer 或 set_weight",
		sig.Composite, AICostExpensiveRatio, minC*AICostExpensiveRatio, AIObservationPriority,
	)
}

// weightCostLiftGateReason blocks raising schedule_weight while the account is
// still cost-justified soft quarantine (ultimate spare). Weight 0 must stick
// until cheaper peers boom / isolation no longer applies.
func weightCostLiftGateReason(acc *Account, next int, accounts []Account, cfg AIAutopilotSettings, recent map[int64]AccountTrafficStats) string {
	if acc == nil || next <= costSoftQuarantineWeight {
		return ""
	}
	// Only care when leaving soft stop or raising while still very expensive.
	if acc.EffectiveScheduleWeight() > costSoftQuarantineWeight && next <= acc.EffectiveScheduleWeight() {
		return ""
	}
	if !costJustifiedIsolation(acc, accounts, cfg, recent) {
		return ""
	}
	sig := accountCostSignalOf(acc)
	minC, _ := cheapestKnownComposite(accounts, acc.ID, recent)
	return fmt.Sprintf(
		"性价比软隔离中:禁止抬权 weight→%d (composite=%.3f > 最便宜peer×%.2f≈%.3f);贵号保持 weight=0 作究极备用,等便宜号挂完再顶",
		next, sig.Composite, AICostVeryExpensiveRatio, minC*AICostVeryExpensiveRatio,
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
	// Under cost pressure, allow expensive accounts to weight 0 (soft quarantine).
	// Non-cost path still cannot crush healthy accounts below AIWeightHealthyFloor.
	if costJustified {
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
//   - recent traffic is majority-failing (recentWindowDisableWorthy)
//   - fresh activation probe is a fatal fail (auth/quota) — NOT 502/503/timeout
//
// costJustified no longer opens disable: expensive accounts use p200+weight=0 spare.
func disableHealthyGateReason(acc *Account, long, recent AccountTrafficStats) string {
	return disableHealthyGateReasonEx(acc, long, recent, nil)
}

func disableHealthyGateReasonEx(acc *Account, long, recent AccountTrafficStats, probe *activationResult) string {
	return disableHealthyGateReasonEx2(acc, long, recent, probe, false)
}

func disableHealthyGateReasonEx2(acc *Account, long, recent AccountTrafficStats, probe *activationResult, costJustified bool) string {
	if acc == nil {
		return ""
	}
	_ = costJustified // kept in signature; isolation is soft (weight=0), not disable
	// Depleted accounts must be disableable regardless of historical SR.
	if st, _, _ := balanceViewFromAccount(acc); strings.EqualFold(st, "depleted") {
		return ""
	}
	if recentWindowDisableWorthy(recent) || longWindowDisableWorthy(long) {
		return ""
	}
	// Only fatal probe (401/403/quota). Transient 502/503/timeout is "slow", not fail.
	if probe != nil && probe.Fresh && strings.EqualFold(probe.Verdict, "fail") && probeErrorIsFatal(probe.Error) {
		return ""
	}
	if longWindowHealthyEnough(long) {
		return "长窗健康且近窗未过半失败,禁止 disable;瞬时 502/503/超时请 set_weight/set_priority"
	}
	// Blank / mediocre long window + no majority-fail recent is not a dead account.
	return "近窗未过半失败且无长窗硬证据,禁止 disable;探测 502/503/超时不等于账号坏了"
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
		costOK := costJustifiedSpareDemotion(acc, accounts, cfg, recentTraffic) ||
			mainLayerOverflowDemotion(acc, next, accounts, recentTraffic, actions) ||
			isCachePriorityBandReason(a.Reason)
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
// Note: spare is not isolation — see injectCostIsolations (ai_disabled).
func injectCostSpareDemotions(decision *decision, accounts []Account, cfg AIAutopilotSettings, recent map[int64]AccountTrafficStats) int {
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
		if !costJustifiedSpareDemotion(acc, accounts, cfg, recent) {
			continue
		}
		// Keep at least one known-cheap main-tier peer before sinking.
		if countKnownCheapMainTier(accounts, acc.ID, recent) < 1 {
			continue
		}
		sig := accountCostSignalOf(acc)
		minC, _ := cheapestKnownComposite(accounts, acc.ID, recent)
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

func occupyingMainLayer(acc *Account, priority int) bool {
	if acc == nil || acc.Status != StatusActive || !acc.Schedulable || acc.AIDisabled {
		return false
	}
	if acc.IsExcludedFromSchedule() || acc.EffectiveScheduleWeight() <= 0 {
		return false
	}
	return priority <= AIObservationPriority
}

func effectivePriority(acc *Account, pending map[int64]int) int {
	if acc == nil {
		return AIMaxPriority
	}
	if p, ok := pending[acc.ID]; ok {
		return p
	}
	return acc.Priority
}

func pendingPriorityMap(actions []decisionAction) map[int64]int {
	out := map[int64]int{}
	for _, a := range actions {
		if a.Op != AIOpSetPriority {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(a.Value))
		if err != nil {
			continue
		}
		out[a.AccountID] = n
	}
	return out
}

func mainLayerKeepScore(acc *Account, recent AccountTrafficStats) float64 {
	if acc == nil {
		return 0
	}
	score := 0.0
	sig := accountCostSignalOf(acc)
	if sig.Known && sig.Composite > 0 {
		score += 40.0 / sig.Composite
	} else {
		score += 15
	}
	if recentWindowHardFail(recent) {
		// Hard-fail mains must rank below every healthy candidate, even cheaper ones.
		return score - 1e6
	}
	n := recent.Requests + recent.Errors
	if n == 0 {
		// Leftover huge schedule_weight used to keep idle Sy/maok in the
		// main 3 while 鲨鱼/saozhao at p150 did all the real traffic.
		score -= 120
	} else {
		sr := float64(recent.Successes) / float64(n)
		score += sr * 40
		if occupyingMainLayer(acc, acc.Priority) {
			score += 80 // incumbent stickiness only while actually serving
		}
	}
	if stickyTransientlyUnavailable(acc, "") {
		score -= 50
	}
	return score
}

type mainLayerCand struct {
	acc   *Account
	score float64
}

func rankedMainLayer(accounts []Account, recent map[int64]AccountTrafficStats, pending map[int64]int) []mainLayerCand {
	var mains []mainLayerCand
	for i := range accounts {
		acc := &accounts[i]
		if !occupyingMainLayer(acc, effectivePriority(acc, pending)) {
			continue
		}
		rst := AccountTrafficStats{}
		if recent != nil {
			rst = recent[acc.ID]
		}
		mains = append(mains, mainLayerCand{acc: acc, score: mainLayerKeepScore(acc, rst)})
	}
	sort.SliceStable(mains, func(i, j int) bool {
		if mains[i].score == mains[j].score {
			return mains[i].acc.ID < mains[j].acc.ID
		}
		return mains[i].score > mains[j].score
	})
	return mains
}

// mainLayerOverflowDemotion allows a healthy 100→150 only for overflow extras
// (not the top-3 keepers). Otherwise the model could sink a cheap keeper while
// inject also sinks extras and the main layer undershoots.
func mainLayerOverflowDemotion(acc *Account, next int, accounts []Account, recent map[int64]AccountTrafficStats, actions []decisionAction) bool {
	if !AIMainLayerCapEnabled {
		return false
	}
	if acc == nil || next < AIPriorityBuriedThreshold || acc.Priority > AIObservationPriority {
		return false
	}
	pending := pendingPriorityMap(actions)
	delete(pending, acc.ID) // judge this sink against others, not after applying it
	ranked := rankedMainLayer(accounts, recent, pending)
	if len(ranked) <= AIMainLayerMaxAccounts {
		return false
	}
	for _, extra := range ranked[AIMainLayerMaxAccounts:] {
		if extra.acc.ID == acc.ID {
			return true
		}
	}
	return false
}

func mainLayerPromotionCapReason(acc *Account, next int, accounts []Account, actions []decisionAction) string {
	if !AIMainLayerCapEnabled {
		return ""
	}
	if acc == nil || next > AIObservationPriority {
		return ""
	}
	if occupyingMainLayer(acc, acc.Priority) {
		return ""
	}
	pending := pendingPriorityMap(actions)
	n := 0
	for i := range accounts {
		o := &accounts[i]
		if o.ID == acc.ID {
			continue
		}
		if occupyingMainLayer(o, effectivePriority(o, pending)) {
			n++
		}
	}
	if n >= AIMainLayerMaxAccounts {
		return fmt.Sprintf("主层已满%d个(p≤%d),禁止再抬入;先把最差主力沉到%d或只调 weight", AIMainLayerMaxAccounts, AIObservationPriority, AIPriorityBuriedThreshold)
	}
	return ""
}

func replaceOrAppendPriority(decision *decision, act decisionAction) {
	if decision == nil {
		return
	}
	out := make([]decisionAction, 0, len(decision.Actions)+1)
	for _, a := range decision.Actions {
		if a.Op == AIOpSetPriority && a.AccountID == act.AccountID {
			continue
		}
		out = append(out, a)
	}
	decision.Actions = append(out, act)
}

// injectMainLayerCap sinks extra p≤100 accounts to 150 so at most 3 mains remain.
// Sticky + too many 中转号池 mains is what bounces Codex sessions and dumps cache to ~3840.
func injectMainLayerCap(decision *decision, accounts []Account, recent map[int64]AccountTrafficStats, cfg AIAutopilotSettings) int {
	if !AIMainLayerCapEnabled {
		return 0
	}
	if decision == nil || !cfg.OpAllowed(AIOpSetPriority) {
		return 0
	}
	pending := pendingPriorityMap(decision.Actions)
	mains := rankedMainLayer(accounts, recent, pending)
	serving := 0
	for _, c := range mains {
		rst := AccountTrafficStats{}
		if recent != nil {
			rst = recent[c.acc.ID]
		}
		if rst.Requests+rst.Errors > 0 && !recentWindowHardFail(rst) {
			serving++
		}
	}
	seen := map[int64]bool{}
	var sink []mainLayerCand
	push := func(c mainLayerCand) {
		if c.acc == nil || seen[c.acc.ID] || !c.acc.AIManaged {
			return
		}
		if pending[c.acc.ID] >= AIPriorityBuriedThreshold {
			return
		}
		seen[c.acc.ID] = true
		sink = append(sink, c)
	}
	if len(mains) > AIMainLayerMaxAccounts {
		for _, extra := range mains[AIMainLayerMaxAccounts:] {
			push(extra)
		}
	}
	// Idle p100 with leftover huge weight must not occupy a main slot while
	// someone else is actually serving (Sy 0 req vs 梦幻).
	if serving >= 1 {
		for _, c := range mains {
			rst := AccountTrafficStats{}
			if recent != nil {
				rst = recent[c.acc.ID]
			}
			if rst.Requests+rst.Errors == 0 {
				push(c)
			}
		}
	}
	if keep := len(mains) - len(sink); keep < 1 && len(mains) > 0 && len(sink) > 0 {
		sink = sink[:len(sink)-1]
	}
	injected := 0
	for _, extra := range sink {
		replaceOrAppendPriority(decision, decisionAction{
			AccountID: extra.acc.ID,
			Op:        AIOpSetPriority,
			Value:     strconv.Itoa(AIPriorityBuriedThreshold),
			Reason: fmt.Sprintf(
				"主层最多%d个以降低跨号池掉缓存: %s score=%.1f 从 p%d 沉到备援 %d",
				AIMainLayerMaxAccounts, extra.acc.Name, extra.score, extra.acc.Priority, AIPriorityBuriedThreshold,
			),
			Confidence: 0.93,
		})
		pending[extra.acc.ID] = AIPriorityBuriedThreshold
		injected++
	}
	return injected
}

// peerAffordableKnown is true when o has a known rate and is not soft-expensive vs pool.
func peerAffordableKnown(o, self *Account, accounts []Account, recent map[int64]AccountTrafficStats) bool {
	if o == nil || self == nil || o.ID == self.ID {
		return false
	}
	if o.AIDisabled || o.Status != StatusActive || !o.Schedulable {
		return false
	}
	sig := accountCostSignalOf(o)
	if !sig.Known || sig.Composite <= 0 {
		return false
	}
	return !isExpensiveVsPeers(o, accounts, AICostExpensiveRatio, recent)
}

// peerHealthyEnoughForCostCover: affordable peer that can actually take traffic right now.
// Temp-unsched / recent hard-fail peers do NOT count — otherwise cost isolation stays
// locked while "cheap" names exist only as dead capacity (user: 便宜全 boom 怎么办).
func peerHealthyEnoughForCostCover(o *Account, recent AccountTrafficStats, now time.Time) bool {
	if o == nil {
		return false
	}
	if o.TempUnschedulableUntil != nil && now.Before(*o.TempUnschedulableUntil) {
		return false
	}
	if o.IsRateLimited() || o.IsOverloaded() {
		return false
	}
	if recentWindowHardFail(recent) {
		return false
	}
	return true
}

// hasAffordablePeerCoveringGroups: every group of acc has another known non-expensive peer.
// recent may be nil (treat as no hard-fail signal). requireHealthy gates boom recovery.
func hasAffordablePeerCoveringGroups(acc *Account, accounts []Account, recent map[int64]AccountTrafficStats, requireHealthy bool) bool {
	if acc == nil {
		return false
	}
	now := time.Now()
	checkPeer := func(o *Account) bool {
		if !peerAffordableKnown(o, acc, accounts, recent) {
			return false
		}
		if !requireHealthy {
			return true
		}
		var st AccountTrafficStats
		if recent != nil {
			st = recent[o.ID]
		}
		return peerHealthyEnoughForCostCover(o, st, now)
	}
	groups := acc.GroupIDs
	if len(groups) == 0 {
		for i := range accounts {
			if checkPeer(&accounts[i]) {
				return true
			}
		}
		return false
	}
	for _, gid := range groups {
		found := false
		for i := range accounts {
			o := &accounts[i]
			if !checkPeer(o) {
				continue
			}
			for _, g := range o.GroupIDs {
				if g == gid {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// costJustifiedIsolation: very expensive + healthy cheaper peers cover groups.
// Soft scheduler still feeds p150/sticky; ai_disabled stops burn. If cheap peers boom,
// isolation must not stick — see injectCostIsolationReleases / costEnableGateReason.
func costJustifiedIsolation(acc *Account, accounts []Account, cfg AIAutopilotSettings, recent map[int64]AccountTrafficStats) bool {
	if acc == nil || !costPressureActive(cfg) {
		return false
	}
	if !isExpensiveVsPeers(acc, accounts, AICostVeryExpensiveRatio, recent) {
		return false
	}
	return hasAffordablePeerCoveringGroups(acc, accounts, recent, true)
}

// costEnableGateReason blocks enable only while healthy cheaper peers still cover groups.
// When all affordable peers hard-fail / temp-unsched, enable is allowed (boom fallback).
func costEnableGateReason(op string, acc *Account, accounts []Account, cfg AIAutopilotSettings, recent map[int64]AccountTrafficStats) string {
	switch op {
	case AIOpEnable, AIOpRelease, AIOpUnlock:
	default:
		return ""
	}
	if acc == nil || !costPressureActive(cfg) {
		return ""
	}
	if !isExpensiveVsPeers(acc, accounts, AICostVeryExpensiveRatio, recent) {
		return ""
	}
	if !hasAffordablePeerCoveringGroups(acc, accounts, recent, true) {
		return ""
	}
	sig := accountCostSignalOf(acc)
	minC, _ := cheapestKnownComposite(accounts, acc.ID, recent)
	return fmt.Sprintf(
		"性价比硬门禁:贵号 composite=%.3f > 最便宜peer×%.2f(≈%.3f)且分组内仍有健康便宜号;禁止 enable(便宜全挂时会自动放行)",
		sig.Composite, AICostVeryExpensiveRatio, minC*AICostVeryExpensiveRatio,
	)
}

// costSoftQuarantineWeight is the schedule_weight used for cost soft-isolation.
// 0 marks ultimate spare: scheduler preferPrimaryAccounts skips it while any
// non-soft-stopped peer remains; only when cheap peers are all dead does it serve.
const costSoftQuarantineWeight = 0

// injectCostIsolations soft-quarantines very expensive accounts: p200 + weight=0.
// Does NOT ai_disabled — disable is reserved for hard-fail / depleted / explicit model.
// Ultimate-spare recovery: when cheaper peers boom, pilot stops re-pinning w0 and
// may unbury; scheduler already allows w0 only as last resort.
func injectCostIsolations(decision *decision, accounts []Account, cfg AIAutopilotSettings, recent map[int64]AccountTrafficStats) int {
	if decision == nil {
		return 0
	}
	havePri := map[int64]bool{}
	haveWeight := map[int64]bool{}
	// Drop model disable that only cites cost isolation style — keep hard-fail disables.
	accOf := func(id int64) *Account {
		for i := range accounts {
			if accounts[i].ID == id {
				return &accounts[i]
			}
		}
		return nil
	}
	filtered := decision.Actions[:0]
	for _, a := range decision.Actions {
		acc := accOf(a.AccountID)
		if a.Op == AIOpDisable {
			// Strip pure cost-isolation disables from the model; hard-fail still allowed via gate later.
			if acc != nil && costJustifiedIsolation(acc, accounts, cfg, recent) {
				r := strings.ToLower(a.Reason)
				if strings.Contains(r, "性价比") || strings.Contains(r, "composite") || strings.Contains(r, "过贵") || strings.Contains(r, "极贵") {
					continue
				}
			}
		}
		// Model keeps slamming 0.08 中转 to p200/w0 even when they are not 1.75×.
		if acc != nil && !costJustifiedIsolation(acc, accounts, cfg, recent) {
			if a.Op == AIOpSetWeight {
				if n, err := strconv.Atoi(strings.TrimSpace(a.Value)); err == nil && n <= costSoftQuarantineWeight {
					continue
				}
			}
			if a.Op == AIOpSetPriority {
				if n, err := strconv.Atoi(strings.TrimSpace(a.Value)); err == nil && n >= AIMaxPriority {
					continue
				}
			}
		}
		filtered = append(filtered, a)
		if a.Op == AIOpSetPriority {
			havePri[a.AccountID] = true
		}
		if a.Op == AIOpSetWeight {
			haveWeight[a.AccountID] = true
		}
	}
	decision.Actions = filtered

	// cost < 20%: still strip LLM 究极备用, but do not inject p200/w0.
	if !costPressureActive(cfg) {
		return 0
	}

	injected := 0
	for i := range accounts {
		acc := &accounts[i]
		if acc.AIDisabled || acc.Status != StatusActive || !acc.Schedulable {
			continue
		}
		if !costJustifiedIsolation(acc, accounts, cfg, recent) {
			continue
		}
		sig := accountCostSignalOf(acc)
		minC, _ := cheapestKnownComposite(accounts, acc.ID, recent)
		needPri := !havePri[acc.ID] && acc.Priority < AIMaxPriority && cfg.OpAllowed(AIOpSetPriority)
		needW := !haveWeight[acc.ID] && acc.EffectiveScheduleWeight() > costSoftQuarantineWeight && cfg.OpAllowed(AIOpSetWeight)
		if !needPri && !needW {
			continue
		}
		baseReason := fmt.Sprintf(
			"性价比软隔离(究极备用): composite=%.3f > 最便宜peer×%.2f(最便宜=%.3f);不 disable,沉 p%d+weight=%d — 调度仅当便宜号都不可用时才接量",
			sig.Composite, AICostVeryExpensiveRatio, minC, AIMaxPriority, costSoftQuarantineWeight,
		)
		if needPri {
			decision.Actions = append(decision.Actions, decisionAction{
				AccountID:  acc.ID,
				Op:         AIOpSetPriority,
				Value:      strconv.Itoa(AIMaxPriority),
				Reason:     baseReason,
				Confidence: 0.92,
			})
			havePri[acc.ID] = true
			injected++
		}
		if needW {
			decision.Actions = append(decision.Actions, decisionAction{
				AccountID:  acc.ID,
				Op:         AIOpSetWeight,
				Value:      strconv.Itoa(costSoftQuarantineWeight),
				Reason:     baseReason,
				Confidence: 0.92,
			})
			haveWeight[acc.ID] = true
			injected++
		}
	}
	return injected
}

// injectCostIsolationReleases re-enables accounts that were previously ai_disabled
// for cost, and restores p200+weight=0 soft-quarantine when the account is no
// longer very-expensive vs a *healthy* cheapest peer (2chat vs dead 梦幻 0.045).
// Hard-fail / depleted stays disabled / quarantined.
func injectCostIsolationReleases(decision *decision, accounts []Account, cfg AIAutopilotSettings, recent map[int64]AccountTrafficStats) int {
	if decision == nil {
		return 0
	}
	haveEnable := map[int64]bool{}
	havePri := map[int64]bool{}
	haveWeight := map[int64]bool{}
	for _, a := range decision.Actions {
		switch a.Op {
		case AIOpEnable:
			haveEnable[a.AccountID] = true
		case AIOpSetPriority:
			havePri[a.AccountID] = true
		case AIOpSetWeight:
			haveWeight[a.AccountID] = true
		}
	}
	injected := 0
	for i := range accounts {
		acc := &accounts[i]
		if acc.Status != StatusActive || !acc.Schedulable {
			continue
		}
		if reason := balanceGateReason(AIOpEnable, acc); reason != "" {
			continue
		}
		var rst AccountTrafficStats
		if recent != nil {
			rst = recent[acc.ID]
		}
		if recentWindowHardFail(rst) {
			continue
		}
		sig := accountCostSignalOf(acc)
		minC, _ := cheapestKnownComposite(accounts, acc.ID, recent)

		if acc.AIDisabled && cfg.OpAllowed(AIOpEnable) && !haveEnable[acc.ID] {
			veryExp := isExpensiveVsPeers(acc, accounts, AICostVeryExpensiveRatio, recent)
			softExp := isExpensiveVsPeers(acc, accounts, AICostExpensiveRatio, recent)
			noRecent := rst.Requests+rst.Errors == 0
			lift := veryExp || softExp || noRecent
			if !lift {
				n := rst.Requests + rst.Errors
				if n >= 5 {
					sr := float64(rst.Successes) / float64(n)
					lift = sr >= 0.7
				} else {
					lift = true
				}
			}
			if lift {
				decision.Actions = append(decision.Actions, decisionAction{
					AccountID: acc.ID,
					Op:        AIOpEnable,
					Value:     fmt.Sprintf("priority=%d;weight=10", AIMaxPriority),
					Reason: fmt.Sprintf(
						"解除过度 disable: 近窗无硬失败(composite=%.3f);disable 仅留给硬失败/余额耗尽,贵号改用 p%d+低权软隔离",
						sig.Composite, AIMaxPriority,
					),
					Confidence: 0.91,
				})
				haveEnable[acc.ID] = true
				injected++
			}
		}

		if acc.AIDisabled || !acc.AIManaged {
			continue
		}
		if costJustifiedIsolation(acc, accounts, cfg, recent) {
			continue
		}
		stuckW := acc.EffectiveScheduleWeight() <= costSoftQuarantineWeight
		stuckP := acc.Priority >= AIMaxPriority
		if !stuckW && !stuckP {
			continue
		}
		expensive := isExpensiveVsPeers(acc, accounts, AICostExpensiveRatio, recent)
		if stuckW && !haveWeight[acc.ID] && cfg.OpAllowed(AIOpSetWeight) {
			decision.Actions = append(decision.Actions, decisionAction{
				AccountID: acc.ID,
				Op:        AIOpSetWeight,
				Value:     "10",
				Reason: fmt.Sprintf(
					"解除过期软隔离: composite=%.3f 已不再>健康最便宜peer×%.2f(最便宜=%.3f);weight 0→10 恢复备援分流",
					sig.Composite, AICostVeryExpensiveRatio, minC,
				),
				Confidence: 0.92,
			})
			haveWeight[acc.ID] = true
			injected++
		}
		if stuckP && !havePri[acc.ID] && cfg.OpAllowed(AIOpSetPriority) {
			target := AIObservationPriority
			if expensive {
				target = AIPriorityBuriedThreshold
			}
			decision.Actions = append(decision.Actions, decisionAction{
				AccountID: acc.ID,
				Op:        AIOpSetPriority,
				Value:     strconv.Itoa(target),
				Reason: fmt.Sprintf(
					"解除过期软隔离: composite=%.3f 从 p%d 回到 %d(不再相对健康便宜号极贵)",
					sig.Composite, acc.Priority, target,
				),
				Confidence: 0.92,
			})
			havePri[acc.ID] = true
			injected++
		}
	}
	return injected
}

// softUnburyAffordableSpareRescue lifts known-rate accounts that are NOT very expensive
// from spare/deep tiers. Broader than near-cheapest-only (1.25×): 麻豆 0.06 vs 小白 0.04
// was stuck at p200 with weight 5240 and zero traffic under soft scoring.
func softUnburyAffordableSpareRescue(acc *Account, accounts []Account, long, recent AccountTrafficStats, probes map[int64]activationResult) bool {
	if acc == nil {
		return false
	}
	sig := accountCostSignalOf(acc)
	if !sig.Known || sig.Composite <= 0 {
		return false
	}
	// Block only very-expensive (1.75×); 0.06 next to 0.04 must still unbury.
	if isExpensiveVsPeers(acc, accounts, AICostVeryExpensiveRatio, nil) {
		return false
	}
	if recentWindowHardFail(recent) {
		return false
	}
	if spareRescueDeadWindow(long, recent) {
		return false
	}
	if pr, ok := probes[acc.ID]; ok && pr.Fresh && pr.Verdict == "fail" {
		return false
	}
	ln := long.Requests + long.Errors
	if ln >= 10 && !longWindowHealthyEnough(long) {
		return false
	}
	return true
}

// countKnownCheapMainTier counts active main-tier accounts that are not expensive
// vs the cheapest peer (excluding skipID). Used so we never sink the last cheap option.
func countKnownCheapMainTier(accounts []Account, skipID int64, recent map[int64]AccountTrafficStats) int {
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
		if !isExpensiveVsPeers(acc, accounts, AICostExpensiveRatio, recent) {
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
