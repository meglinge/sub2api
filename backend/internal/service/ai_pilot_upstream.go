package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OneAPIGroup is a user-visible group with billing ratio.
type OneAPIGroup struct {
	Name  string
	Ratio float64
}

// OneAPIToken is a row from GET /api/token.
type OneAPIToken struct {
	ID          string
	Name        string
	Key         string
	Group       string
	RemainQuota int64
	Enabled     bool
}

// ParseOneAPIGroupRatioMap accepts UpstreamRouter-compatible shapes:
// {data:{name:{ratio,desc}}} | {data:{group_ratio:{...}}} | {group_ratio:{...}} | bare numbers.
func ParseOneAPIGroupRatioMap(body []byte) []OneAPIGroup {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return nil
	}
	candidates := []any{obj["data"], obj["group_ratio"], obj}
	for _, c := range candidates {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if gr, ok := m["group_ratio"].(map[string]any); ok {
			m = gr
		}
		out := []OneAPIGroup{}
		for name, v := range m {
			switch vv := v.(type) {
			case map[string]any:
				ratio := anyToFloat64(vv["ratio"])
				if ratio <= 0 {
					ratio = anyToFloat64(vv["GroupRatio"])
				}
				if ratio <= 0 {
					ratio = 1
				}
				out = append(out, OneAPIGroup{Name: name, Ratio: ratio})
			default:
				if r := anyToFloat64(v); r > 0 {
					out = append(out, OneAPIGroup{Name: name, Ratio: r})
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// ParseOneAPITokenList accepts new-api data.items and one-api data array.
func ParseOneAPITokenList(body []byte) []OneAPIToken {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return nil
	}
	data := obj["data"]
	var items []any
	switch d := data.(type) {
	case map[string]any:
		items, _ = d["items"].([]any)
		if items == nil {
			items, _ = d["data"].([]any)
		}
	case []any:
		items = d
	}
	out := []OneAPIToken{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		status := int(anyToFloat64(m["status"]))
		tok := OneAPIToken{
			ID:          stringifyAny(m["id"]),
			Name:        strAny(m["name"]),
			Key:         strAny(m["key"]),
			Group:       strAny(m["group"]),
			RemainQuota: int64(anyToFloat64(m["remain_quota"])),
			Enabled:     status == 1 || (status == 0 && m["status"] == nil),
		}
		if status == 0 && m["status"] != nil {
			tok.Enabled = status == 1
		}
		out = append(out, tok)
	}
	return out
}

// ResolveRateFromTokenAndGroups finds the API key's token group and maps to group_ratio.
// apiKey may be full sk-... or suffix; matching is suffix-based when masked.
// Returns matched token remain_quota when available (new-api 500000 quota ≈ $1).
func ResolveRateFromTokenAndGroups(apiKey string, tokens []OneAPIToken, groups []OneAPIGroup) (rate float64, groupName string, remainQuota int64, ok bool) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || len(groups) == 0 {
		return 0, "", 0, false
	}
	ratioByGroup := map[string]float64{}
	for _, g := range groups {
		ratioByGroup[g.Name] = g.Ratio
	}
	for _, t := range tokens {
		if !tokenKeyMatch(apiKey, t.Key) {
			continue
		}
		g := t.Group
		if g == "" {
			g = "default"
		}
		if r, found := ratioByGroup[g]; found && r > 0 {
			return r, g, t.RemainQuota, true
		}
		// group known on token but missing from ratio map → assume 1
		return 1, g, t.RemainQuota, true
	}
	return 0, "", 0, false
}

func tokenKeyMatch(apiKey, listed string) bool {
	apiKey = strings.TrimSpace(apiKey)
	listed = strings.TrimSpace(listed)
	if apiKey == "" || listed == "" {
		return false
	}
	if apiKey == listed {
		return true
	}
	// new-api often masks as 8M3m**********wlh3 while full key is sk-8M3m…wlh3
	// (the "sk-" prefix is dropped from the masked form).
	keyVariants := []string{apiKey}
	if strings.HasPrefix(apiKey, "sk-") {
		keyVariants = append(keyVariants, apiKey[3:])
	}
	if strings.Contains(listed, "*") {
		parts := strings.Split(listed, "*")
		prefix := parts[0]
		suffix := parts[len(parts)-1]
		if suffix != "" {
			okSuffix := false
			for _, k := range keyVariants {
				if strings.HasSuffix(k, suffix) {
					okSuffix = true
					break
				}
			}
			if !okSuffix {
				return false
			}
		}
		if prefix != "" {
			okPrefix := false
			for _, k := range keyVariants {
				if strings.HasPrefix(k, prefix) {
					okPrefix = true
					break
				}
			}
			if !okPrefix {
				return false
			}
		}
		return true
	}
	// bare listed (no mask): exact / sk-stripped / suffix
	for _, k := range keyVariants {
		if k == listed || strings.TrimPrefix(listed, "sk-") == k {
			return true
		}
	}
	if len(apiKey) >= 8 && len(listed) >= 8 {
		return strings.HasSuffix(apiKey, listed[len(listed)-8:]) || strings.HasSuffix(listed, apiKey[len(apiKey)-8:])
	}
	return false
}

const oneAPIQuotaPerDollar = 500000.0

// ParseOneAPIBalanceSelf parses GET /api/user/self quota → USD (500000 quota = $1).
func ParseOneAPIBalanceSelf(body []byte) (status string, usd float64, err error) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return "error", 0, fmt.Errorf("bad self json")
	}
	if succ, ok := obj["success"].(bool); ok && !succ {
		msg, _ := obj["message"].(string)
		return "error", 0, fmt.Errorf("%s", msg)
	}
	data, _ := obj["data"].(map[string]any)
	if data == nil {
		return "error", 0, fmt.Errorf("self empty data")
	}
	// unlimited user quota (some one-api forks)
	if b, ok := data["unlimited_quota"].(bool); ok && b {
		return "unlimited", 0, nil
	}
	quota := anyToFloat64(data["quota"])
	usd = quota / oneAPIQuotaPerDollar
	if usd <= 0 {
		return "depleted", 0, nil
	}
	return "ok", usd, nil
}

// ParseAPIKeyTokenUsageBalance parses GET /api/usage/token (new-api style, sk Bearer).
// Shape: {code:true,data:{total_available,unlimited_quota,...}} or bare data.
func ParseAPIKeyTokenUsageBalance(body []byte) (status string, usd float64, ok bool) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return "", 0, false
	}
	data, _ := obj["data"].(map[string]any)
	if data == nil {
		// some forks return fields at top level
		if _, has := obj["total_available"]; has {
			data = obj
		} else if _, has := obj["unlimited_quota"]; has {
			data = obj
		}
	}
	if data == nil {
		return "", 0, false
	}
	// only accept when object looks like token usage or has quota fields
	objName := strAny(data["object"])
	if objName != "" && objName != "token_usage" && objName != "token" {
		// still allow if unlimited / total_available present
		if data["unlimited_quota"] == nil && data["total_available"] == nil && data["remain_quota"] == nil {
			return "", 0, false
		}
	}
	if b, ok := data["unlimited_quota"].(bool); ok && b {
		return "unlimited", 0, true
	}
	// total_available preferred; fall back remain_quota
	avail := anyToFloat64(data["total_available"])
	if data["total_available"] == nil {
		avail = anyToFloat64(data["remain_quota"])
		if data["remain_quota"] == nil {
			return "", 0, false
		}
	}
	// negative without unlimited → treat as depleted (overdrawn)
	if avail <= 0 {
		return "depleted", 0, true
	}
	return "ok", avail / oneAPIQuotaPerDollar, true
}

// ParseSub2APIBillingRate parses GET /v1/sub2api/billing with sk Bearer.
func ParseSub2APIBillingRate(body []byte) (rate float64, ok bool) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return 0, false
	}
	if strAny(obj["object"]) != "sub2api.key_billing" {
		return 0, false
	}
	for _, k := range []string{"resolved_rate_multiplier", "effective_rate_multiplier", "group_rate_multiplier"} {
		if v := anyToFloat64(obj[k]); v > 0 {
			return v, true
		}
	}
	return 0, false
}

// RemainQuotaToBalance converts new-api remain_quota units to status+USD.
func RemainQuotaToBalance(remain int64) (status string, usd float64) {
	if remain < 0 {
		// many new-api builds use negative remain for unlimited tokens
		return "unlimited", 0
	}
	if remain == 0 {
		return "depleted", 0
	}
	return "ok", float64(remain) / oneAPIQuotaPerDollar
}

const (
	ExtraAIMoneyError    = "ai_money_error"
	ExtraAIMoneyProbedAt = "ai_money_probed_at"
)

// ForceResolveAccountMoney ignores soft cache and always re-probes upstream.
// Used by the account UI refresh button and post-save hooks so operators don't
// wait for the next autopilot cycle.
func (p *AIPilotService) ForceResolveAccountMoney(ctx context.Context, acc *Account) (rate float64, source string, balStatus string, balUSD float64) {
	if acc != nil && acc.Extra != nil {
		delete(acc.Extra, ExtraAIRateCheckedAt)
		delete(acc.Extra, ExtraAIBalanceCheckedAt)
	}
	return p.ResolveAccountMoney(ctx, acc)
}

// ResolveAccountMoney attempts live rate/balance refresh when possible:
//  1. new-api/one-api mgmt token → groups+token rate + /api/user/self balance
//  2. account API key → /v1/sub2api/billing rate + /api/usage/token balance (no mgmt needed)
// otherwise uses cached extra / billing probe / defaults. Never invents multi-group rates.
// When a live refresh succeeds, mutations are written to acc.Extra and persisted via
// Accounts.UpdateExtra so applyDecisionActions (GetByID) sees depleted/rate state.
func (p *AIPilotService) ResolveAccountMoney(ctx context.Context, acc *Account) (rate float64, source string, balStatus string, balUSD float64) {
	rate, source = resolveAccountRate(acc)
	balStatus, balUSD, _ = balanceViewFromAccount(acc)

	if acc == nil {
		return rate, source, balStatus, balUSD
	}
	kind := strings.ToLower(extraString(acc.Extra, ExtraUpstreamKind))
	mgmt := extraString(acc.Extra, ExtraUpstreamMgmtToken)
	uid := extraString(acc.Extra, ExtraUpstreamMgmtUserID)
	base := strings.TrimRight(strings.TrimSpace(acc.GetOpenAIBaseURL()), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(acc.GetCredential("base_url")), "/")
	}
	apiKey := strings.TrimSpace(acc.GetOpenAIApiKey())
	if apiKey != "" && strings.Contains(apiKey, "*") {
		apiKey = "" // masked key cannot call upstream
	}

	persist := map[string]any{}
	var errParts []string
	writeRate := func(r float64, src string) {
		rate, source = r, src
		if acc.Extra == nil {
			acc.Extra = map[string]any{}
		}
		now := time.Now().UTC().Format(time.RFC3339)
		acc.Extra[ExtraAIRateMultiplier] = r
		acc.Extra[ExtraAIRateSource] = src
		acc.Extra[ExtraAIRateCheckedAt] = now
		persist[ExtraAIRateMultiplier] = r
		persist[ExtraAIRateSource] = src
		persist[ExtraAIRateCheckedAt] = now
	}
	writeBal := func(st string, usd float64) {
		balStatus, balUSD = st, usd
		if acc.Extra == nil {
			acc.Extra = map[string]any{}
		}
		now := time.Now().UTC().Format(time.RFC3339)
		acc.Extra[ExtraAIBalanceStatus] = st
		acc.Extra[ExtraAIBalanceUSD] = usd
		acc.Extra[ExtraAIBalanceCheckedAt] = now
		persist[ExtraAIBalanceStatus] = st
		persist[ExtraAIBalanceUSD] = usd
		persist[ExtraAIBalanceCheckedAt] = now
	}

	// --- Rate refresh ---
	// Empty kind + mgmt present: treat as newapi (UI often sets token before kind sticks).
	needNewAPI := mgmt != "" && base != "" && (kind == "newapi" || kind == "oneapi" || kind == "" || kind == "manual")
	var matchedTokenRemain int64 // non-zero only when token list matched; used as balance last resort
	gotRate := rateCacheFresh(acc) && source != "default_one" && source != "custom" && source != ""
	if needNewAPI && !rateCacheFresh(acc) {
		if r, src, remain, ok := p.fetchNewAPIRate(ctx, base, mgmt, uid, apiKey); ok {
			writeRate(r, src)
			matchedTokenRemain = remain
			gotRate = true
		} else {
			errParts = append(errParts, "newapi_rate_unresolved")
		}
	}
	// sub2api key billing (sk) when rate still soft/default and ai-rate cache stale.
	// Do NOT overwrite billing_probe/newapi/imported — those are already high-confidence.
	if base != "" && apiKey != "" && !rateCacheFresh(acc) {
		switch source {
		case "default_one", "custom", "":
			if r, ok := p.fetchSub2APIRate(ctx, base, apiKey); ok {
				writeRate(r, "sub2api")
				gotRate = true
			}
		}
	}

	// --- Balance refresh (UpstreamRouter parity) ---
	// priority: mgmt /api/user/self → sk GET /v1/usage (sub2api) → sk /api/usage/token/
	//         → sk dashboard billing → matched token remain_quota
	gotBal := balanceCacheFresh(acc) && balStatus != "" && balStatus != "unknown"
	if base != "" && !balanceCacheFresh(acc) {
		gotBal = false
		if needNewAPI {
			if st, usd, err := p.fetchNewAPIBalance(ctx, base, mgmt, uid); err == nil {
				writeBal(st, usd)
				gotBal = true
			} else if err != nil {
				errParts = append(errParts, "newapi_self:"+err.Error())
			}
		}
		// Original BalanceChecker path: sk probes both families (sub2api /v1/usage first).
		if !gotBal && apiKey != "" {
			if st, usd, ok := p.probeUpstreamBalanceSK(ctx, base, apiKey); ok {
				// Prefer real wallet over token-unlimited placeholder when mgmt self failed
				// but token usage only says unlimited — still better than unknown.
				writeBal(st, usd)
				gotBal = true
			}
		}
		// last resort: remain_quota on the matched token from the rate call
		if !gotBal && matchedTokenRemain != 0 {
			st, usd := RemainQuotaToBalance(matchedTokenRemain)
			writeBal(st, usd)
			gotBal = true
		}
		if !gotBal {
			errParts = append(errParts, "balance_unresolved")
		}
	}

	// Always stamp probe attempt so UI can show "last tried" even on partial fail.
	now := time.Now().UTC().Format(time.RFC3339)
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	acc.Extra[ExtraAIMoneyProbedAt] = now
	persist[ExtraAIMoneyProbedAt] = now
	if gotRate && gotBal {
		delete(acc.Extra, ExtraAIMoneyError)
		persist[ExtraAIMoneyError] = ""
	} else if len(errParts) > 0 {
		msg := strings.Join(errParts, "; ")
		if len(msg) > 300 {
			msg = msg[:300]
		}
		acc.Extra[ExtraAIMoneyError] = msg
		persist[ExtraAIMoneyError] = msg
	}

	if len(persist) > 0 && p.Accounts != nil {
		_ = p.Accounts.UpdateExtra(ctx, acc.ID, persist)
	}
	// re-read rate after possible writes so return is consistent
	if r := extraFloat(acc.Extra, ExtraAIRateMultiplier); r > 0 {
		rate = r
		if s := extraString(acc.Extra, ExtraAIRateSource); s != "" {
			source = s
		}
	} else {
		rate, source = resolveAccountRate(acc)
	}
	balStatus, balUSD, _ = balanceViewFromAccount(acc)
	return rate, source, balStatus, balUSD
}

func rateCacheFresh(acc *Account) bool {
	ts := extraString(acc.Extra, ExtraAIRateCheckedAt)
	if ts == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return time.Since(t) <= AIRateCacheMaxAge
}

func balanceCacheFresh(acc *Account) bool {
	ts := extraString(acc.Extra, ExtraAIBalanceCheckedAt)
	if ts == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return time.Since(t) <= AIBalanceCacheMaxAge
}

// fetchNewAPIRate returns (rate, source, remainQuota, ok).
// remainQuota is from the matched token when available (0 if unknown).
// Never falls back to an arbitrary first group when multiple groups exist —
// that previously mis-labeled Niko-class keys (e.g. Gemini 0.3 vs codex 0.06).
func (p *AIPilotService) fetchNewAPIRate(ctx context.Context, base, token, userID, apiKey string) (float64, string, int64, bool) {
	// Groups: prefer authenticated self groups (complete map), then public user groups.
	var groups []OneAPIGroup
	for _, path := range []string{"/api/user/self/groups", "/api/user/groups", "/api/group/"} {
		st, body, err := p.oneAPIDo(ctx, base, token, userID, http.MethodGet, path, nil)
		if err != nil || st != 200 {
			continue
		}
		groups = ParseOneAPIGroupRatioMap(body)
		if len(groups) > 0 {
			break
		}
	}
	if len(groups) == 0 {
		st, body, err := p.oneAPIDo(ctx, base, token, userID, http.MethodGet, "/api/pricing", nil)
		if err == nil && st == 200 {
			groups = ParseOneAPIGroupRatioMap(body)
		}
	}
	if len(groups) == 0 {
		return 0, "", 0, false
	}

	// Token list pagination differs across new-api forks (p=0 vs p=1, size vs page_size).
	var tokens []OneAPIToken
	for _, path := range []string{
		"/api/token/?p=0&size=100",
		"/api/token/?p=1&size=100",
		"/api/token/?page=1&page_size=100",
		"/api/token/?p=0&page_size=100",
		"/api/token/",
	} {
		st, body, err := p.oneAPIDo(ctx, base, token, userID, http.MethodGet, path, nil)
		if err != nil || st != 200 {
			continue
		}
		tokens = ParseOneAPITokenList(body)
		if len(tokens) > 0 {
			break
		}
	}
	if len(tokens) == 0 {
		// without token list: only safe when a single group exists
		if len(groups) == 1 {
			return groups[0].Ratio, "newapi", 0, true
		}
		return 0, "", 0, false
	}
	if r, _, remain, ok := ResolveRateFromTokenAndGroups(apiKey, tokens, groups); ok {
		return r, "newapi", remain, true
	}
	// No token match: single-group is unambiguous; multi-group must not guess.
	if len(groups) == 1 {
		return groups[0].Ratio, "newapi", 0, true
	}
	return 0, "", 0, false
}

func (p *AIPilotService) fetchNewAPIBalance(ctx context.Context, base, token, userID string) (string, float64, error) {
	st, body, err := p.oneAPIDo(ctx, base, token, userID, http.MethodGet, "/api/user/self", nil)
	if err != nil {
		return "error", 0, err
	}
	if st != 200 {
		return "error", 0, fmt.Errorf("self HTTP %d", st)
	}
	return ParseOneAPIBalanceSelf(body)
}

// fetchSub2APIRate probes /v1/sub2api/billing with the account API key.
func (p *AIPilotService) fetchSub2APIRate(ctx context.Context, base, apiKey string) (float64, bool) {
	st, body, err := p.apiKeyDo(ctx, base, apiKey, http.MethodGet, "/v1/sub2api/billing")
	if err != nil || st != 200 {
		return 0, false
	}
	return ParseSub2APIBillingRate(body)
}

// probeUpstreamBalanceSK mirrors UpstreamRouter balance.Probe (sk-only path):
// concurrent-style sequential tries of sub2api /v1/usage then new-api token usage,
// then one-api dashboard billing (last — many new-api forks return 1e8 hard_limit = unlimited).
func (p *AIPilotService) probeUpstreamBalanceSK(ctx context.Context, base, apiKey string) (string, float64, bool) {
	// 1) sub2api-family: GET /v1/usage → balance / remaining / quota
	if st, body, err := p.apiKeyDo(ctx, base, apiKey, http.MethodGet, "/v1/usage"); err == nil && st == 200 {
		if status, usd, ok := ParseSub2APIUsageBalance(body); ok {
			return status, usd, true
		}
	}
	// 2) new-api token usage (prefer trailing slash — bare path 301s and drops auth)
	for _, path := range []string{"/api/usage/token/", "/api/usage/token"} {
		if st, body, err := p.apiKeyDo(ctx, base, apiKey, http.MethodGet, path); err == nil && st == 200 {
			if status, usd, ok := ParseAPIKeyTokenUsageBalance(body); ok {
				return status, usd, true
			}
		}
	}
	// 3) one-api / new-api dashboard subscription (often placeholder unlimited hard_limit)
	if status, usd, ok := p.fetchOneAPIDashboardBalance(ctx, base, apiKey); ok {
		// Ignore pure unlimited placeholder when we only got the 1e8 hard_limit shape —
		// caller can still use mgmt /api/user/self for real wallet.
		if status == "unlimited" {
			return status, usd, true
		}
		return status, usd, true
	}
	return "", 0, false
}

// ParseSub2APIUsageBalance parses GET /v1/usage (UpstreamRouter probeSub2API).
// Live shape seen on pool vendors: {"balance":333.71,"daily_usage":[...]}
// Also accepts mode/isValid/remaining/quota/rate_limits from classic sub2api.
func ParseSub2APIUsageBalance(body []byte) (status string, usd float64, ok bool) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil || !looksLikeSub2APIUsage(obj) {
		return "", 0, false
	}
	// top-level balance (most pool sub2api deployments)
	if _, has := obj["balance"]; has {
		usd = anyToFloat64(obj["balance"])
		return balanceStatusFromUSD(usd), usd, true
	}
	if _, has := obj["remaining"]; has {
		usd = anyToFloat64(obj["remaining"])
		return balanceStatusFromUSD(usd), usd, true
	}
	if quota, ok := obj["quota"].(map[string]any); ok {
		if _, has := quota["remaining"]; has {
			usd = anyToFloat64(quota["remaining"])
			return balanceStatusFromUSD(usd), usd, true
		}
	}
	// isValid=false with remaining 0
	if valid, ok := obj["isValid"].(bool); ok && !valid {
		return "depleted", 0, true
	}
	// rate_limits windows: take tightest remaining
	if windows, ok := obj["rate_limits"].([]any); ok && len(windows) > 0 {
		best := -1.0
		found := false
		for _, w := range windows {
			m, _ := w.(map[string]any)
			if m == nil {
				continue
			}
			if _, has := m["remaining"]; !has {
				continue
			}
			v := anyToFloat64(m["remaining"])
			if !found || v < best {
				best, found = v, true
			}
		}
		if found {
			return balanceStatusFromUSD(best), best, true
		}
	}
	// recognized as sub2api but no quota concept → unlimited key
	return "unlimited", 0, true
}

func looksLikeSub2APIUsage(j map[string]any) bool {
	if j == nil {
		return false
	}
	for _, k := range []string{"mode", "isValid", "quota", "rate_limits", "planName", "remaining", "balance", "daily_usage"} {
		if _, ok := j[k]; ok {
			return true
		}
	}
	return false
}

func balanceStatusFromUSD(usd float64) string {
	if usd <= 0 {
		return "depleted"
	}
	return "ok"
}

// fetchOneAPIDashboardBalance: hard_limit_usd - total_usage/100 (usage is cents).
func (p *AIPilotService) fetchOneAPIDashboardBalance(ctx context.Context, base, apiKey string) (string, float64, bool) {
	st, body, err := p.apiKeyDo(ctx, base, apiKey, http.MethodGet, "/v1/dashboard/billing/subscription")
	if err != nil || st != 200 {
		return "", 0, false
	}
	var sub map[string]any
	if json.Unmarshal(body, &sub) != nil {
		return "", 0, false
	}
	hard, hasHard := sub["hard_limit_usd"]
	if !hasHard {
		return "", 0, false
	}
	hardUSD := anyToFloat64(hard)
	// new-api unlimited placeholder (~1e8)
	const unlimitedHard = 10_000_000.0
	if hardUSD >= unlimitedHard {
		return "unlimited", 0, true
	}
	usedUSD := 0.0
	if st2, body2, err2 := p.apiKeyDo(ctx, base, apiKey, http.MethodGet, "/v1/dashboard/billing/usage"); err2 == nil && st2 == 200 {
		var u map[string]any
		if json.Unmarshal(body2, &u) == nil {
			// total_usage unit is cents
			usedUSD = anyToFloat64(u["total_usage"]) / 100.0
		}
	}
	bal := hardUSD - usedUSD
	return balanceStatusFromUSD(bal), bal, true
}

// apiKeyDo is like oneAPIDo but authenticates with the account sk (not mgmt token).
// Uses a browser UA — Cloudflare on many 中转站 rejects the default Go client (error 1010).
func (p *AIPilotService) apiKeyDo(ctx context.Context, base, apiKey, method, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx2)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return resp.StatusCode, b, nil
}

func (p *AIPilotService) oneAPIDo(ctx context.Context, base, token, userID, method, path string, body []byte) (int, []byte, error) {
	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	// short timeout for money refresh
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	doOnce := func(authHeader string) (int, []byte, error) {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx2, method, strings.TrimRight(base, "/")+path, rdr)
		if err != nil {
			return 0, nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		if userID != "" {
			req.Header.Set("New-Api-User", userID)
			// Some new-api forks also accept bare User header.
			req.Header.Set("User-Id", userID)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		return resp.StatusCode, b, nil
	}

	// new-api system access tokens: some builds want "Bearer <tok>", others raw "<tok>".
	// Try Bearer first (OpenAPI style); on 401/403 fall back to raw.
	token = strings.TrimSpace(token)
	if token == "" {
		return doOnce("")
	}
	st, b, err := doOnce("Bearer " + token)
	if err != nil {
		return st, b, err
	}
	if st == http.StatusUnauthorized || st == http.StatusForbidden {
		if st2, b2, err2 := doOnce(token); err2 == nil && st2 > 0 && st2 < 400 {
			return st2, b2, nil
		}
	}
	return st, b, nil
}

func strAny(v any) string {
	s, _ := v.(string)
	return s
}

func stringifyAny(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// buildGroupPeers attaches peers[] with trusted composite rates for same-group rebalance.
func buildGroupPeers(chs []map[string]any, groupAvail map[int64]map[string]any) []map[string]any {
	// index channels by group
	byGroup := map[int64][]map[string]any{}
	for _, ch := range chs {
		gs, _ := ch["groups"].([]int64)
		if gs == nil {
			// JSON unmarshaling may leave []any
			if raw, ok := ch["groups"].([]any); ok {
				for _, x := range raw {
					gid := int64(anyToFloat64(x))
					byGroup[gid] = append(byGroup[gid], ch)
				}
				continue
			}
		}
		for _, gid := range gs {
			byGroup[gid] = append(byGroup[gid], ch)
		}
	}
	out := make([]map[string]any, 0, len(groupAvail))
	for gid, meta := range groupAvail {
		peers := []map[string]any{}
		var sumComp, sumSR, sumW float64
		var nComp, nSR, nW, nReq int
		for _, ch := range byGroup[gid] {
			st, _ := ch["state"].(map[string]any)
			aiDis, _ := st["aiDisabled"].(bool)
			sched, _ := st["schedulable"].(bool)
			status, _ := st["status"].(string)
			if aiDis || !sched || status != StatusActive {
				continue
			}
			tr, _ := ch["traffic"].(map[string]any)
			money, _ := ch["money"].(map[string]any)
			w := int(anyToFloat64(ch["weight"]))
			sr := anyToFloat64(tr["successRate"])
			reqN := int(anyToFloat64(tr["requests"]))
			comp := anyToFloat64(money["compositeRateMultiplier"])
			level := 3
			if rc, ok := money["rateConfidence"].(map[string]any); ok {
				if v := anyToFloat64(rc["trustedComposite"]); v > 0 {
					comp = v
				}
				level = int(anyToFloat64(rc["level"]))
			}
			balSt, _ := money["balanceStatus"].(string)
			balUSD := anyToFloat64(money["balanceUsd"])
			peers = append(peers, map[string]any{
				"id": ch["id"], "weight": w, "priority": ch["priority"],
				"requests": reqN, "successRate": sr,
				"avgTtfbMs":           anyToFloat64(tr["avgTtfbMs"]),
				"compositeRate":       comp,
				"rateConfidenceLevel": level,
				"balanceStatus":       balSt,
				"balanceUsd":          balUSD,
			})
			if reqN > 0 {
				sumSR += sr
				nSR++
				nReq += reqN
			}
			if comp > 0 {
				sumComp += comp
				nComp++
			}
			if w > 0 {
				sumW += float64(w)
				nW++
			}
		}
		avg := map[string]any{"sampleChannels": len(peers), "requests": nReq}
		if nSR > 0 {
			avg["successRate"] = sumSR / float64(nSR)
		}
		if nComp > 0 {
			avg["compositeRate"] = sumComp / float64(nComp)
		}
		if nW > 0 {
			avg["weight"] = sumW / float64(nW)
		}
		row := map[string]any{
			"id": gid, "channelCount": meta["channelCount"], "availableCount": meta["availableCount"],
			"peerAvg": avg, "peers": peers,
		}
		out = append(out, row)
	}
	return out
}
