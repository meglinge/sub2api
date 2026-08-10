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

// activationResult is the outcome of one account activation probe.
type activationResult struct {
	AccountID int64  `json:"accountId"`
	Verdict   string `json:"verdict"` // pass | slow | fail | unknown
	TTFBMs    int64  `json:"ttfbMs"`
	Error     string `json:"error,omitempty"`
	Fresh     bool   `json:"fresh"`
	Reason    string `json:"reason,omitempty"`
	// Source: upstream = hit account credentials; control_plane = pilot LLM only.
	Source string `json:"source,omitempty"`
}

// probeRequest is what the model asks for in multi-turn analysis.
type probeRequest struct {
	AccountID int64  `json:"accountId"`
	Reason    string `json:"reason"`
	Juice     bool   `json:"juice"`
}

// activationNeeded marks accounts whose traffic stats cannot prove health
// (AI-disabled, soft-stopped weight, or long-window idle).
func activationNeeded(acc *Account, idle bool) bool {
	if acc == nil {
		return false
	}
	return acc.AIDisabled || acc.EffectiveScheduleWeight() <= 0 || idle
}

// activationView is the snapshot block the model reads before enable/lift-weight.
func activationView(acc *Account, res activationResult, idle bool, cfg AIAutopilotSettings) map[string]any {
	needed := activationNeeded(acc, idle)
	out := map[string]any{
		"needed":  needed,
		"verdict": "unknown",
		"note":    "无现场探测时不要把空白流量当成健康",
	}
	if !needed {
		out["note"] = "有流量/未停用,历史统计可用;探测非必须"
		return out
	}
	if res.AccountID == acc.ID && res.Fresh {
		out["verdict"] = res.Verdict
		out["ttfbMs"] = res.TTFBMs
		out["fresh"] = true
		if res.Source != "" {
			out["source"] = res.Source
		}
		if res.Error != "" {
			out["error"] = truncateStr(res.Error, 200)
		}
		if res.Reason != "" {
			out["reason"] = res.Reason
		}
		switch res.Verdict {
		case "pass", "slow":
			if acc.AIDisabled {
				out["note"] = "探测通过且当前 aiDisabled:应 enable 恢复;blank traffic ≠ 仍坏"
			} else {
				out["note"] = "探测通过,可考虑抬权/恢复分流"
			}
		case "fail":
			out["note"] = "探测失败:禁止 enable/从 0 抬权"
		default:
			out["note"] = "探测未知:禁止 enable/从 0 抬权"
		}
		return out
	}
	if cfg.ActivationProbeOn() {
		out["note"] = "需要探测但本轮无新鲜结果;禁止 enable/从 0 抬权"
	}
	return out
}

// collectRecoveryProbeRequests lists accounts that need pre-snapshot probes.
// AI-disabled first (recovery path), then weight=0, then long-window idle.
// Only OpenAI API-key accounts — oauth mass must never enter the probe queue.
func collectRecoveryProbeRequests(accounts []Account, traffic map[int64]AccountTrafficStats, max int) []probeRequest {
	if max <= 0 {
		max = 6
	}
	if max > 8 {
		max = 8
	}
	var disabled, softStop, idle []probeRequest
	for i := range accounts {
		acc := &accounts[i]
		if !acc.IsOpenAIApiKey() {
			continue
		}
		st := traffic[acc.ID]
		isIdle := st.Requests+st.Errors == 0
		if acc.AIDisabled {
			disabled = append(disabled, probeRequest{AccountID: acc.ID, Reason: "ai_disabled recovery probe"})
			continue
		}
		if acc.EffectiveScheduleWeight() <= 0 {
			softStop = append(softStop, probeRequest{AccountID: acc.ID, Reason: "weight=0 recovery probe"})
			continue
		}
		if isIdle {
			idle = append(idle, probeRequest{AccountID: acc.ID, Reason: "idle recovery probe"})
		}
	}
	out := make([]probeRequest, 0, max)
	for _, list := range [][]probeRequest{disabled, softStop, idle} {
		for _, req := range list {
			if len(out) >= max {
				return out
			}
			out = append(out, req)
		}
	}
	return out
}

// runActivationProbes probes accounts for enable gates and snapshot activation.
//
// Only hits the account's own upstream credentials (API-key /v1/responses).
// NEVER falls back to the pilot LLM control-plane (CCH chat/messages) — that
// path flooded claude-code-hub with abort-499s and made every analyze cycle
// multi-minute.
func (p *AIPilotService) runActivationProbes(
	ctx context.Context,
	cfg AIAutopilotSettings,
	reqs []probeRequest,
	accountsByID map[int64]*Account,
	nameByID map[int64]string,
) []activationResult {
	if !cfg.ActivationProbeOn() || len(reqs) == 0 {
		return nil
	}
	max := cfg.ActivationProbeMaxPerRun
	if max <= 0 {
		max = 6
	}
	if max > 8 {
		max = 8 // hard cap: sequential probes must not dominate the cycle
	}
	timeout := time.Duration(cfg.ActivationProbeTimeoutSeconds) * time.Second
	if timeout < time.Second {
		timeout = 8 * time.Second
	}
	if timeout > 12*time.Second {
		timeout = 12 * time.Second
	}
	_ = nameByID // reserved for logging; probes never go via control-plane LLM
	out := make([]activationResult, 0, len(reqs))
	seen := map[int64]bool{}
	for _, req := range reqs {
		if req.AccountID <= 0 || seen[req.AccountID] {
			continue
		}
		seen[req.AccountID] = true
		if len(out) >= max {
			out = append(out, activationResult{
				AccountID: req.AccountID, Verdict: "unknown",
				Error: "超过单轮探测上限", Reason: req.Reason, Fresh: true,
			})
			continue
		}
		acc := accountsByID[req.AccountID]
		if acc == nil {
			out = append(out, activationResult{
				AccountID: req.AccountID, Verdict: "unknown", Fresh: true,
				Error: "账号不在本轮管理集", Reason: req.Reason, Source: "skipped",
			})
			continue
		}
		// Skip oauth/subscription mass — no reliable sk probe path; don't spam CCH.
		if !acc.IsOpenAIApiKey() {
			out = append(out, activationResult{
				AccountID: req.AccountID, Verdict: "unknown", Fresh: true,
				Error: "非 apikey 账号跳过上游探测(不走 control-plane LLM)", Reason: req.Reason, Source: "skipped",
			})
			continue
		}
		if up, ok := p.probeViaUpstream(ctx, acc, timeout, req.Reason, cfg); ok {
			out = append(out, up)
			continue
		}
		out = append(out, activationResult{
			AccountID: req.AccountID, Verdict: "unknown", Fresh: true,
			Error: "上游探测不可用(无密钥/路径)", Reason: req.Reason, Source: "upstream",
		})
	}
	return out
}

// probeViaUpstream hits the account's own upstream with a tiny request.
//
// Pool vendors run Codex/Responses: never fall back to /v1/chat/completions
// (messages) when openai_responses_supported is true/unknown — that path caused
// multi-model retries and slow "messages" spam on 中转站 logs.
// Chat is only used when the account explicitly marks responses unsupported.
func (p *AIPilotService) probeViaUpstream(
	ctx context.Context,
	acc *Account,
	timeout time.Duration,
	reason string,
	cfg AIAutopilotSettings,
) (activationResult, bool) {
	res := activationResult{AccountID: acc.ID, Reason: reason, Fresh: true, Source: "upstream"}
	if acc == nil || !acc.IsOpenAI() {
		return res, false
	}
	// API-key path is reliable; OAuth/subscription often needs special hosts.
	if !acc.IsOpenAIApiKey() {
		return res, false
	}
	apiKey := strings.TrimSpace(acc.GetOpenAIApiKey())
	if apiKey == "" || strings.Contains(apiKey, "*") {
		return res, false
	}
	base := strings.TrimRight(strings.TrimSpace(acc.GetOpenAIBaseURL()), "/")
	if base == "" {
		base = "https://api.openai.com"
	}
	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	// Account mapping / probe_model only — do NOT GET /v1/models (slow + wrong catalog).
	models := probeModelCandidates(acc)
	if len(models) > 2 {
		models = models[:2]
	}

	// Default to Responses for OpenAI pool; only force chat when probe said unsupported.
	useResponses := true
	if acc.Extra != nil {
		if v, ok := acc.Extra["openai_responses_supported"].(bool); ok && !v {
			useResponses = false
		}
	}

	type probePath struct {
		name string
		path string
		body func(model string) map[string]any
	}
	responsesBody := func(model string) map[string]any {
		// Minimal Responses health ping (no tools). String input is widely accepted.
		return map[string]any{
			"model":             model,
			"input":             "ping",
			"max_output_tokens": 16,
			"stream":            false,
		}
	}
	chatBody := func(model string) map[string]any {
		return map[string]any{
			"model": model,
			"messages": []map[string]string{
				{"role": "user", "content": "ping"},
			},
			"max_tokens":  1,
			"temperature": 0,
			"stream":      false,
		}
	}

	var paths []probePath
	if useResponses {
		// Responses only — no chat/messages fallback for Codex pools.
		paths = []probePath{{"responses", "/responses", responsesBody}}
	} else {
		paths = []probePath{{"chat", "/chat/completions", chatBody}}
	}

	// Per-attempt cap so a hung relay cannot burn the whole analyze cycle.
	attemptTO := timeout
	if attemptTO <= 0 || attemptTO > 8*time.Second {
		attemptTO = 8 * time.Second
	}

	var lastErr string
	var lastTTFB int64
	for _, pe := range paths {
		url := joinOpenAIURL(base, pe.path)
		for _, model := range models {
			if model == "" {
				continue
			}
			raw, _ := json.Marshal(pe.body(model))
			reqCtx, cancel := context.WithTimeout(ctx, attemptTO)
			httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(raw))
			if err != nil {
				cancel()
				res.Verdict = "fail"
				res.Error = err.Error()
				return res, true
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
			httpReq.Header.Set("User-Agent", "Mozilla/5.0 (compatible; sub2api-activation-probe/1.0)")
			start := time.Now()
			resp, err := client.Do(httpReq)
			ttfb := time.Since(start).Milliseconds()
			lastTTFB = ttfb
			if err != nil {
				cancel()
				lastErr = fmt.Sprintf("%s model=%s: %v", pe.name, model, err)
				// Timeout / network: try next model once, do not switch to chat.
				if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline") {
					continue
				}
				res.TTFBMs = ttfb
				res.Verdict = "fail"
				res.Error = lastErr
				return res, true
			}
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			cancel()
			if resp.StatusCode >= 300 {
				msg := fmt.Sprintf("HTTP %d %s model=%s: %s", resp.StatusCode, pe.name, model, truncateStr(string(respBody), 160))
				lastErr = msg
				if isModelNotFoundBody(string(respBody)) || resp.StatusCode == 400 || resp.StatusCode == 404 {
					continue // next model
				}
				res.TTFBMs = ttfb
				res.Verdict = "fail"
				res.Error = msg
				return res, true
			}
			res.TTFBMs = ttfb
			if cfg.ActivationProbeMaxTtfbMs > 0 && ttfb > int64(cfg.ActivationProbeMaxTtfbMs) {
				res.Verdict = "slow"
				res.Reason = fmt.Sprintf("%s via=%s model=%s", reason, pe.name, model)
				return res, true
			}
			res.Verdict = "pass"
			res.Reason = fmt.Sprintf("%s via=%s model=%s", reason, pe.name, model)
			return res, true
		}
	}
	res.TTFBMs = lastTTFB
	res.Verdict = "fail"
	if lastErr == "" {
		lastErr = "no usable probe model"
	}
	res.Error = lastErr
	return res, true
}

func joinOpenAIURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + path
	}
	return base + "/v1" + path
}

func probeModelCandidates(acc *Account) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			return
		}
		// Skip clearly non-chat models.
		low := strings.ToLower(m)
		if strings.Contains(low, "embed") || strings.Contains(low, "whisper") || strings.Contains(low, "tts") || strings.Contains(low, "dall-e") || strings.Contains(low, "image") {
			return
		}
		seen[m] = true
		out = append(out, m)
	}
	if acc != nil {
		add(acc.GetExtraString("probe_model"))
		add(acc.GetExtraString("default_model"))
		add(acc.GetExtraString("test_model"))
		// Model mapping values are what the upstream actually accepts.
		for _, v := range acc.GetModelMapping() {
			add(v)
		}
		for k := range acc.GetModelMapping() {
			add(k)
		}
	}
	// Prefer models that match real pool traffic (Responses/Codex). Keep short.
	for _, m := range []string{
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.4", "gpt-5",
	} {
		add(m)
	}
	return out
}

func mergeProbeModels(preferred, fallback []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(preferred)+len(fallback))
	for _, m := range preferred {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		low := strings.ToLower(m)
		if strings.Contains(low, "embed") || strings.Contains(low, "whisper") || strings.Contains(low, "tts") || strings.Contains(low, "dall-e") || strings.Contains(low, "image") {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	for _, m := range fallback {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	// Cap attempts so a huge catalog doesn't stall the analyze cycle.
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func isModelNotFoundBody(body string) bool {
	low := strings.ToLower(body)
	return strings.Contains(low, "model_not_found") ||
		strings.Contains(low, "does not exist") ||
		strings.Contains(low, "not supported") ||
		strings.Contains(low, "unknown model") ||
		strings.Contains(low, "invalid model")
}

// listUpstreamModels returns a few chat-looking model ids from GET /v1/models.
func (p *AIPilotService) listUpstreamModels(ctx context.Context, client *http.Client, base, apiKey string, timeout time.Duration) []string {
	if client == nil {
		return nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, joinOpenAIURL(base, "/models"), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var obj struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return nil
	}
	out := make([]string, 0, 8)
	for _, it := range obj.Data {
		id := strings.TrimSpace(it.ID)
		if id == "" {
			continue
		}
		low := strings.ToLower(id)
		if strings.Contains(low, "embed") || strings.Contains(low, "whisper") || strings.Contains(low, "tts") || strings.Contains(low, "dall-e") || strings.Contains(low, "image") || strings.Contains(low, "moderation") {
			continue
		}
		out = append(out, id)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func (p *AIPilotService) probeViaLLM(
	ctx context.Context,
	cfg AIAutopilotSettings,
	accountID int64,
	accountName, prompt string,
	timeout time.Duration,
	reason string,
) activationResult {
	res := activationResult{AccountID: accountID, Reason: reason, Fresh: true, Source: "control_plane"}
	url := chatCompletionsURL(cfg.BaseURL)
	body := map[string]any{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0,
		"max_tokens":  16,
	}
	raw, _ := json.Marshal(body)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		res.Verdict = "fail"
		res.Error = err.Error()
		return res
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	// Tag which account this probe is for (operators / CCH logs).
	httpReq.Header.Set("X-Autopilot-Account-Id", fmt.Sprintf("%d", accountID))
	if accountName != "" {
		httpReq.Header.Set("X-Autopilot-Account-Name", accountName)
	}
	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	start := time.Now()
	resp, err := client.Do(httpReq)
	res.TTFBMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Verdict = "fail"
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		res.Verdict = "fail"
		res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
		return res
	}
	// Slow is still a pass for enable gating but model should lower confidence.
	if cfg.ActivationProbeMaxTtfbMs > 0 && res.TTFBMs > int64(cfg.ActivationProbeMaxTtfbMs) {
		res.Verdict = "slow"
		return res
	}
	res.Verdict = "pass"
	return res
}

// activationGateReason rejects enable / 0→non-zero weight when probe is required and not fresh-pass.
func activationGateReason(op, value string, beforeWeight int, probes map[int64]activationResult, accountID int64, cfg AIAutopilotSettings) string {
	if !cfg.ActivationProbeOn() {
		return ""
	}
	needs := false
	switch op {
	case AIOpEnable:
		needs = true
	case AIOpSetWeight:
		n := 0
		fmt.Sscanf(strings.TrimSpace(value), "%d", &n)
		if beforeWeight <= 0 && n > 0 {
			needs = true
		}
	}
	if !needs {
		return ""
	}
	pr, ok := probes[accountID]
	if !ok || !pr.Fresh {
		return "启用前探测：无新鲜探测结果,拒绝 enable/抬权"
	}
	switch pr.Verdict {
	case "pass", "slow":
		return ""
	case "fail":
		return "启用前探测失败: " + pr.Error
	default:
		return "启用前探测结果未知,拒绝 enable/抬权"
	}
}

// injectRecoveryEnables appends enable (+ priority un-burial) when the model forgot.
// Breaks: (1) one-way disable ratchet (2) deep exile >200 (3) sticky spare tier 150–200
// death spiral (demote → no recent traffic → demote again).
//
// Soft spare unbury requires: long-window sample evidence AND no recent hard fail
// AND no recent spare demotion within AISoftUnburyDwell (prevents 刚因429沉到150
// 下一轮 soft-unbury 又拉回 100 的 thrash). lastSpareDemotion may be nil (tests).
func injectRecoveryEnables(decision *decision, accounts []Account, probes map[int64]activationResult, longTraffic, recentTraffic map[int64]AccountTrafficStats, cfg AIAutopilotSettings, lastSpareDemotion map[int64]time.Time, now time.Time) int {
	if decision == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	haveEnable := map[int64]bool{}
	havePri := map[int64]bool{}
	for _, a := range decision.Actions {
		if a.Op == AIOpEnable {
			haveEnable[a.AccountID] = true
		}
		if a.Op == AIOpSetPriority {
			havePri[a.AccountID] = true
		}
	}
	injected := 0
	for i := range accounts {
		acc := &accounts[i]
		if !acc.AIDisabled {
			// Monopoly front (priority < 50, e.g. user default 1): strict layering
			// starves everyone else. Auto-clamp to observation tier even if the model
			// forgot or prior set_priority was stuck on cooldown / delta gates.
			if !havePri[acc.ID] && acc.Priority < AIMinPriority && cfg.OpAllowed(AIOpSetPriority) &&
				acc.Status == StatusActive && acc.Schedulable {
				obs := AIObservationPriority
				decision.Actions = append(decision.Actions, decisionAction{
					AccountID: acc.ID,
					Op:        AIOpSetPriority,
					Value:     strconv.Itoa(obs),
					Reason: fmt.Sprintf(
						"自动纠正非法顶层: priority=%d < 自动驾驶下限 %d,严格分层会独占号池;拉到观察层 %d(与默认主层对齐)",
						acc.Priority, AIMinPriority, obs,
					),
					Confidence: 0.95,
				})
				havePri[acc.ID] = true
				injected++
				// Do not continue — account may also need other injects later; havePri set.
			}
			// Deep exile always un-bury.
			if !havePri[acc.ID] && ShouldUnburyPriority(acc.Priority) && cfg.OpAllowed(AIOpSetPriority) {
				obs := RecoveryObservationPriority(acc.Priority)
				decision.Actions = append(decision.Actions, decisionAction{
					AccountID:  acc.ID,
					Op:         AIOpSetPriority,
					Value:      strconv.Itoa(obs),
					Reason:     fmt.Sprintf("自动解埋: priority=%d 超过深埋阈值 %d,提到观察层 %d 以便恢复时可接流量", acc.Priority, AIMaxPriority, obs),
					Confidence: 0.85,
				})
				havePri[acc.ID] = true
				injected++
				continue
			}
			// Sticky spare tier (150–200):
			// 1) classic: long window has real healthy samples + no recent hard fail
			// 2) cheap-rescue: known near-cheapest composite stuck at p≥150 with empty
			//    long window (strict layering → 0 traffic → classic never fires; model
			//    only bumps weight → Sy-class bug). Expensive still blocked below.
			// Under high 性价比 weight, do NOT soft-unbury expensive accounts — that is how
			// Wawapi(Pro) kept re-entering main tier and burning money despite demotions.
			if !havePri[acc.ID] && ShouldSoftUnburySpareTier(acc.Priority) && cfg.OpAllowed(AIOpSetPriority) {
				// Just demoted into spare (e.g. 429): stay put for AISoftUnburyDwell.
				if lastSpareDemotion != nil {
					if reason := softUnburyDwellGateReason(acc.Priority, AIObservationPriority, lastSpareDemotion[acc.ID], now); reason != "" {
						continue
					}
				}
				st := AccountTrafficStats{}
				if longTraffic != nil {
					st = longTraffic[acc.ID]
				}
				rst := AccountTrafficStats{}
				if recentTraffic != nil {
					rst = recentTraffic[acc.ID]
				}
				// Block only VERY expensive (1.75×). Soft expensive (e.g. 0.06 vs 0.04)
				// must still unbury — otherwise 麻豆 stays p200 forever with zero traffic.
				if costPressureActive(cfg) && isExpensiveVsPeers(acc, accounts, AICostVeryExpensiveRatio) {
					continue
				}
				classic := softUnburyEligible(st, rst)
				cheapRescue := softUnburyCheapSpareRescue(acc, accounts, st, rst, probes)
				// Broader: known-rate not very-expensive (e.g. 麻豆 0.06 stuck at p200).
				affordableRescue := softUnburyAffordableSpareRescue(acc, accounts, st, rst, probes)
				if (classic || cheapRescue || affordableRescue) && balanceGateReason(AIOpEnable, acc) == "" &&
					acc.Status == StatusActive && acc.Schedulable {
					obs := AIObservationPriority
					reason := fmt.Sprintf(
						"自动解埋备援死循环: priority=%d∈[%d,%d] 长窗有充足健康样本且近窗无硬失败,回观察层 %d",
						acc.Priority, AIPriorityBuriedThreshold, AIMaxPriority, obs,
					)
					conf := 0.88
					if (cheapRescue || affordableRescue) && !classic {
						sig := accountCostSignalOf(acc)
						reason = fmt.Sprintf(
							"性价比/软调度修复: 已知非极贵号(composite=%.3f)卡在 priority=%d 几乎无量;抬回主层 %d 参与分流(非仅调 weight)",
							sig.Composite, acc.Priority, obs,
						)
						conf = 0.92
					}
					decision.Actions = append(decision.Actions, decisionAction{
						AccountID:  acc.ID,
						Op:         AIOpSetPriority,
						Value:      strconv.Itoa(obs),
						Reason:     reason,
						Confidence: conf,
					})
					havePri[acc.ID] = true
					injected++
				}
			}
			continue
		}
		if !cfg.OpAllowed(AIOpEnable) {
			continue
		}
		pr, ok := probes[acc.ID]
		if !ok || !pr.Fresh {
			continue
		}
		if pr.Verdict != "pass" && pr.Verdict != "slow" {
			continue
		}
		// Depleted balance: do not auto-enable.
		if reason := balanceGateReason(AIOpEnable, acc); reason != "" {
			continue
		}
		// Very expensive + healthy cheaper peers: do not re-enable (probe pass ≠ should burn money).
		// When cheap peers boom, costEnableGateReason returns "" and enable proceeds.
		if reason := costEnableGateReason(AIOpEnable, acc, accounts, cfg, recentTraffic); reason != "" {
			continue
		}
		conf := 0.82
		if pr.Verdict == "slow" {
			conf = 0.72
		}
		if pr.Source == "control_plane" {
			if conf > 0.7 {
				conf = 0.7
			}
		}
		src := pr.Source
		if src == "" {
			src = "probe"
		}
		if !haveEnable[acc.ID] {
			decision.Actions = append(decision.Actions, decisionAction{
				AccountID:  acc.ID,
				Op:         AIOpEnable,
				Value:      "",
				Reason:     fmt.Sprintf("自动恢复: aiDisabled 且 activation 探测 %s (source=%s, ttfb=%dms); 空白流量不等于仍坏", pr.Verdict, src, pr.TTFBMs),
				Confidence: conf,
			})
			haveEnable[acc.ID] = true
			injected++
		}
		// Explicit priority restore so even if ApplyAIOp un-bury is skipped, model path sets it.
		// Cost pressure: expensive accounts stay in spare (150) after enable — only deep exile lifts.
		if !havePri[acc.ID] && cfg.OpAllowed(AIOpSetPriority) {
			deep := ShouldUnburyPriority(acc.Priority)
			soft := ShouldSoftUnburySpareTier(acc.Priority) && !costBlocksMainPromotion(acc, accounts, cfg)
			if deep || soft {
				obs := RecoveryObservationPriority(acc.Priority)
				if deep && costBlocksMainPromotion(acc, accounts, cfg) {
					// Lift out of 9000-class exile but keep spare tier, not main.
					obs = AIPriorityBuriedThreshold
				}
				decision.Actions = append(decision.Actions, decisionAction{
					AccountID:  acc.ID,
					Op:         AIOpSetPriority,
					Value:      strconv.Itoa(obs),
					Reason:     fmt.Sprintf("恢复观察层: enable 后 priority 从 %d 提到 %d,避免埋葬层永久无流量", acc.Priority, obs),
					Confidence: conf,
				})
				havePri[acc.ID] = true
				injected++
			}
		}
	}
	return injected
}
