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

// Upstream group switch (new-api token.group / sub2api panel key.group_id).
//
// Safety model (v2): NEVER probe by switching the production key first.
//  1. Create a temporary upstream key/token on the *target* group
//  2. Probe that temp credential (production traffic untouched)
//  3. On pass: soft-drain production account, switch prod key group, probe prod sk
//  4. On prod probe fail: roll production group back
//  5. Always delete the temporary key/token
//
// CF/captcha login still out of scope — plain email+password + session-bound UA.
const (
	ExtraAIUpstreamGroupSwitch       = "ai_upstream_group_switch" // bool, default false
	ExtraUpstreamPanelEmail          = "upstream_panel_email"
	ExtraUpstreamPanelPassword       = "upstream_panel_password"
	ExtraUpstreamPanelAccessToken    = "upstream_panel_access_token"
	ExtraUpstreamPanelRefreshToken   = "upstream_panel_refresh_token"
	ExtraUpstreamPanelTokenExpiresAt = "upstream_panel_token_expires_at"
	ExtraUpstreamPanelKeyID          = "upstream_panel_key_id" // cached matched production key id
	ExtraUpstreamCurrentGroup        = "upstream_current_group"
	ExtraUpstreamLastGoodGroup       = "upstream_last_good_group"
	ExtraUpstreamLastGroupSwitchAt   = "upstream_last_group_switch_at"
	ExtraUpstreamGroupCandidatesJSON = "upstream_group_candidates_json"
	ExtraUpstreamGroupCandidatesAt   = "upstream_group_candidates_at"
	// ExtraUpstreamGroupDenylist: JSON string array of group ids/names that failed after switch.
	ExtraUpstreamGroupDenylist = "upstream_group_denylist"

	// AIUpstreamGroupSwitchDwell blocks thrash after a successful switch.
	AIUpstreamGroupSwitchDwell = 30 * time.Minute
	// AIUpstreamGroupObserveWindow: if hard-fail/503 within this after switch → auto-rollback.
	AIUpstreamGroupObserveWindow = 20 * time.Minute
	// AIUpstreamGroupMinSaveRatio: target must be at least this cheaper (ratio lower).
	AIUpstreamGroupMinSaveRatio = 0.15
	// Soft-drain window before flipping production group (keeps live traffic off the key).
	AIUpstreamGroupSoftDrain = 3 * time.Second
	// Candidate list soft cache.
	AIUpstreamGroupCandidatesCache = 5 * time.Minute
	// Multi-probe counts (temp key / production verify).
	AIUpstreamGroupTestProbes = 3
	AIUpstreamGroupProdProbes = 2

	// upstreamPanelUserAgent must be identical for login and subsequent panel calls:
	// many sub2api hosts enable session binding (IP+UA hash).
	upstreamPanelUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// UpstreamGroupCandidate is a switchable billing group on the upstream panel.
type UpstreamGroupCandidate struct {
	ID       string  `json:"id,omitempty"` // sub2api numeric id as string
	Name     string  `json:"name"`
	Ratio    float64 `json:"ratio"`
	Platform string  `json:"platform,omitempty"`
	Source   string  `json:"source"` // newapi|sub2api
	Eligible bool    `json:"eligible"`
	Note     string  `json:"note,omitempty"`
}

// UpstreamGroupView is attached to pilot channel snapshots.
type UpstreamGroupView struct {
	Switchable        bool                     `json:"switchable"`
	SwitchEnabled     bool                     `json:"switchEnabled"`
	Kind              string                   `json:"kind,omitempty"`
	Current           string                   `json:"current,omitempty"`
	CurrentRatio      float64                  `json:"currentRatio,omitempty"`
	AccountPlatform   string                   `json:"accountPlatform,omitempty"`
	Candidates        []UpstreamGroupCandidate `json:"candidates,omitempty"`
	DwellRemainingSec int                      `json:"dwellRemainingSec,omitempty"`
	Reason            string                   `json:"reason,omitempty"`
	SafetyNote        string                   `json:"safetyNote,omitempty"`
}

func accountUpstreamGroupSwitchEnabled(acc *Account) bool {
	if acc == nil || acc.Extra == nil {
		return false
	}
	v, ok := acc.Extra[ExtraAIUpstreamGroupSwitch]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "true") || t == "1"
	case float64:
		return t != 0
	default:
		return false
	}
}

func upstreamGroupSwitchInDwell(acc *Account, now time.Time) (bool, time.Duration) {
	if now.IsZero() {
		now = time.Now()
	}
	ts := extraString(acc.Extra, ExtraUpstreamLastGroupSwitchAt)
	if ts == "" {
		return false, 0
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false, 0
	}
	elapsed := now.Sub(t)
	if elapsed >= AIUpstreamGroupSwitchDwell {
		return false, 0
	}
	return true, AIUpstreamGroupSwitchDwell - elapsed
}

// platformMatchesAccount keeps OpenAI accounts off Claude groups etc.
func platformMatchesAccount(accPlatform, groupPlatform string) bool {
	accPlatform = strings.ToLower(strings.TrimSpace(accPlatform))
	groupPlatform = strings.ToLower(strings.TrimSpace(groupPlatform))
	if groupPlatform == "" {
		// Unknown platform: allow only if account is openai (common default on some panels)
		return accPlatform == "" || accPlatform == PlatformOpenAI || accPlatform == "openai"
	}
	if accPlatform == "" || accPlatform == "openai" {
		accPlatform = PlatformOpenAI
	}
	// normalize aliases
	if groupPlatform == "openai" || groupPlatform == "chatgpt" || groupPlatform == "codex" {
		groupPlatform = PlatformOpenAI
	}
	if groupPlatform == "claude" || groupPlatform == "anthropic" {
		groupPlatform = PlatformAnthropic
	}
	return accPlatform == groupPlatform
}

func accountPlatformForUpstream(acc *Account) string {
	if acc == nil {
		return PlatformOpenAI
	}
	p := strings.ToLower(strings.TrimSpace(acc.Platform))
	if p == "" || p == "openai" {
		return PlatformOpenAI
	}
	return p
}

// buildUpstreamGroupView prefers cached candidates; LiveList fills on demand when empty.
func buildUpstreamGroupView(acc *Account) UpstreamGroupView {
	v := UpstreamGroupView{
		SafetyNote: "切组前会新建临时 key 探测,通过后才改生产 key;近窗硬失败/非同平台组禁止",
	}
	if acc == nil {
		v.Reason = "no account"
		return v
	}
	v.SwitchEnabled = accountUpstreamGroupSwitchEnabled(acc)
	v.AccountPlatform = accountPlatformForUpstream(acc)
	kind := strings.ToLower(extraString(acc.Extra, ExtraUpstreamKind))
	if kind == "oneapi" {
		kind = "newapi"
	}
	v.Kind = kind
	v.Current = extraString(acc.Extra, ExtraUpstreamCurrentGroup)
	if r := extraFloat(acc.Extra, ExtraAIRateMultiplier); r > 0 {
		v.CurrentRatio = r
	}
	if cands := loadCachedUpstreamCandidates(acc); len(cands) > 0 {
		v.Candidates = cands
	}
	if in, rem := upstreamGroupSwitchInDwell(acc, time.Now()); in {
		v.DwellRemainingSec = int(rem.Seconds())
	}
	if !v.SwitchEnabled {
		v.Reason = "账号未开启 ai_upstream_group_switch"
		return v
	}
	mgmt := extraString(acc.Extra, ExtraUpstreamMgmtToken)
	email := extraString(acc.Extra, ExtraUpstreamPanelEmail)
	pass := extraString(acc.Extra, ExtraUpstreamPanelPassword)
	tok := extraString(acc.Extra, ExtraUpstreamPanelAccessToken)
	switch {
	case kind == "newapi" || (kind == "" && mgmt != ""):
		if mgmt == "" {
			v.Reason = "newapi 缺少 upstream_mgmt_token"
			return v
		}
		v.Kind = "newapi"
		v.Switchable = true
	case kind == "sub2api" || (kind == "" && (email != "" || tok != "")):
		if email == "" || pass == "" {
			if tok == "" {
				v.Reason = "sub2api 需要面板邮箱+密码（或 access_token）"
				return v
			}
		}
		v.Kind = "sub2api"
		v.Switchable = true
	default:
		v.Reason = "upstream_kind 不支持切组或缺少凭证"
		return v
	}
	return v
}

func loadCachedUpstreamCandidates(acc *Account) []UpstreamGroupCandidate {
	raw := extraString(acc.Extra, ExtraUpstreamGroupCandidatesJSON)
	if raw == "" {
		return nil
	}
	at := extraString(acc.Extra, ExtraUpstreamGroupCandidatesAt)
	if at != "" {
		if t, err := time.Parse(time.RFC3339, at); err == nil && time.Since(t) > AIUpstreamGroupCandidatesCache {
			return nil
		}
	}
	var out []UpstreamGroupCandidate
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

func (p *AIPilotService) cacheUpstreamCandidates(ctx context.Context, acc *Account, cands []UpstreamGroupCandidate) {
	if acc == nil {
		return
	}
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	b, _ := json.Marshal(cands)
	now := time.Now().UTC().Format(time.RFC3339)
	acc.Extra[ExtraUpstreamGroupCandidatesJSON] = string(b)
	acc.Extra[ExtraUpstreamGroupCandidatesAt] = now
	if p.Accounts != nil {
		_ = p.Accounts.UpdateExtra(ctx, acc.ID, map[string]any{
			ExtraUpstreamGroupCandidatesJSON: string(b),
			ExtraUpstreamGroupCandidatesAt:   now,
		})
	}
}

// ListUpstreamGroupCandidates fetches + filters groups for this account (network).
func (p *AIPilotService) ListUpstreamGroupCandidates(ctx context.Context, acc *Account) ([]UpstreamGroupCandidate, error) {
	if acc == nil {
		return nil, fmt.Errorf("account nil")
	}
	if cached := loadCachedUpstreamCandidates(acc); len(cached) > 0 {
		return cached, nil
	}
	kind := strings.ToLower(extraString(acc.Extra, ExtraUpstreamKind))
	if kind == "oneapi" {
		kind = "newapi"
	}
	base := accountUpstreamBase(acc)
	if base == "" {
		return nil, fmt.Errorf("缺少 base_url")
	}
	wantPlat := accountPlatformForUpstream(acc)
	var out []UpstreamGroupCandidate
	var err error
	switch {
	case kind == "newapi" || extraString(acc.Extra, ExtraUpstreamMgmtToken) != "":
		out, err = p.listNewAPIGroupCandidates(ctx, acc, base, wantPlat)
	case kind == "sub2api" || extraString(acc.Extra, ExtraUpstreamPanelEmail) != "":
		out, err = p.listSub2APIGroupCandidates(ctx, acc, base, wantPlat)
	default:
		return nil, fmt.Errorf("不支持的 upstream_kind")
	}
	if err != nil {
		return nil, err
	}
	// mark eligible vs current ratio / denylist / risky names
	cur := extraFloat(acc.Extra, ExtraAIRateMultiplier)
	deny := loadUpstreamGroupDenylist(acc)
	for i := range out {
		if !platformMatchesAccount(wantPlat, out[i].Platform) {
			out[i].Eligible = false
			out[i].Note = "platform 不匹配账号(" + wantPlat + ")"
			continue
		}
		if denylistHas(deny, out[i].ID, out[i].Name) {
			out[i].Eligible = false
			out[i].Note = "denylist:曾切后失败/503"
			continue
		}
		if riskyUpstreamGroupName(out[i].Name) {
			out[i].Eligible = false
			out[i].Note = "组名高风险(随时拉闸/测试等),禁止自动切入"
			continue
		}
		out[i].Eligible = true
		if cur > 0 && out[i].Ratio > 0 {
			if out[i].Ratio < cur*(1-AIUpstreamGroupMinSaveRatio)-1e-12 {
				out[i].Note = "更便宜,可考虑"
			} else if out[i].Ratio > cur+1e-12 {
				out[i].Note = "更贵,仅故障回退"
			} else {
				out[i].Note = "与当前接近"
			}
		}
	}
	p.cacheUpstreamCandidates(ctx, acc, out)
	return out, nil
}

func loadUpstreamGroupDenylist(acc *Account) []string {
	if acc == nil || acc.Extra == nil {
		return nil
	}
	raw, _ := acc.Extra[ExtraUpstreamGroupDenylist]
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		var arr []string
		if json.Unmarshal([]byte(v), &arr) == nil {
			return arr
		}
		return nil
	case []any:
		out := make([]string, 0, len(v))
		for _, it := range v {
			if s, ok := it.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	default:
		// jsonb array may already be []interface via map
		b, err := json.Marshal(raw)
		if err != nil {
			return nil
		}
		var arr []string
		_ = json.Unmarshal(b, &arr)
		return arr
	}
}

func denylistHas(deny []string, id, name string) bool {
	for _, d := range deny {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if d == id || strings.EqualFold(d, name) || strings.HasPrefix(name, d+":") || strings.HasPrefix(d, id+":") {
			return true
		}
	}
	return false
}

// riskyUpstreamGroupName flags marketing/unstable channels by name keywords.
func riskyUpstreamGroupName(name string) bool {
	low := strings.ToLower(name)
	keys := []string{
		"随时拉闸", "拉闸", "不稳", "测试", "test", "trial", "临时",
		"demo", "free", "福利", "限时", "可炸",
	}
	for _, k := range keys {
		if strings.Contains(low, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

func appendUpstreamGroupDenylist(acc *Account, idOrName string) map[string]any {
	if acc == nil || idOrName == "" {
		return nil
	}
	deny := loadUpstreamGroupDenylist(acc)
	if denylistHas(deny, idOrName, idOrName) {
		return nil
	}
	deny = append(deny, idOrName)
	b, _ := json.Marshal(deny)
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	acc.Extra[ExtraUpstreamGroupDenylist] = string(b)
	// also clear candidates cache
	delete(acc.Extra, ExtraUpstreamGroupCandidatesJSON)
	delete(acc.Extra, ExtraUpstreamGroupCandidatesAt)
	return map[string]any{
		ExtraUpstreamGroupDenylist:       string(b),
		ExtraUpstreamGroupCandidatesJSON: "",
		ExtraUpstreamGroupCandidatesAt:   "",
	}
}

func accountUpstreamBase(acc *Account) string {
	if acc == nil {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(acc.GetOpenAIBaseURL()), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(acc.GetCredential("base_url")), "/")
	}
	return base
}

func (p *AIPilotService) listSub2APIGroupCandidates(ctx context.Context, acc *Account, base, wantPlat string) ([]UpstreamGroupCandidate, error) {
	jwt, err := p.ensureSub2APIPanelJWT(ctx, acc, base)
	if err != nil {
		return nil, err
	}
	groups, err := p.sub2apiListAvailableGroups(ctx, base, jwt)
	if err != nil {
		return nil, err
	}
	out := make([]UpstreamGroupCandidate, 0, len(groups))
	for _, g := range groups {
		if !platformMatchesAccount(wantPlat, g.Platform) {
			continue // hard-drop other platforms from candidate list
		}
		out = append(out, UpstreamGroupCandidate{
			ID:       strconv.FormatInt(g.ID, 10),
			Name:     g.Name,
			Ratio:    g.RateMultiplier,
			Platform: g.Platform,
			Source:   "sub2api",
			Eligible: true,
		})
	}
	return out, nil
}

func (p *AIPilotService) listNewAPIGroupCandidates(ctx context.Context, acc *Account, base, wantPlat string) ([]UpstreamGroupCandidate, error) {
	mgmt := extraString(acc.Extra, ExtraUpstreamMgmtToken)
	uid := extraString(acc.Extra, ExtraUpstreamMgmtUserID)
	if mgmt == "" {
		return nil, fmt.Errorf("newapi 缺少 mgmt token")
	}
	var groups []OneAPIGroup
	for _, path := range []string{"/api/user/self/groups", "/api/user/groups", "/api/group/", "/api/pricing"} {
		st, body, e := p.oneAPIDo(ctx, base, mgmt, uid, http.MethodGet, path, nil)
		if e != nil || st != 200 {
			continue
		}
		groups = ParseOneAPIGroupRatioMap(body)
		if len(groups) > 0 {
			break
		}
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("无法拉取 newapi 分组")
	}
	// new-api groups rarely carry platform — treat as matching account platform
	// but drop names that clearly look like other stacks when account is openai.
	out := make([]UpstreamGroupCandidate, 0, len(groups))
	for _, g := range groups {
		plat := wantPlat
		low := strings.ToLower(g.Name)
		if strings.Contains(low, "claude") || strings.Contains(low, "anthropic") || strings.Contains(low, "kiro") {
			plat = PlatformAnthropic
		}
		if strings.Contains(low, "gemini") || strings.Contains(low, "google") {
			plat = PlatformGemini
		}
		if !platformMatchesAccount(wantPlat, plat) {
			continue
		}
		out = append(out, UpstreamGroupCandidate{
			ID:       g.Name,
			Name:     g.Name,
			Ratio:    g.Ratio,
			Platform: plat,
			Source:   "newapi",
			Eligible: true,
		})
	}
	return out, nil
}

// SwitchUpstreamGroup: safe test-key probe then production switch.
// value: group id (sub2api) or group name (new-api).
func (p *AIPilotService) SwitchUpstreamGroup(ctx context.Context, acc *Account, value string) (before, after string, err error) {
	if acc == nil {
		return "", "", fmt.Errorf("account nil")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", fmt.Errorf("目标上游组为空")
	}
	if !accountUpstreamGroupSwitchEnabled(acc) {
		return "", "", fmt.Errorf("账号未开启 ai_upstream_group_switch")
	}
	if in, rem := upstreamGroupSwitchInDwell(acc, time.Now()); in {
		return "", "", fmt.Errorf("切组驻留期内,剩余约 %.0fm", rem.Minutes()+0.5)
	}
	cands, err := p.ListUpstreamGroupCandidates(ctx, acc)
	if err != nil {
		return "", "", err
	}
	lastGood := extraString(acc.Extra, ExtraUpstreamLastGoodGroup)
	lastGoodID := strings.Split(lastGood, ":")[0]
	isRollback := lastGood != "" && (value == lastGood || value == lastGoodID ||
		strings.HasPrefix(lastGood, value+":") || strings.EqualFold(value, lastGood))
	target, ok := resolveUpstreamTarget(cands, value)
	if !ok && isRollback {
		// Emergency rollback may target a group filtered from "cheap" eligibility.
		target = UpstreamGroupCandidate{ID: lastGoodID, Name: lastGood, Eligible: true, Source: "rollback"}
		ok = true
	}
	if !ok {
		return "", "", fmt.Errorf("目标组 %q 不在同平台候选列表(已排除 Claude/拉闸/denylist 等)", value)
	}
	if !target.Eligible && !isRollback {
		return "", "", fmt.Errorf("目标组不可用: %s", target.Note)
	}
	// Enforce min-save when switching to cheaper (not for emergency rollback).
	curRatio := extraFloat(acc.Extra, ExtraAIRateMultiplier)
	if !isRollback && curRatio > 0 && target.Ratio > 0 && target.Ratio < curRatio-1e-12 {
		if target.Ratio > curRatio*(1-AIUpstreamGroupMinSaveRatio)+1e-12 {
			return "", "", fmt.Errorf("目标组仅便宜 %.1f%%,低于最低省钱阈值 %.0f%%",
				(1-target.Ratio/curRatio)*100, AIUpstreamGroupMinSaveRatio*100)
		}
	}
	if !isRollback && riskyUpstreamGroupName(target.Name) {
		return "", "", fmt.Errorf("目标组名高风险,禁止自动切入: %s", target.Name)
	}

	kind := strings.ToLower(extraString(acc.Extra, ExtraUpstreamKind))
	if kind == "oneapi" {
		kind = "newapi"
	}
	base := accountUpstreamBase(acc)
	apiKey := strings.TrimSpace(acc.GetOpenAIApiKey())
	if apiKey != "" && strings.Contains(apiKey, "*") {
		apiKey = ""
	}
	if kind == "" {
		if extraString(acc.Extra, ExtraUpstreamMgmtToken) != "" {
			kind = "newapi"
		} else {
			kind = "sub2api"
		}
	}
	switch kind {
	case "sub2api":
		return p.safeSwitchSub2API(ctx, acc, base, apiKey, target)
	case "newapi":
		return p.safeSwitchNewAPI(ctx, acc, base, apiKey, target)
	default:
		return "", "", fmt.Errorf("不支持的 upstream_kind=%s", kind)
	}
}

func resolveUpstreamTarget(cands []UpstreamGroupCandidate, value string) (UpstreamGroupCandidate, bool) {
	for _, c := range cands {
		if c.ID == value || strings.EqualFold(c.Name, value) {
			return c, true
		}
	}
	// numeric id without being in filtered list → reject (e.g. Claude id)
	return UpstreamGroupCandidate{}, false
}

// --- Safe sub2api path ---

func (p *AIPilotService) safeSwitchSub2API(ctx context.Context, acc *Account, base, prodSK string, target UpstreamGroupCandidate) (before, after string, err error) {
	jwt, err := p.ensureSub2APIPanelJWT(ctx, acc, base)
	if err != nil {
		return "", "", err
	}
	targetID, _ := strconv.ParseInt(target.ID, 10, 64)
	if targetID <= 0 {
		return "", "", fmt.Errorf("无效 group id %s", target.ID)
	}
	prodKeyID, curGID, err := p.sub2apiFindKey(ctx, base, jwt, prodSK, acc)
	if err != nil {
		return "", "", err
	}
	before = strconv.FormatInt(curGID, 10)
	if curGID == targetID {
		return before, before, nil
	}

	// 1) Create temporary key on TARGET group — production key still on old group.
	testName := fmt.Sprintf("ai-probe-%d-%d", acc.ID, targetID)
	if len(testName) > 40 {
		testName = testName[:40]
	}
	testID, testSK, err := p.sub2apiCreateKey(ctx, base, jwt, testName, targetID)
	if err != nil {
		return before, "", fmt.Errorf("创建测试 key 失败: %w", err)
	}
	defer func() {
		_ = p.sub2apiDeleteKey(ctx, base, jwt, testID)
	}()

	// 2) Multi-probe temp key (prod traffic unaffected).
	if ok, detail := p.multiProbeUpstreamKey(ctx, acc, base, testSK, AIUpstreamGroupTestProbes, "upstream-group-test-key"); !ok {
		if deny := appendUpstreamGroupDenylist(acc, target.ID); deny != nil && p.Accounts != nil {
			_ = p.Accounts.UpdateExtra(ctx, acc.ID, deny)
		}
		return before, "", fmt.Errorf("测试 key 多次探测失败 — 生产 key 未改动: %s", detail)
	}

	// 3) Soft-drain production account then switch production key.
	// Keep drained at spare after switch for observe window (do NOT restore main tier).
	prevPri, drained := p.softDrainAccount(ctx, acc)
	_ = prevPri
	_ = drained

	if err := p.sub2apiUpdateKeyGroup(ctx, base, jwt, prodKeyID, targetID); err != nil {
		return before, "", fmt.Errorf("生产 key 改组失败(测试已通过): %w", err)
	}

	// 4) Multi-probe production sk; rollback + denylist on fail.
	if prodSK != "" {
		if ok, detail := p.multiProbeUpstreamKey(ctx, acc, base, prodSK, AIUpstreamGroupProdProbes, "upstream-group-prod-verify"); !ok {
			_ = p.sub2apiUpdateKeyGroup(ctx, base, jwt, prodKeyID, curGID)
			deny := appendUpstreamGroupDenylist(acc, target.ID)
			if deny != nil && p.Accounts != nil {
				_ = p.Accounts.UpdateExtra(ctx, acc.ID, deny)
			}
			return before, "", fmt.Errorf("生产 key 切后探测失败,已回滚并拉黑目标组: %s", detail)
		}
	}
	// After success stay at spare tier for observation (avoid dumping traffic into new group).
	if p.Accounts != nil {
		if a, e := p.Accounts.GetByID(ctx, acc.ID); e == nil && a != nil {
			if a.Priority < AIPriorityBuriedThreshold {
				a.Priority = AIPriorityBuriedThreshold
				_ = p.Accounts.Update(ctx, a)
			}
			// Cap weight so p150 fallback cannot monopolize on a brand-new group.
			if a.EffectiveScheduleWeight() > 500 {
				a.ScheduleWeight = 500
				_ = p.Accounts.Update(ctx, a)
			}
		}
	}
	after = strconv.FormatInt(targetID, 10) + ":" + target.Name
	// last_good = previous production group (for auto-rollback)
	p.persistUpstreamGroupSwitch(ctx, acc, before, after, target.Ratio, "sub2api")
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	acc.Extra[ExtraUpstreamPanelKeyID] = prodKeyID
	if p.Accounts != nil {
		_ = p.Accounts.UpdateExtra(ctx, acc.ID, map[string]any{ExtraUpstreamPanelKeyID: prodKeyID})
	}
	return before, after, nil
}

// multiProbeUpstreamKey requires a strict majority of pass/slow across n attempts.
func (p *AIPilotService) multiProbeUpstreamKey(ctx context.Context, acc *Account, base, sk string, n int, reason string) (bool, string) {
	if n <= 0 {
		n = 1
	}
	pass, fail := 0, 0
	var last string
	for i := 0; i < n; i++ {
		pr := p.probeViaUpstreamWithKey(ctx, acc, base, sk, 8*time.Second, reason)
		if pr.Verdict == "pass" || pr.Verdict == "slow" {
			pass++
		} else {
			fail++
			last = pr.Verdict + ": " + pr.Error
		}
		if i+1 < n {
			select {
			case <-ctx.Done():
			case <-time.After(400 * time.Millisecond):
			}
		}
	}
	need := (n + 1) / 2
	if n >= 3 {
		need = 2 // 2/3 for test probes
	}
	if pass >= need {
		return true, fmt.Sprintf("pass=%d/%d", pass, n)
	}
	return false, fmt.Sprintf("pass=%d fail=%d/%d last=%s", pass, fail, n, truncateStr(last, 120))
}

func (p *AIPilotService) softDrainAccount(ctx context.Context, acc *Account) (prev int, drained bool) {
	if acc == nil || p.Accounts == nil {
		return 0, false
	}
	prev = acc.Priority
	if prev > AIObservationPriority {
		return prev, false // already spare
	}
	// Move to spare tier so strict layering steers traffic away before group flip.
	a, err := p.Accounts.GetByID(ctx, acc.ID)
	if err != nil || a == nil {
		return prev, false
	}
	a.Priority = AIPriorityBuriedThreshold
	if err := p.Accounts.Update(ctx, a); err != nil {
		return prev, false
	}
	acc.Priority = AIPriorityBuriedThreshold
	// Brief pause so in-flight selection prefers other main-tier peers.
	select {
	case <-ctx.Done():
	case <-time.After(AIUpstreamGroupSoftDrain):
	}
	return prev, true
}

// --- Safe new-api path ---

func (p *AIPilotService) safeSwitchNewAPI(ctx context.Context, acc *Account, base, prodSK string, target UpstreamGroupCandidate) (before, after string, err error) {
	mgmt := extraString(acc.Extra, ExtraUpstreamMgmtToken)
	uid := extraString(acc.Extra, ExtraUpstreamMgmtUserID)
	if mgmt == "" {
		return "", "", fmt.Errorf("newapi 缺少 mgmt token")
	}
	// List tokens, find production
	tokens, err := p.newAPIListTokens(ctx, base, mgmt, uid)
	if err != nil {
		return "", "", err
	}
	prodTok, ok := matchOneAPIToken(prodSK, tokens)
	if !ok {
		return "", "", fmt.Errorf("token 列表未匹配本账号 sk")
	}
	before = strings.TrimSpace(prodTok.Group)
	if strings.EqualFold(before, target.Name) {
		return before, target.Name, nil
	}

	// 1) Create temp token on target group
	testName := fmt.Sprintf("ai-probe-%d", acc.ID)
	testID, testKey, err := p.newAPICreateToken(ctx, base, mgmt, uid, testName, target.Name)
	if err != nil {
		return before, "", fmt.Errorf("创建测试 token 失败: %w", err)
	}
	defer func() { _ = p.newAPIDeleteToken(ctx, base, mgmt, uid, testID) }()

	if ok, detail := p.multiProbeUpstreamKey(ctx, acc, base, testKey, AIUpstreamGroupTestProbes, "upstream-group-test-token"); !ok {
		if deny := appendUpstreamGroupDenylist(acc, target.Name); deny != nil && p.Accounts != nil {
			_ = p.Accounts.UpdateExtra(ctx, acc.ID, deny)
		}
		return before, "", fmt.Errorf("测试 token 多次探测失败 — 生产未改动: %s", detail)
	}

	_, _ = p.softDrainAccount(ctx, acc)

	if err := p.newAPIUpdateTokenGroup(ctx, base, mgmt, uid, prodTok, target.Name); err != nil {
		return before, "", fmt.Errorf("生产 token 改组失败: %w", err)
	}
	if prodSK != "" {
		if ok, detail := p.multiProbeUpstreamKey(ctx, acc, base, prodSK, AIUpstreamGroupProdProbes, "upstream-group-prod-verify"); !ok {
			_ = p.newAPIUpdateTokenGroup(ctx, base, mgmt, uid, prodTok, before)
			if deny := appendUpstreamGroupDenylist(acc, target.Name); deny != nil && p.Accounts != nil {
				_ = p.Accounts.UpdateExtra(ctx, acc.ID, deny)
			}
			return before, "", fmt.Errorf("生产 token 切后探测失败,已回滚并拉黑: %s", detail)
		}
	}
	if p.Accounts != nil {
		if a, e := p.Accounts.GetByID(ctx, acc.ID); e == nil && a != nil {
			if a.Priority < AIPriorityBuriedThreshold {
				a.Priority = AIPriorityBuriedThreshold
				_ = p.Accounts.Update(ctx, a)
			}
			if a.EffectiveScheduleWeight() > 500 {
				a.ScheduleWeight = 500
				_ = p.Accounts.Update(ctx, a)
			}
		}
	}
	after = target.Name
	p.persistUpstreamGroupSwitch(ctx, acc, before, after, target.Ratio, "newapi")
	return before, after, nil
}

// probeViaUpstreamWithKey is like probeViaUpstream but uses an explicit sk
// (test key / production key) so group tests never require flipping production first.
func (p *AIPilotService) probeViaUpstreamWithKey(
	ctx context.Context,
	acc *Account,
	base, apiKey string,
	timeout time.Duration,
	reason string,
) activationResult {
	res := activationResult{AccountID: 0, Reason: reason, Fresh: true, Source: "upstream"}
	if acc != nil {
		res.AccountID = acc.ID
	}
	if apiKey == "" || strings.Contains(apiKey, "*") {
		res.Verdict = "fail"
		res.Error = "empty api key"
		return res
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		res.Verdict = "fail"
		res.Error = "empty base"
		return res
	}
	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	cfg := DefaultAIAutopilotSettings()
	if p != nil {
		cfg = p.loadSettings(ctx)
	}
	models := resolveActivationProbeModels(cfg, acc)
	if len(models) == 0 {
		models = []string{"gpt-5.6-sol"}
	}
	useResponses := true
	if acc != nil && acc.Extra != nil {
		if v, ok := acc.Extra["openai_responses_supported"].(bool); ok && !v {
			useResponses = false
		}
	}
	attemptTO := timeout
	if attemptTO <= 0 || attemptTO > 8*time.Second {
		attemptTO = 8 * time.Second
	}
	type pathSpec struct {
		path string
		body func(string) map[string]any
	}
	var paths []pathSpec
	prompt := activationProbeInput(cfg)
	if useResponses {
		paths = []pathSpec{{"/responses", func(m string) map[string]any {
			return map[string]any{"model": m, "input": prompt, "stream": true}
		}}}
	} else {
		paths = []pathSpec{{"/chat/completions", func(m string) map[string]any {
			return map[string]any{
				"model": m, "messages": []map[string]string{{"role": "user", "content": prompt}},
				"max_tokens": 1, "temperature": 0, "stream": false,
			}
		}}}
	}
	var lastErr string
	sawTransient := false
	for _, pe := range paths {
		url := joinOpenAIURL(base, pe.path)
		for _, model := range models {
			raw, _ := json.Marshal(pe.body(model))
			reqCtx, cancel := context.WithTimeout(ctx, attemptTO)
			httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(raw))
			if err != nil {
				cancel()
				res.Verdict = "fail"
				res.Error = err.Error()
				return res
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
			httpReq.Header.Set("User-Agent", upstreamPanelUserAgent)
			start := time.Now()
			resp, err := client.Do(httpReq)
			ttfb := time.Since(start).Milliseconds()
			if err != nil {
				cancel()
				lastErr = fmt.Sprintf("model=%s: %v", model, err)
				if probeErrorIsTransient(err.Error()) {
					sawTransient = true
				}
				continue
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
			resp.Body.Close()
			cancel()
			res.TTFBMs = ttfb
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				res.Verdict = "pass"
				if ttfb > 5000 {
					res.Verdict = "slow"
				}
				return res
			}
			lastErr = fmt.Sprintf("HTTP %d model=%s %s", resp.StatusCode, model, truncateStr(string(b), 120))
			if probeHTTPIsTransient(resp.StatusCode) {
				sawTransient = true
				continue
			}
			if resp.StatusCode == 404 || strings.Contains(strings.ToLower(string(b)), "model") {
				continue
			}
		}
	}
	res.Error = lastErr
	if res.Error == "" {
		res.Error = "probe failed"
	}
	if sawTransient {
		res.Verdict = "slow"
	} else {
		res.Verdict = "fail"
	}
	return res
}

// injectUpstreamGroupRollbacks auto-reverts a recent switch when the new group
// is already hard-failing (503 etc.). Uses last_good; denylists the failed group.
func injectUpstreamGroupRollbacks(
	decision *decision,
	accounts []Account,
	recentTraffic map[int64]AccountTrafficStats,
	cfg AIAutopilotSettings,
) int {
	if decision == nil || !cfg.OpAllowed(AIOpSwitchUpstreamGroup) {
		return 0
	}
	have := map[int64]bool{}
	for _, a := range decision.Actions {
		if a.Op == AIOpSwitchUpstreamGroup {
			have[a.AccountID] = true
		}
	}
	injected := 0
	now := time.Now()
	for i := range accounts {
		acc := &accounts[i]
		if !accountUpstreamGroupSwitchEnabled(acc) || !acc.AIManaged || have[acc.ID] {
			continue
		}
		lastSw := extraString(acc.Extra, ExtraUpstreamLastGroupSwitchAt)
		if lastSw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, lastSw)
		if err != nil || now.Sub(t) > AIUpstreamGroupObserveWindow {
			continue
		}
		lastGood := extraString(acc.Extra, ExtraUpstreamLastGoodGroup)
		cur := extraString(acc.Extra, ExtraUpstreamCurrentGroup)
		if lastGood == "" || cur == "" {
			continue
		}
		// already on last good
		if cur == lastGood || strings.HasPrefix(cur, strings.Split(lastGood, ":")[0]+":") ||
			strings.Split(cur, ":")[0] == strings.Split(lastGood, ":")[0] {
			continue
		}
		// Need evidence of pain: recent hard fail OR high error rate with samples
		rst := recentTraffic[acc.ID]
		if !recentWindowHardFail(rst) {
			// also catch mid failures: n>=5 and SR < 0.92 with errors
			n := rst.Requests + rst.Errors
			if n < 5 {
				continue
			}
			sr := float64(rst.Successes) / float64(n)
			if sr >= 0.92 {
				continue
			}
		}
		val := strings.Split(lastGood, ":")[0]
		if val == "" {
			val = lastGood
		}
		// denylist current bad group in-memory for this apply path (persist on execute)
		decision.Actions = append(decision.Actions, decisionAction{
			AccountID: acc.ID,
			Op:        AIOpSwitchUpstreamGroup,
			Value:     val,
			Reason: fmt.Sprintf(
				"切组观察窗内硬失败/503: 当前=%s 自动回滚 last_good=%s 并拉黑失败组;生产先停流血",
				cur, lastGood,
			),
			Confidence: 0.95,
		})
		// also disable to stop traffic while rollback applies
		decision.Actions = append(decision.Actions, decisionAction{
			AccountID:  acc.ID,
			Op:         AIOpDisable,
			Value:      "true",
			Reason:     "切组后故障自动停用,等待回滚 last_good 完成",
			Confidence: 0.9,
		})
		have[acc.ID] = true
		injected++
	}
	return injected
}

// injectUpstreamGroupSwitches proposes at most one safe cheaper switch per run
// for healthy switch-enabled accounts (backend-driven, not LLM-only).
func injectUpstreamGroupSwitches(
	p *AIPilotService,
	ctx context.Context,
	decision *decision,
	accounts []Account,
	recentTraffic map[int64]AccountTrafficStats,
	cfg AIAutopilotSettings,
) int {
	if decision == nil || p == nil || !cfg.OpAllowed(AIOpSwitchUpstreamGroup) {
		return 0
	}
	have := map[int64]bool{}
	for _, a := range decision.Actions {
		if a.Op == AIOpSwitchUpstreamGroup || a.Op == AIOpSetPriority {
			have[a.AccountID] = true
		}
	}
	injected := 0
	const maxPerRun = 1
	for i := range accounts {
		if injected >= maxPerRun {
			break
		}
		acc := &accounts[i]
		if !accountUpstreamGroupSwitchEnabled(acc) || !acc.AIManaged {
			continue
		}
		if have[acc.ID] {
			continue
		}
		if recentWindowHardFail(recentTraffic[acc.ID]) {
			continue
		}
		if in, _ := upstreamGroupSwitchInDwell(acc, time.Now()); in {
			continue
		}
		cands, err := p.ListUpstreamGroupCandidates(ctx, acc)
		if err != nil || len(cands) == 0 {
			continue
		}
		cur := extraFloat(acc.Extra, ExtraAIRateMultiplier)
		if cur <= 0 {
			// try from current group id match
			curID := extraString(acc.Extra, ExtraUpstreamCurrentGroup)
			for _, c := range cands {
				if c.ID == curID || strings.HasPrefix(curID, c.ID+":") || strings.EqualFold(c.Name, curID) {
					cur = c.Ratio
					break
				}
			}
		}
		if cur <= 0 {
			continue
		}
		// pick cheapest eligible with enough savings
		var best *UpstreamGroupCandidate
		for i := range cands {
			c := &cands[i]
			if !c.Eligible || c.Ratio <= 0 {
				continue
			}
			if c.Ratio >= cur*(1-AIUpstreamGroupMinSaveRatio)-1e-12 {
				continue
			}
			if best == nil || c.Ratio < best.Ratio {
				best = c
			}
		}
		if best == nil {
			continue
		}
		val := best.ID
		if val == "" {
			val = best.Name
		}
		decision.Actions = append(decision.Actions, decisionAction{
			AccountID: acc.ID,
			Op:        AIOpSwitchUpstreamGroup,
			Value:     val,
			Reason: fmt.Sprintf(
				"性价比/安全切组: 同平台候选 %s ratio=%.3f < 当前≈%.3f (省≥%.0f%%);先测临时key再改生产key",
				best.Name, best.Ratio, cur, AIUpstreamGroupMinSaveRatio*100,
			),
			Confidence: 0.86,
		})
		have[acc.ID] = true
		injected++
	}
	return injected
}

// --- sub2api panel helpers ---

type sub2apiPanelGroup struct {
	ID             int64
	Name           string
	RateMultiplier float64
	Platform       string
}

func (p *AIPilotService) ensureSub2APIPanelJWT(ctx context.Context, acc *Account, base string) (string, error) {
	tok := extraString(acc.Extra, ExtraUpstreamPanelAccessToken)
	exp := extraString(acc.Extra, ExtraUpstreamPanelTokenExpiresAt)
	if tok != "" {
		if exp == "" {
			return tok, nil
		}
		if t, err := time.Parse(time.RFC3339, exp); err == nil && time.Until(t) > 2*time.Minute {
			return tok, nil
		}
	}
	refresh := extraString(acc.Extra, ExtraUpstreamPanelRefreshToken)
	if refresh != "" {
		if access, newRefresh, expiresAt, err := p.sub2apiPanelRefresh(ctx, base, refresh); err == nil && access != "" {
			p.cacheSub2APIPanelTokens(ctx, acc, access, newRefresh, expiresAt)
			return access, nil
		}
	}
	email := extraString(acc.Extra, ExtraUpstreamPanelEmail)
	pass := extraString(acc.Extra, ExtraUpstreamPanelPassword)
	if email == "" || pass == "" {
		return "", fmt.Errorf("sub2api 面板登录需要 email+password（或未过期 access_token）")
	}
	access, newRefresh, expiresAt, err := p.sub2apiPanelLogin(ctx, base, email, pass)
	if err != nil {
		return "", err
	}
	p.cacheSub2APIPanelTokens(ctx, acc, access, newRefresh, expiresAt)
	return access, nil
}

func (p *AIPilotService) cacheSub2APIPanelTokens(ctx context.Context, acc *Account, access, refresh, expiresAt string) {
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	persist := map[string]any{}
	acc.Extra[ExtraUpstreamPanelAccessToken] = access
	persist[ExtraUpstreamPanelAccessToken] = access
	if refresh != "" {
		acc.Extra[ExtraUpstreamPanelRefreshToken] = refresh
		persist[ExtraUpstreamPanelRefreshToken] = refresh
	}
	if expiresAt != "" {
		acc.Extra[ExtraUpstreamPanelTokenExpiresAt] = expiresAt
		persist[ExtraUpstreamPanelTokenExpiresAt] = expiresAt
	} else {
		exp := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
		acc.Extra[ExtraUpstreamPanelTokenExpiresAt] = exp
		persist[ExtraUpstreamPanelTokenExpiresAt] = exp
	}
	if p.Accounts != nil {
		_ = p.Accounts.UpdateExtra(ctx, acc.ID, persist)
	}
}

func (p *AIPilotService) sub2apiPanelLogin(ctx context.Context, base, email, password string) (access, refresh, expiresAt string, err error) {
	payload, _ := json.Marshal(map[string]string{"email": email, "password": password})
	for _, path := range []string{"/api/v1/auth/login", "/auth/login"} {
		st, body, e := p.sub2apiPanelDo(ctx, base, "", http.MethodPost, path, payload)
		if e != nil {
			err = e
			continue
		}
		if st != 200 {
			low := strings.ToLower(string(body))
			if strings.Contains(low, "captcha") || strings.Contains(low, "turnstile") || strings.Contains(low, "2fa") || strings.Contains(low, "totp") {
				return "", "", "", fmt.Errorf("面板登录需要验证码/2FA（暂不支持）: %s", truncateForErr(body, 160))
			}
			err = fmt.Errorf("login HTTP %d: %s", st, truncateForErr(body, 180))
			continue
		}
		access, refresh, expiresAt = parseSub2APIAuthTokens(body)
		if access != "" {
			return access, refresh, expiresAt, nil
		}
		err = fmt.Errorf("login 响应无 access_token")
	}
	if err == nil {
		err = fmt.Errorf("login failed")
	}
	return "", "", "", err
}

func (p *AIPilotService) sub2apiPanelRefresh(ctx context.Context, base, refreshToken string) (access, refresh, expiresAt string, err error) {
	payload, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	for _, path := range []string{"/api/v1/auth/refresh", "/auth/refresh"} {
		st, body, e := p.sub2apiPanelDo(ctx, base, "", http.MethodPost, path, payload)
		if e != nil || st != 200 {
			continue
		}
		access, refresh, expiresAt = parseSub2APIAuthTokens(body)
		if access != "" {
			if refresh == "" {
				refresh = refreshToken
			}
			return access, refresh, expiresAt, nil
		}
	}
	return "", "", "", fmt.Errorf("refresh token failed")
}

func parseSub2APIAuthTokens(body []byte) (access, refresh, expiresAt string) {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return "", "", ""
	}
	data := root
	if d, ok := root["data"].(map[string]any); ok {
		data = d
	}
	access = strAny(data["access_token"])
	if access == "" {
		access = strAny(data["token"])
	}
	refresh = strAny(data["refresh_token"])
	expiresAt = strAny(data["expires_at"])
	if expiresAt == "" {
		if sec := anyToFloat64(data["expires_in"]); sec > 0 {
			expiresAt = time.Now().UTC().Add(time.Duration(sec) * time.Second).Format(time.RFC3339)
		}
	}
	return access, refresh, expiresAt
}

func (p *AIPilotService) sub2apiListAvailableGroups(ctx context.Context, base, jwt string) ([]sub2apiPanelGroup, error) {
	for _, path := range []string{"/api/v1/groups/available", "/groups/available"} {
		st, body, err := p.sub2apiPanelDo(ctx, base, jwt, http.MethodGet, path, nil)
		if err != nil || st != 200 {
			continue
		}
		groups := parseSub2APIAvailableGroups(body)
		if len(groups) > 0 {
			return groups, nil
		}
	}
	return nil, fmt.Errorf("无法拉取 sub2api available groups")
}

func parseSub2APIAvailableGroups(body []byte) []sub2apiPanelGroup {
	var root any
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	var arr []any
	switch v := root.(type) {
	case map[string]any:
		if d, ok := v["data"].([]any); ok {
			arr = d
		} else if d, ok := v["data"].(map[string]any); ok {
			if items, ok := d["items"].([]any); ok {
				arr = items
			}
		}
	case []any:
		arr = v
	}
	out := make([]sub2apiPanelGroup, 0, len(arr))
	for _, it := range arr {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		id := int64(anyToFloat64(m["id"]))
		if id <= 0 {
			continue
		}
		g := sub2apiPanelGroup{
			ID: id, Name: strAny(m["name"]),
			RateMultiplier: anyToFloat64(m["rate_multiplier"]),
			Platform:       strAny(m["platform"]),
		}
		if g.RateMultiplier <= 0 {
			g.RateMultiplier = 1
		}
		out = append(out, g)
	}
	return out
}

func (p *AIPilotService) sub2apiFindKey(ctx context.Context, base, jwt, apiKey string, acc *Account) (keyID int64, groupID int64, err error) {
	if cached := int64(extraFloat(acc.Extra, ExtraUpstreamPanelKeyID)); cached > 0 {
		keyID = cached
	}
	for page := 1; page <= 5; page++ {
		path := fmt.Sprintf("/api/v1/keys?page=%d&page_size=50", page)
		st, body, e := p.sub2apiPanelDo(ctx, base, jwt, http.MethodGet, path, nil)
		if e != nil || st != 200 {
			path = fmt.Sprintf("/keys?page=%d&page_size=50", page)
			st, body, e = p.sub2apiPanelDo(ctx, base, jwt, http.MethodGet, path, nil)
			if e != nil || st != 200 {
				continue
			}
		}
		items := parseSub2APIKeyList(body)
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			if keyID > 0 && it.ID == keyID {
				return it.ID, it.GroupID, nil
			}
			if apiKey != "" && keyMatchesSK(apiKey, it.Key) {
				return it.ID, it.GroupID, nil
			}
		}
		if len(items) < 50 {
			break
		}
	}
	if keyID > 0 {
		return keyID, 0, fmt.Errorf("缓存 key_id=%d 未在列表中找到", keyID)
	}
	return 0, 0, fmt.Errorf("面板 key 列表未匹配到本账号 sk")
}

type sub2apiPanelKey struct {
	ID      int64
	Key     string
	Name    string
	GroupID int64
}

func parseSub2APIKeyList(body []byte) []sub2apiPanelKey {
	var root any
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	var arr []any
	switch v := root.(type) {
	case map[string]any:
		if d, ok := v["data"].(map[string]any); ok {
			if items, ok := d["items"].([]any); ok {
				arr = items
			} else if items, ok := d["data"].([]any); ok {
				arr = items
			}
		} else if items, ok := v["data"].([]any); ok {
			arr = items
		} else if items, ok := v["items"].([]any); ok {
			arr = items
		}
	case []any:
		arr = v
	}
	out := make([]sub2apiPanelKey, 0, len(arr))
	for _, it := range arr {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		id := int64(anyToFloat64(m["id"]))
		if id <= 0 {
			continue
		}
		gid := int64(anyToFloat64(m["group_id"]))
		if gid == 0 {
			if g, ok := m["group"].(map[string]any); ok {
				gid = int64(anyToFloat64(g["id"]))
			}
		}
		out = append(out, sub2apiPanelKey{ID: id, Key: strAny(m["key"]), Name: strAny(m["name"]), GroupID: gid})
	}
	return out
}

func (p *AIPilotService) sub2apiCreateKey(ctx context.Context, base, jwt, name string, groupID int64) (id int64, key string, err error) {
	payload, _ := json.Marshal(map[string]any{"name": name, "group_id": groupID})
	for _, path := range []string{"/api/v1/keys", "/keys"} {
		st, body, e := p.sub2apiPanelDo(ctx, base, jwt, http.MethodPost, path, payload)
		if e != nil {
			err = e
			continue
		}
		if st != 200 && st != 201 {
			err = fmt.Errorf("create key HTTP %d: %s", st, truncateForErr(body, 160))
			continue
		}
		id, key = parseSub2APICreatedKey(body)
		if id > 0 && key != "" {
			return id, key, nil
		}
		err = fmt.Errorf("create key 响应缺 id/key: %s", truncateForErr(body, 160))
	}
	if err == nil {
		err = fmt.Errorf("create key failed")
	}
	return 0, "", err
}

func parseSub2APICreatedKey(body []byte) (id int64, key string) {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return 0, ""
	}
	data := root
	if d, ok := root["data"].(map[string]any); ok {
		data = d
	}
	id = int64(anyToFloat64(data["id"]))
	key = strAny(data["key"])
	return id, key
}

func (p *AIPilotService) sub2apiDeleteKey(ctx context.Context, base, jwt string, id int64) error {
	if id <= 0 {
		return nil
	}
	for _, path := range []string{
		"/api/v1/keys/" + strconv.FormatInt(id, 10),
		"/keys/" + strconv.FormatInt(id, 10),
	} {
		st, _, err := p.sub2apiPanelDo(ctx, base, jwt, http.MethodDelete, path, nil)
		if err == nil && (st == 200 || st == 204) {
			return nil
		}
	}
	return fmt.Errorf("delete key %d failed", id)
}

func (p *AIPilotService) sub2apiUpdateKeyGroup(ctx context.Context, base, jwt string, keyID, groupID int64) error {
	payload, _ := json.Marshal(map[string]any{"group_id": groupID})
	for _, path := range []string{
		"/api/v1/keys/" + strconv.FormatInt(keyID, 10),
		"/keys/" + strconv.FormatInt(keyID, 10),
	} {
		st, body, err := p.sub2apiPanelDo(ctx, base, jwt, http.MethodPut, path, payload)
		if err != nil {
			continue
		}
		if st == 200 {
			return nil
		}
		_ = body
	}
	return fmt.Errorf("update key group failed")
}

func keyMatchesSK(apiKey, listed string) bool {
	apiKey = strings.TrimSpace(apiKey)
	listed = strings.TrimSpace(listed)
	if apiKey == "" || listed == "" {
		return false
	}
	if apiKey == listed {
		return true
	}
	if strings.Contains(listed, "*") {
		want := trailingAlnum(apiKey, 6)
		got := trailingAlnum(listed, 6)
		return want != "" && got != "" && want == got
	}
	if len(apiKey) >= 8 && len(listed) >= 8 {
		return strings.HasSuffix(apiKey, listed[len(listed)-8:]) || strings.HasSuffix(listed, apiKey[len(apiKey)-8:])
	}
	return false
}

func trailingAlnum(s string, n int) string {
	var b strings.Builder
	for i := len(s) - 1; i >= 0 && b.Len() < n; i-- {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		}
	}
	r := []byte(b.String())
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

func (p *AIPilotService) sub2apiPanelDo(ctx context.Context, base, jwt, method, path string, body []byte) (int, []byte, error) {
	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	ctx2, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
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
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", upstreamPanelUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return resp.StatusCode, b, nil
}

// --- new-api helpers ---

func (p *AIPilotService) newAPIListTokens(ctx context.Context, base, mgmt, uid string) ([]OneAPIToken, error) {
	for _, path := range []string{
		"/api/token/?p=0&size=100", "/api/token/?p=1&size=100",
		"/api/token/?page=1&page_size=100", "/api/token/",
	} {
		st, body, err := p.oneAPIDo(ctx, base, mgmt, uid, http.MethodGet, path, nil)
		if err != nil || st != 200 {
			continue
		}
		toks := ParseOneAPITokenList(body)
		if len(toks) > 0 {
			return toks, nil
		}
	}
	return nil, fmt.Errorf("无法拉取 token 列表")
}

func (p *AIPilotService) newAPICreateToken(ctx context.Context, base, mgmt, uid, name, group string) (id int, key string, err error) {
	payload, _ := json.Marshal(map[string]any{
		"name": name, "group": group, "unlimited_quota": true,
		"expired_time": -1, "remain_quota": 0,
	})
	st, body, e := p.oneAPIDo(ctx, base, mgmt, uid, http.MethodPost, "/api/token/", payload)
	if e != nil {
		return 0, "", e
	}
	if st != 200 || !oneAPISuccess(body) {
		return 0, "", fmt.Errorf("create token HTTP %d: %s", st, truncateForErr(body, 160))
	}
	// re-list to find by name
	toks, err := p.newAPIListTokens(ctx, base, mgmt, uid)
	if err != nil {
		return 0, "", err
	}
	var found OneAPIToken
	for _, t := range toks {
		if t.Name == name {
			found = t
			break
		}
	}
	if found.ID == "" {
		return 0, "", fmt.Errorf("created token not found in list")
	}
	id = mustAtoiInt(found.ID)
	// reveal key
	st, body, e = p.oneAPIDo(ctx, base, mgmt, uid, http.MethodPost, "/api/token/"+found.ID+"/key", nil)
	if e != nil || st != 200 {
		// some forks return key in list
		if found.Key != "" && !strings.Contains(found.Key, "*") {
			return id, found.Key, nil
		}
		return 0, "", fmt.Errorf("reveal token key failed")
	}
	key = parseNewAPITokenKey(body)
	if key == "" {
		return 0, "", fmt.Errorf("empty token key")
	}
	return id, key, nil
}

func parseNewAPITokenKey(body []byte) string {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	if d, ok := root["data"].(map[string]any); ok {
		if k := strAny(d["key"]); k != "" {
			return k
		}
	}
	return strAny(root["key"])
}

func (p *AIPilotService) newAPIDeleteToken(ctx context.Context, base, mgmt, uid string, id int) error {
	if id <= 0 {
		return nil
	}
	st, _, err := p.oneAPIDo(ctx, base, mgmt, uid, http.MethodDelete, "/api/token/"+strconv.Itoa(id), nil)
	if err != nil {
		return err
	}
	if st != 200 {
		return fmt.Errorf("delete token HTTP %d", st)
	}
	return nil
}

func (p *AIPilotService) newAPIUpdateTokenGroup(ctx context.Context, base, mgmt, uid string, tok OneAPIToken, group string) error {
	payload := map[string]any{
		"id": mustAtoi(tok.ID), "name": tok.Name, "group": group,
		"unlimited_quota": true, "remain_quota": tok.RemainQuota, "expired_time": -1,
	}
	body, _ := json.Marshal(payload)
	st, resp, err := p.oneAPIDo(ctx, base, mgmt, uid, http.MethodPut, "/api/token/", body)
	if err != nil {
		return err
	}
	if st == 200 && oneAPISuccess(resp) {
		return nil
	}
	payload["id"] = tok.ID
	body, _ = json.Marshal(payload)
	st, resp, err = p.oneAPIDo(ctx, base, mgmt, uid, http.MethodPut, "/api/token/", body)
	if err != nil {
		return err
	}
	if st != 200 || !oneAPISuccess(resp) {
		return fmt.Errorf("update token group HTTP %d: %s", st, truncateForErr(resp, 160))
	}
	return nil
}

func matchOneAPIToken(apiKey string, tokens []OneAPIToken) (OneAPIToken, bool) {
	apiKey = strings.TrimSpace(apiKey)
	for _, t := range tokens {
		if apiKey != "" && keyMatchesSK(apiKey, t.Key) {
			return t, true
		}
	}
	if apiKey == "" && len(tokens) == 1 {
		return tokens[0], true
	}
	return OneAPIToken{}, false
}

func oneAPISuccess(body []byte) bool {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return len(body) == 0
	}
	if v, ok := obj["success"].(bool); ok {
		return v
	}
	if c, ok := obj["code"]; ok {
		switch n := c.(type) {
		case float64:
			return n == 0 || n == 200
		case string:
			return n == "0" || n == "success"
		}
	}
	return true
}

func mustAtoi(s string) any {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return s
	}
	return n
}

func mustAtoiInt(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func truncateForErr(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (p *AIPilotService) persistUpstreamGroupSwitch(ctx context.Context, acc *Account, before, after string, newRatio float64, source string) {
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	persist := map[string]any{
		ExtraUpstreamLastGoodGroup:     before,
		ExtraUpstreamCurrentGroup:      after,
		ExtraUpstreamLastGroupSwitchAt: now,
	}
	if before != "" {
		acc.Extra[ExtraUpstreamLastGoodGroup] = before
	}
	acc.Extra[ExtraUpstreamCurrentGroup] = after
	acc.Extra[ExtraUpstreamLastGroupSwitchAt] = now
	// invalidate candidates cache
	delete(acc.Extra, ExtraUpstreamGroupCandidatesJSON)
	delete(acc.Extra, ExtraUpstreamGroupCandidatesAt)
	persist[ExtraUpstreamGroupCandidatesJSON] = ""
	persist[ExtraUpstreamGroupCandidatesAt] = ""
	if newRatio > 0 {
		acc.Extra[ExtraAIRateMultiplier] = newRatio
		acc.Extra[ExtraAIRateSource] = source + "_group_switch"
		acc.Extra[ExtraAIRateCheckedAt] = now
		persist[ExtraAIRateMultiplier] = newRatio
		persist[ExtraAIRateSource] = source + "_group_switch"
		persist[ExtraAIRateCheckedAt] = now
	}
	if p.Accounts != nil {
		_ = p.Accounts.UpdateExtra(ctx, acc.ID, persist)
	}
}

// gateSwitchUpstreamGroupReason is apply-time conservative gate.
func gateSwitchUpstreamGroupReason(acc *Account, recent AccountTrafficStats, cfg AIAutopilotSettings) string {
	if acc == nil {
		return "账号不存在"
	}
	if !cfg.OpAllowed(AIOpSwitchUpstreamGroup) {
		return "动作权限已关闭: switch_upstream_group"
	}
	if !accountUpstreamGroupSwitchEnabled(acc) {
		return "账号未开启 ai_upstream_group_switch"
	}
	if in, rem := upstreamGroupSwitchInDwell(acc, time.Now()); in {
		return fmt.Sprintf("切组驻留期内,剩余约 %.0fm", rem.Minutes()+0.5)
	}
	if recentWindowHardFail(recent) {
		return "近窗硬失败,禁止切更便宜组;请先 set_priority/set_weight 或 disable"
	}
	return ""
}
