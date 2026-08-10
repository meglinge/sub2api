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
// CF/captcha login is out of scope for v1 — plain email+password only.
const (
	ExtraAIUpstreamGroupSwitch       = "ai_upstream_group_switch" // bool, default false
	ExtraUpstreamPanelEmail          = "upstream_panel_email"
	ExtraUpstreamPanelPassword       = "upstream_panel_password"
	ExtraUpstreamPanelAccessToken    = "upstream_panel_access_token"
	ExtraUpstreamPanelRefreshToken   = "upstream_panel_refresh_token"
	ExtraUpstreamPanelTokenExpiresAt = "upstream_panel_token_expires_at"
	ExtraUpstreamPanelKeyID          = "upstream_panel_key_id" // cached matched key id (sub2api)
	ExtraUpstreamCurrentGroup        = "upstream_current_group"
	ExtraUpstreamLastGoodGroup       = "upstream_last_good_group"
	ExtraUpstreamLastGroupSwitchAt   = "upstream_last_group_switch_at"

	// AIUpstreamGroupSwitchDwell blocks thrash after a successful switch.
	AIUpstreamGroupSwitchDwell = 30 * time.Minute
	// AIUpstreamGroupMinSaveRatio: target must be at least this cheaper (ratio lower).
	AIUpstreamGroupMinSaveRatio = 0.15
)

// UpstreamGroupCandidate is a switchable billing group on the upstream panel.
type UpstreamGroupCandidate struct {
	ID     string  `json:"id,omitempty"` // sub2api numeric id as string
	Name   string  `json:"name"`         // new-api group name or sub2api group name
	Ratio  float64 `json:"ratio"`        // group_ratio / rate_multiplier
	Source string  `json:"source"`       // newapi|sub2api
}

// UpstreamGroupView is attached to pilot channel snapshots.
type UpstreamGroupView struct {
	Switchable        bool                     `json:"switchable"`
	SwitchEnabled     bool                     `json:"switchEnabled"`
	Kind              string                   `json:"kind,omitempty"`
	Current           string                   `json:"current,omitempty"`
	CurrentRatio      float64                  `json:"currentRatio,omitempty"`
	Candidates        []UpstreamGroupCandidate `json:"candidates,omitempty"`
	DwellRemainingSec int                      `json:"dwellRemainingSec,omitempty"`
	Reason            string                   `json:"reason,omitempty"`
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
	if acc == nil || now.IsZero() {
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

// buildUpstreamGroupView is a lightweight snapshot helper (no network when not switch-enabled).
func buildUpstreamGroupView(acc *Account) UpstreamGroupView {
	v := UpstreamGroupView{}
	if acc == nil {
		v.Reason = "no account"
		return v
	}
	v.SwitchEnabled = accountUpstreamGroupSwitchEnabled(acc)
	kind := strings.ToLower(extraString(acc.Extra, ExtraUpstreamKind))
	if kind == "oneapi" {
		kind = "newapi"
	}
	v.Kind = kind
	v.Current = extraString(acc.Extra, ExtraUpstreamCurrentGroup)
	if r := extraFloat(acc.Extra, ExtraAIRateMultiplier); r > 0 {
		v.CurrentRatio = r
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
	case kind == "newapi" || kind == "":
		if mgmt == "" && kind == "newapi" {
			v.Reason = "newapi 缺少 upstream_mgmt_token"
			return v
		}
		if mgmt != "" {
			v.Switchable = true
			v.Kind = "newapi"
			return v
		}
	case kind == "sub2api":
		if email == "" || pass == "" {
			if tok == "" {
				v.Reason = "sub2api 需要面板邮箱+密码（或 access_token）"
				return v
			}
		}
		v.Switchable = true
		return v
	default:
		v.Reason = "upstream_kind 不支持切组: " + kind
		return v
	}
	if in, rem := upstreamGroupSwitchInDwell(acc, time.Now()); in {
		v.DwellRemainingSec = int(rem.Seconds())
	}
	return v
}

// SwitchUpstreamGroup applies a group switch on the remote panel and updates local extras.
// value: new-api group name, or sub2api group id / name.
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
	kind := strings.ToLower(extraString(acc.Extra, ExtraUpstreamKind))
	if kind == "oneapi" {
		kind = "newapi"
	}
	base := strings.TrimRight(strings.TrimSpace(acc.GetOpenAIBaseURL()), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(acc.GetCredential("base_url")), "/")
	}
	if base == "" {
		return "", "", fmt.Errorf("账号缺少 base_url")
	}
	apiKey := strings.TrimSpace(acc.GetOpenAIApiKey())
	if apiKey != "" && strings.Contains(apiKey, "*") {
		apiKey = ""
	}

	// Prefer explicit kind; empty + mgmt → newapi; empty + panel → sub2api.
	if kind == "" {
		if extraString(acc.Extra, ExtraUpstreamMgmtToken) != "" {
			kind = "newapi"
		} else if extraString(acc.Extra, ExtraUpstreamPanelEmail) != "" || extraString(acc.Extra, ExtraUpstreamPanelAccessToken) != "" {
			kind = "sub2api"
		}
	}

	switch kind {
	case "newapi":
		return p.switchNewAPITokenGroup(ctx, acc, base, apiKey, value)
	case "sub2api":
		return p.switchSub2APIKeyGroup(ctx, acc, base, apiKey, value)
	default:
		return "", "", fmt.Errorf("不支持的 upstream_kind=%s", kind)
	}
}

func (p *AIPilotService) switchNewAPITokenGroup(ctx context.Context, acc *Account, base, apiKey, targetGroup string) (before, after string, err error) {
	mgmt := extraString(acc.Extra, ExtraUpstreamMgmtToken)
	uid := extraString(acc.Extra, ExtraUpstreamMgmtUserID)
	if mgmt == "" {
		return "", "", fmt.Errorf("newapi 缺少 upstream_mgmt_token")
	}
	// List groups for ratio map + validation.
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
		return "", "", fmt.Errorf("无法拉取 newapi 分组列表")
	}
	var targetRatio float64
	found := false
	for _, g := range groups {
		if strings.EqualFold(g.Name, targetGroup) {
			targetGroup = g.Name
			targetRatio = g.Ratio
			found = true
			break
		}
	}
	if !found {
		return "", "", fmt.Errorf("目标组 %q 不在上游可选列表", targetGroup)
	}

	// Find token matching api key.
	var tokens []OneAPIToken
	for _, path := range []string{
		"/api/token/?p=0&size=100",
		"/api/token/?p=1&size=100",
		"/api/token/?page=1&page_size=100",
		"/api/token/",
	} {
		st, body, e := p.oneAPIDo(ctx, base, mgmt, uid, http.MethodGet, path, nil)
		if e != nil || st != 200 {
			continue
		}
		tokens = ParseOneAPITokenList(body)
		if len(tokens) > 0 {
			break
		}
	}
	if len(tokens) == 0 {
		return "", "", fmt.Errorf("无法拉取 newapi token 列表")
	}
	tok, ok := matchOneAPIToken(apiKey, tokens)
	if !ok {
		return "", "", fmt.Errorf("token 列表中未匹配到本账号 sk")
	}
	before = strings.TrimSpace(tok.Group)
	if before == "" {
		before = extraString(acc.Extra, ExtraUpstreamCurrentGroup)
	}
	if strings.EqualFold(before, targetGroup) {
		return before, targetGroup, nil
	}

	// PUT /api/token/ — send id + fields; group is the critical one.
	payload := map[string]any{
		"id":                   mustAtoi(tok.ID),
		"name":                 tok.Name,
		"group":                targetGroup,
		"unlimited_quota":      true,
		"remain_quota":         tok.RemainQuota,
		"expired_time":         -1,
		"cross_group_retry":    false,
		"model_limits_enabled": false,
	}
	// Prefer numeric id when possible; some forks want string — retry with string id.
	body, _ := json.Marshal(payload)
	st, respBody, e := p.oneAPIDo(ctx, base, mgmt, uid, http.MethodPut, "/api/token/", body)
	if e != nil {
		return before, "", e
	}
	if st != 200 || !oneAPISuccess(respBody) {
		// retry with string id
		payload["id"] = tok.ID
		body, _ = json.Marshal(payload)
		st, respBody, e = p.oneAPIDo(ctx, base, mgmt, uid, http.MethodPut, "/api/token/", body)
		if e != nil {
			return before, "", e
		}
		if st != 200 || !oneAPISuccess(respBody) {
			return before, "", fmt.Errorf("newapi 改组失败 HTTP %d: %s", st, truncateForErr(respBody, 200))
		}
	}
	after = targetGroup
	p.persistUpstreamGroupSwitch(ctx, acc, before, after, targetRatio, "newapi")
	return before, after, nil
}

func (p *AIPilotService) switchSub2APIKeyGroup(ctx context.Context, acc *Account, base, apiKey, target string) (before, after string, err error) {
	jwt, err := p.ensureSub2APIPanelJWT(ctx, acc, base)
	if err != nil {
		return "", "", err
	}
	// Available groups
	groups, err := p.sub2apiListAvailableGroups(ctx, base, jwt)
	if err != nil {
		return "", "", err
	}
	var targetID int64
	var targetName string
	var targetRatio float64
	// target can be id or name
	if id, e := strconv.ParseInt(target, 10, 64); e == nil {
		for _, g := range groups {
			if g.ID == id {
				targetID, targetName, targetRatio = g.ID, g.Name, g.RateMultiplier
				break
			}
		}
	}
	if targetID == 0 {
		for _, g := range groups {
			if strings.EqualFold(g.Name, target) {
				targetID, targetName, targetRatio = g.ID, g.Name, g.RateMultiplier
				break
			}
		}
	}
	if targetID == 0 {
		return "", "", fmt.Errorf("目标组 %q 不在 available groups", target)
	}

	keyID, curGID, err := p.sub2apiFindKey(ctx, base, jwt, apiKey, acc)
	if err != nil {
		return "", "", err
	}
	if curGID == targetID {
		before = strconv.FormatInt(curGID, 10)
		return before, before, nil
	}
	before = strconv.FormatInt(curGID, 10)
	if curGID == 0 {
		before = extraString(acc.Extra, ExtraUpstreamCurrentGroup)
	}

	// PUT /api/v1/keys/:id
	payload, _ := json.Marshal(map[string]any{"group_id": targetID})
	st, body, err := p.sub2apiPanelDo(ctx, base, jwt, http.MethodPut, "/api/v1/keys/"+strconv.FormatInt(keyID, 10), payload)
	if err != nil {
		return before, "", err
	}
	if st != 200 {
		// try without /api/v1 prefix (some deploys mount at root)
		st2, body2, err2 := p.sub2apiPanelDo(ctx, base, jwt, http.MethodPut, "/keys/"+strconv.FormatInt(keyID, 10), payload)
		if err2 != nil {
			return before, "", err2
		}
		if st2 != 200 {
			return before, "", fmt.Errorf("sub2api 改组失败 HTTP %d/%d: %s", st, st2, truncateForErr(body, 160)+truncateForErr(body2, 80))
		}
	}
	after = strconv.FormatInt(targetID, 10)
	if targetName != "" {
		after = after + ":" + targetName
	}
	// cache key id
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	acc.Extra[ExtraUpstreamPanelKeyID] = keyID
	p.persistUpstreamGroupSwitch(ctx, acc, before, after, targetRatio, "sub2api")
	return before, after, nil
}

type sub2apiPanelGroup struct {
	ID             int64
	Name           string
	RateMultiplier float64
	Platform       string
}

func (p *AIPilotService) ensureSub2APIPanelJWT(ctx context.Context, acc *Account, base string) (string, error) {
	// Reuse cached access token if not expired (leave 2m skew).
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
	// Try refresh
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
		// default 1h if unknown
		exp := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
		acc.Extra[ExtraUpstreamPanelTokenExpiresAt] = exp
		persist[ExtraUpstreamPanelTokenExpiresAt] = exp
	}
	if p.Accounts != nil {
		_ = p.Accounts.UpdateExtra(ctx, acc.ID, persist)
	}
}

func (p *AIPilotService) sub2apiPanelLogin(ctx context.Context, base, email, password string) (access, refresh, expiresAt string, err error) {
	payload, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": password,
	})
	for _, path := range []string{"/api/v1/auth/login", "/auth/login"} {
		st, body, e := p.sub2apiPanelDo(ctx, base, "", http.MethodPost, path, payload)
		if e != nil {
			err = e
			continue
		}
		if st != 200 {
			err = fmt.Errorf("login HTTP %d: %s", st, truncateForErr(body, 180))
			// captcha / 2fa hints
			low := strings.ToLower(string(body))
			if strings.Contains(low, "captcha") || strings.Contains(low, "turnstile") || strings.Contains(low, "2fa") || strings.Contains(low, "totp") {
				return "", "", "", fmt.Errorf("面板登录需要验证码/2FA（暂不支持）: %s", truncateForErr(body, 160))
			}
			continue
		}
		access, refresh, expiresAt = parseSub2APIAuthTokens(body)
		if access != "" {
			return access, refresh, expiresAt, nil
		}
		err = fmt.Errorf("login 响应无 access_token: %s", truncateForErr(body, 160))
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
	// envelope: {code, data:{access_token,refresh_token,expires_at}} or flat
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
		// expires_in seconds
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
	// unwrap data
	var arr []any
	switch v := root.(type) {
	case map[string]any:
		if d, ok := v["data"].([]any); ok {
			arr = d
		} else if d, ok := v["data"].(map[string]any); ok {
			// maybe {items:[]}
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
			ID:             id,
			Name:           strAny(m["name"]),
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
	// cached id
	if cached := int64(extraFloat(acc.Extra, ExtraUpstreamPanelKeyID)); cached > 0 {
		// still verify via list or get
		keyID = cached
	}
	// list pages
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
	return 0, 0, fmt.Errorf("面板 key 列表未匹配到本账号 sk（请确认 sk 完整且属于该面板用户）")
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
		out = append(out, sub2apiPanelKey{
			ID:      id,
			Key:     strAny(m["key"]),
			Name:    strAny(m["name"]),
			GroupID: gid,
		})
	}
	return out
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
	// Masked listing (sk-****abcd): compare trailing alnum of both.
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
	rev := b.String()
	// reverse
	r := []byte(rev)
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
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	url := strings.TrimRight(base, "/") + path
	req, err := http.NewRequestWithContext(ctx2, method, url, rdr)
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
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return resp.StatusCode, b, nil
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

func matchOneAPIToken(apiKey string, tokens []OneAPIToken) (OneAPIToken, bool) {
	apiKey = strings.TrimSpace(apiKey)
	for _, t := range tokens {
		if apiKey != "" && keyMatchesSK(apiKey, t.Key) {
			return t, true
		}
		// also try suffix of listed key (often full or masked)
		if apiKey != "" && t.Key != "" && (strings.HasSuffix(apiKey, t.Key) || strings.HasSuffix(t.Key, trailingAlnum(apiKey, 8))) {
			return t, true
		}
	}
	// single token account: if only one enabled token, use it when apiKey empty
	if apiKey == "" && len(tokens) == 1 {
		return tokens[0], true
	}
	enabled := 0
	var only OneAPIToken
	for _, t := range tokens {
		if t.Enabled {
			enabled++
			only = t
		}
	}
	if apiKey == "" && enabled == 1 {
		return only, true
	}
	return OneAPIToken{}, false
}

func oneAPISuccess(body []byte) bool {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		// empty/unknown: treat HTTP 200 as ok
		return len(body) == 0 || strings.TrimSpace(string(body)) == "ok"
	}
	if v, ok := obj["success"].(bool); ok {
		return v
	}
	// some return {code:0} or {data:...}
	if c, ok := obj["code"]; ok {
		switch n := c.(type) {
		case float64:
			return n == 0 || n == 200
		case string:
			return n == "0" || n == "success"
		}
	}
	if _, ok := obj["data"]; ok {
		return true
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

func truncateForErr(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// gateSwitchUpstreamGroupReason is apply-time conservative gate (quality + dwell + enable).
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
	// Conservative: refuse switch-to-cheaper when recent hard-failing.
	if recentWindowHardFail(recent) {
		return "近窗硬失败,禁止切更便宜组;请先 set_priority/set_weight 或 disable"
	}
	return ""
}
