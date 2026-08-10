package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AIPilotAccountStore is the account surface the pilot needs (narrower than
// AccountRepository so unit tests can drive ApplyAIOp/applyDecisionActions).
type AIPilotAccountStore interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	Update(ctx context.Context, account *Account) error
	UpdateExtra(ctx context.Context, id int64, updates map[string]any) error
	ListByPlatform(ctx context.Context, platform string) ([]Account, error)
}

// AIPilotService runs the account autopilot control loop.
type AIPilotService struct {
	Settings *SettingService
	Accounts AIPilotAccountStore
	Groups   GroupRepository
	Repo     AIPilotStore
	HTTP     *http.Client
	Log      *slog.Logger

	mu           sync.Mutex
	running      bool
	runStartedAt int64
	runTrigger   string
	runPhase     string
	runTurn      int
	stopCh       chan struct{}
	stoppedCh    chan struct{}
	kickCh       chan struct{}
}

// NewAIPilotService constructs the pilot.
func NewAIPilotService(
	settings *SettingService,
	accounts AccountRepository,
	groups GroupRepository,
	repo AIPilotStore,
) *AIPilotService {
	return &AIPilotService{
		Settings: settings,
		Accounts: accounts,
		Groups:   groups,
		Repo:     repo,
		HTTP:     &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}},
		Log:      slog.Default(),
		kickCh:   make(chan struct{}, 1),
	}
}

// Start begins the schedule loop (non-blocking).
func (p *AIPilotService) Start() {
	p.mu.Lock()
	if p.stopCh != nil {
		p.mu.Unlock()
		return
	}
	p.stopCh = make(chan struct{})
	p.stoppedCh = make(chan struct{})
	if p.kickCh == nil {
		p.kickCh = make(chan struct{}, 1)
	}
	p.mu.Unlock()
	go p.loop()
}

// Stop stops the schedule loop.
func (p *AIPilotService) Stop() {
	p.mu.Lock()
	if p.stopCh == nil {
		p.mu.Unlock()
		return
	}
	close(p.stopCh)
	stopped := p.stoppedCh
	p.mu.Unlock()
	if stopped != nil {
		<-stopped
	}
}

// Kick interrupts the schedule sleep so enable/interval changes take effect soon
// (and, when enabled, a schedule analyze can start without waiting a full interval).
func (p *AIPilotService) Kick() {
	if p == nil {
		return
	}
	select {
	case p.kickCh <- struct{}{}:
	default:
	}
}

func (p *AIPilotService) loop() {
	defer close(p.stoppedCh)
	// First pass: if disabled, wait; if enabled, analyze then wait.
	// Kick wakes sleep so toggling enabled does not wait a full interval.
	for {
		cfg := p.loadSettings(context.Background())
		interval := time.Duration(cfg.IntervalMinutes) * time.Minute
		if interval < time.Minute {
			interval = time.Minute
		}
		if cfg.Enabled {
			if _, err := p.Analyze(context.Background(), "schedule"); err != nil && p.Log != nil {
				// "already running" is benign when manual + schedule race.
				p.Log.Warn("ai autopilot analyze failed", "error", err)
			}
		}
		select {
		case <-p.stopCh:
			return
		case <-p.kickCh:
			// re-check settings immediately
		case <-time.After(interval):
		}
	}
}

// Status returns live progress plus master-switch state.
func (p *AIPilotService) Status() AIPilotStatus {
	cfg := p.loadSettings(context.Background())
	p.mu.Lock()
	defer p.mu.Unlock()
	return AIPilotStatus{
		Enabled:         cfg.Enabled,
		Scheduled:       p.stopCh != nil,
		Running:         p.running,
		StartedAt:       p.runStartedAt,
		Trigger:         p.runTrigger,
		Phase:           p.runPhase,
		Turn:            p.runTurn,
		MaxTurns:        cfg.MaxProbeTurns,
		IntervalMinutes: cfg.IntervalMinutes,
		ApplyMode:       cfg.ApplyMode,
	}
}

// BuildHistory assembles the trends tab payload.
func (p *AIPilotService) BuildHistory(ctx context.Context, runLimit int) (AIHistory, error) {
	if runLimit <= 0 {
		runLimit = 100
	}
	runs, err := p.Repo.ListRunsAsc(ctx, runLimit)
	if err != nil {
		return AIHistory{}, err
	}
	actions, err := p.Repo.ListActionsInRecentRuns(ctx, runLimit)
	if err != nil {
		return AIHistory{}, err
	}
	histRuns := make([]AIHistoryRun, 0, len(runs))
	for _, r := range runs {
		histRuns = append(histRuns, AIHistoryRun{ID: r.ID, TS: r.TS, Trigger: r.Trigger, Status: r.Status})
	}
	accounts, err := p.Accounts.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return AIHistory{}, err
	}
	cfg := p.loadSettings(ctx)
	managed := map[int64]bool{}
	for _, id := range cfg.ManagedGroupIDs {
		managed[id] = true
	}
	histAcc := make([]AIHistoryAccount, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		if len(cfg.ManagedGroupIDs) > 0 {
			ok := len(acc.GroupIDs) > 0
			for _, gid := range acc.GroupIDs {
				if !managed[gid] {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		histAcc = append(histAcc, AIHistoryAccount{
			ID: acc.ID, Name: acc.Name, GroupIDs: acc.GroupIDs,
			Weight: acc.EffectiveScheduleWeight(), Priority: acc.Priority,
			Schedulable: acc.Schedulable, AIDisabled: acc.AIDisabled, AIManaged: acc.AIManaged,
			Status: acc.Status,
		})
	}
	return AIHistory{Runs: histRuns, Actions: actions, Accounts: histAcc}, nil
}

// ListScoreRows returns latest scores joined with live account state.
func (p *AIPilotService) ListScoreRows(ctx context.Context) ([]AIScoreRow, error) {
	scores, err := p.Repo.LatestAccountScores(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := p.Accounts.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	cfg := p.loadSettings(ctx)
	managed := map[int64]bool{}
	for _, id := range cfg.ManagedGroupIDs {
		managed[id] = true
	}
	out := make([]AIScoreRow, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		if len(cfg.ManagedGroupIDs) > 0 {
			ok := len(acc.GroupIDs) > 0
			for _, gid := range acc.GroupIDs {
				if !managed[gid] {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		sc, scored := scores[acc.ID]
		out = append(out, AIScoreRow{
			AccountID: acc.ID, Name: acc.Name, GroupIDs: acc.GroupIDs,
			Weight: acc.EffectiveScheduleWeight(), Priority: acc.Priority,
			Schedulable: acc.Schedulable, AIDisabled: acc.AIDisabled, AIManaged: acc.AIManaged,
			Status: acc.Status, RateLimited: acc.IsRateLimited(),
			Scored: scored, Score: sc,
		})
	}
	return out, nil
}

// ApproveRunSuggestions approves all suggested actions on a run.
func (p *AIPilotService) ApproveRunSuggestions(ctx context.Context, runID int64) BulkSuggestionResult {
	acts, err := p.Repo.ListActionsByRun(ctx, runID)
	if err != nil {
		return BulkSuggestionResult{Failed: 1, Errors: []string{err.Error()}}
	}
	var res BulkSuggestionResult
	for _, a := range acts {
		if a.State != AIActionSuggested {
			continue
		}
		if err := p.ApproveAction(ctx, a.ID); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("#%d: %v", a.ID, err))
			continue
		}
		res.Done++
	}
	return res
}

// DismissRunSuggestions dismisses all suggested actions on a run.
func (p *AIPilotService) DismissRunSuggestions(ctx context.Context, runID int64) BulkSuggestionResult {
	acts, err := p.Repo.ListActionsByRun(ctx, runID)
	if err != nil {
		return BulkSuggestionResult{Failed: 1, Errors: []string{err.Error()}}
	}
	var res BulkSuggestionResult
	for _, a := range acts {
		if a.State != AIActionSuggested {
			continue
		}
		if err := p.DismissAction(ctx, a.ID); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("#%d: %v", a.ID, err))
			continue
		}
		res.Done++
	}
	return res
}

func (p *AIPilotService) setPhase(phase string, turn int) {
	p.mu.Lock()
	p.runPhase = phase
	if turn > 0 {
		p.runTurn = turn
	}
	p.mu.Unlock()
}

func (p *AIPilotService) loadSettings(ctx context.Context) AIAutopilotSettings {
	if p.Settings == nil {
		return DefaultAIAutopilotSettings().Normalize()
	}
	return p.Settings.GetAIAutopilotSettings(ctx)
}

// Analyze runs one analysis cycle (multi-turn probe + scores + gated actions).
func (p *AIPilotService) Analyze(ctx context.Context, trigger string) (AIRun, error) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return AIRun{}, fmt.Errorf("已有分析在进行")
	}
	p.running = true
	p.runStartedAt = time.Now().UnixMilli()
	p.runTrigger = trigger
	p.runPhase = "collecting"
	p.runTurn = 0
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running = false
		p.runPhase = ""
		p.mu.Unlock()
	}()

	cfg := p.loadSettings(ctx)
	if !cfg.Enabled && trigger == "schedule" {
		return AIRun{Status: AIRunSkipped, Error: "未启用", Trigger: trigger, TS: time.Now()}, nil
	}
	if cfg.BaseURL == "" || cfg.APIKey == "" {
		run := AIRun{
			TS: time.Now(), Trigger: trigger, Model: cfg.Model,
			Status: AIRunSkipped, Error: "未配置 LLM base_url/api_key",
		}
		if p.Repo != nil {
			return p.Repo.CreateRun(ctx, run)
		}
		return run, nil
	}

	// UpstreamRouter: no wall-clock around the whole Analyze — only per-LLM-call
	// TimeoutSeconds. Early abort of the whole cycle is what made CCH log 499 while
	// the model was still generating (normal user gpt-5.5 traffic stayed fine).
	now := time.Now()
	windowTo := now
	windowFrom := now.Add(-time.Duration(cfg.WindowMinutes) * time.Minute)

	// Clear stale / irrelevant pending suggestions so the panel is not a graveyard.
	// - auto mode: nothing should sit as "待审" (apply path already ran)
	// - always: drop suggestions older than AISuggestionMaxAge
	if p.Repo != nil {
		if cfg.ApplyMode == "auto" {
			_, _ = p.Repo.DismissSuggestions(ctx, true, nil, "auto 模式无需待审建议")
		} else {
			cutoff := time.Now().Add(-AISuggestionMaxAge)
			_, _ = p.Repo.DismissSuggestions(ctx, false, &cutoff, "待审建议超时自动清除")
		}
	}

	p.setPhase("collecting", 0)
	snap, accounts, managed, probeMemo, longTraffic, recentTraffic, err := p.buildSnapshot(ctx, windowFrom, windowTo, cfg)
	if err != nil {
		return AIRun{}, err
	}
	_ = recentTraffic // used at apply for demotion gate
	nameByID := map[int64]string{}
	accountsByID := map[int64]*Account{}
	for i := range accounts {
		nameByID[accounts[i].ID] = accounts[i].Name
		accountsByID[accounts[i].ID] = &accounts[i]
	}
	rawReq, _ := json.Marshal(snap)

	// Multi-turn: model may return probeRequests; backend probes and continues
	// (UpstreamRouter MaxProbeTurns, default 5, hard ceiling 8).
	maxTurns := cfg.MaxProbeTurns
	if maxTurns <= 0 {
		maxTurns = 5
	}
	if maxTurns > 8 {
		maxTurns = 8
	}

	messages := []map[string]string{
		{"role": "system", "content": aiPilotSystemPrompt},
		{"role": "system", "content": renderOpPermissions(cfg)},
		{"role": "user", "content": string(rawReq)},
	}

	var (
		finalDecision decision
		totalIn       int
		totalOut      int
		lastContent   string
		turns         int
	)
	if probeMemo == nil {
		probeMemo = map[int64]activationResult{}
	}
	started := time.Now()

	for turn := 1; turn <= maxTurns; turn++ {
		turns = turn
		p.setPhase("thinking", turn)
		content, inTok, outTok, callErr := p.callLLMMessages(ctx, cfg, messages)
		totalIn += inTok
		totalOut += outTok
		lastContent = content
		if callErr != nil {
			run := AIRun{
				TS: windowTo, Trigger: trigger, Model: cfg.Model,
				WindowFrom: &windowFrom, WindowTo: &windowTo,
				InputTokens: totalIn, OutputTokens: totalOut,
				LatencyMs: int(time.Since(started).Milliseconds()), Turns: turns,
				RawRequest: string(rawReq), RawResponse: lastContent,
				Status: AIRunLLMError, Error: callErr.Error(),
			}
			if p.Repo != nil {
				return p.Repo.CreateRun(ctx, run)
			}
			return run, nil
		}
		d, perr := parseDecision(content)
		if perr != nil {
			run := AIRun{
				TS: windowTo, Trigger: trigger, Model: cfg.Model,
				WindowFrom: &windowFrom, WindowTo: &windowTo,
				InputTokens: totalIn, OutputTokens: totalOut,
				LatencyMs: int(time.Since(started).Milliseconds()), Turns: turns,
				RawRequest: string(rawReq), RawResponse: lastContent,
				Status: AIRunParseFailed, Error: perr.Error(), Summary: "解析失败",
			}
			if p.Repo != nil {
				return p.Repo.CreateRun(ctx, run)
			}
			return run, nil
		}
		// Cost dim is deterministic from money.composite (LLM invents peer rates otherwise).
		// Then recompute overall with configured 稳/延迟/吞吐/性价比 weights.
		sw := cfg.ScoreWeights()
		for i := range d.Scores {
			d.Scores[i] = NormalizeAccountScoreWith(d.Scores[i], sw)
		}
		applyDeterministicCostScores(d.Scores, accounts, sw)

		// Multi-turn: model asked for more probes and we still have turns left.
		// Merge into probeMemo (do not drop snapshot pre-probes for disabled accounts).
		if len(d.ProbeRequests) > 0 && turn < maxTurns && cfg.ActivationProbeOn() {
			p.setPhase("probing", turn)
			results := p.runActivationProbes(ctx, cfg, d.ProbeRequests, accountsByID, nameByID)
			for _, r := range results {
				probeMemo[r.AccountID] = r
			}
			messages = append(messages,
				map[string]string{"role": "assistant", "content": content},
			)
			probeJSON, _ := json.Marshal(map[string]any{
				"probeResults": results,
				"hint":         "以上是追加探测结果。若 activation.verdict=pass/slow 且 aiDisabled,应 enable;证据足够请输出最终 summary/actions/scores；仍不足可再给 probeRequests。",
			})
			messages = append(messages, map[string]string{"role": "user", "content": string(probeJSON)})
			continue
		}
		finalDecision = d
		break
	}

	// Drop death-spiral demotions so soft-unbury is not blocked by havePri on the same account.
	// Cost-justified spare demotions (very expensive under high 性价比) are retained.
	finalDecision.Actions = filterDeathSpiralDemotions(finalDecision.Actions, accounts, longTraffic, recentTraffic, cfg)
	// Recent spare demotions (429 etc.): soft-unbury dwell so we do not 100↔150 thrash.
	accountIDs := make([]int64, 0, len(accounts))
	for i := range accounts {
		accountIDs = append(accountIDs, accounts[i].ID)
	}
	lastSpareDemotion, _ := p.Repo.LastSpareDemotions(ctx, accountIDs, time.Now().Add(-AISoftUnburyDwell))
	// Safety net: enable forgotten recovery + un-bury deep exile + break sticky spare-tier death spiral.
	// Soft-unbury skips expensive accounts when cost pressure is on (see injectRecoveryEnables).
	if n := injectRecoveryEnables(&finalDecision, accounts, probeMemo, longTraffic, recentTraffic, cfg, lastSpareDemotion, time.Now()); n > 0 && p.Log != nil {
		p.Log.Info("ai pilot injected recovery enables", "count", n, "trigger", trigger)
	}
	// High 性价比: sink very expensive healthy main-tier accounts the model kept at p=100.
	if n := injectCostSpareDemotions(&finalDecision, accounts, cfg); n > 0 && p.Log != nil {
		p.Log.Info("ai pilot injected cost spare demotions", "count", n, "trigger", trigger)
	}
	// If a recent group switch is already 503/hard-failing, roll back to last_good first.
	if n := injectUpstreamGroupRollbacks(&finalDecision, accounts, recentTraffic, cfg); n > 0 && p.Log != nil {
		p.Log.Info("ai pilot injected upstream group rollbacks", "count", n, "trigger", trigger)
	}
	// Safe upstream group switch: only when cheaper same-platform candidate exists.
	// Execution creates a temp key to probe first — never flips production cold.
	if n := injectUpstreamGroupSwitches(p, ctx, &finalDecision, accounts, recentTraffic, cfg); n > 0 && p.Log != nil {
		p.Log.Info("ai pilot injected upstream group switches", "count", n, "trigger", trigger)
	}

	run := AIRun{
		TS: windowTo, Trigger: trigger, Model: cfg.Model,
		WindowFrom: &windowFrom, WindowTo: &windowTo,
		InputTokens: totalIn, OutputTokens: totalOut,
		LatencyMs: int(time.Since(started).Milliseconds()), Turns: turns,
		RawRequest: string(rawReq), RawResponse: lastContent,
		Status: AIRunOK, Summary: finalDecision.Summary,
	}
	obs, _ := json.Marshal(finalDecision.Observations)
	run.Observations = string(obs)

	created, err := p.Repo.CreateRun(ctx, run)
	if err != nil {
		return AIRun{}, err
	}

	// Persist scores with account names.
	if len(finalDecision.Scores) > 0 {
		for i := range finalDecision.Scores {
			if finalDecision.Scores[i].AccountName == "" {
				finalDecision.Scores[i].AccountName = nameByID[finalDecision.Scores[i].AccountID]
			}
			finalDecision.Scores[i].RunID = created.ID
			finalDecision.Scores[i].TS = now
		}
		_ = p.Repo.AppendAccountScores(ctx, created.ID, finalDecision.Scores)
	}

	p.setPhase("applying", turns)
	p.applyDecisionActions(ctx, created.ID, now, cfg, finalDecision, accounts, managed, probeMemo, longTraffic, recentTraffic)

	// Opportunistic prune (best-effort).
	if cfg.DailyRunBudget > 0 { // reuse field as days? No — ScoreRetentionDays not in settings; use 30.
		_, _ = p.Repo.PruneAccountScores(ctx, 30)
	} else {
		_, _ = p.Repo.PruneAccountScores(ctx, 30)
	}

	full, err := p.Repo.GetRun(ctx, created.ID)
	if err != nil {
		return created, nil
	}
	return *full, nil
}

func (p *AIPilotService) applyDecisionActions(
	ctx context.Context,
	runID int64,
	now time.Time,
	cfg AIAutopilotSettings,
	decision decision,
	accounts []Account,
	managed map[int64]bool,
	probes map[int64]activationResult,
	longTraffic map[int64]AccountTrafficStats,
	recentTraffic map[int64]AccountTrafficStats,
) {
	sort.SliceStable(decision.Actions, func(i, j int) bool {
		return decision.Actions[i].Confidence > decision.Actions[j].Confidence
	})
	// Dwell map for soft-unbury thrash guard (LLM + inject paths share this gate).
	var lastSpareDemotion map[int64]time.Time
	if p.Repo != nil {
		ids := make([]int64, 0, len(decision.Actions))
		seen := map[int64]bool{}
		for _, act := range decision.Actions {
			if act.Op == AIOpSetPriority && !seen[act.AccountID] {
				seen[act.AccountID] = true
				ids = append(ids, act.AccountID)
			}
		}
		if len(ids) > 0 {
			lastSpareDemotion, _ = p.Repo.LastSpareDemotions(ctx, ids, now.Add(-AISoftUnburyDwell))
		}
	}
	applied := 0
	suggestOnly := cfg.ApplyMode == "suggest_only"
	for _, act := range decision.Actions {
		a := AIAction{
			RunID: runID, TS: now,
			AccountID: act.AccountID, Op: act.Op, Reason: act.Reason, Confidence: act.Confidence,
			After: act.Value,
		}
		if reason := opGateReason(cfg, act.Op); reason != "" {
			a.State = AIActionRejected
			a.RejectReason = reason
			_, _ = p.Repo.CreateAction(ctx, a)
			continue
		}
		acc, err := p.Accounts.GetByID(ctx, act.AccountID)
		if err != nil || acc == nil {
			a.State = AIActionRejected
			a.RejectReason = "账号不存在"
			_, _ = p.Repo.CreateAction(ctx, a)
			continue
		}
		a.AccountName = acc.Name

		if reason := readOnlyReason(acc, cfg, now, managed); reason != "" && act.Op != AIOpRelease && act.Op != AIOpUnlock {
			a.State = AIActionRejected
			a.RejectReason = "账号只读:" + reason
			_, _ = p.Repo.CreateAction(ctx, a)
			continue
		}
		if reason := activationGateReason(act.Op, act.Value, acc.EffectiveScheduleWeight(), probes, act.AccountID, cfg); reason != "" {
			a.State = AIActionRejected
			a.RejectReason = reason
			_, _ = p.Repo.CreateAction(ctx, a)
			continue
		}
		if reason := balanceGateReason(act.Op, acc); reason != "" {
			a.State = AIActionRejected
			a.RejectReason = reason
			_, _ = p.Repo.CreateAction(ctx, a)
			continue
		}
		var longSt, recentSt AccountTrafficStats
		if longTraffic != nil {
			longSt = longTraffic[act.AccountID]
		}
		if recentTraffic != nil {
			recentSt = recentTraffic[act.AccountID]
		}
		if act.Op == AIOpSetPriority {
			if next, err := parseIntValue(act.Value); err == nil {
				// Soft spare unbury dwell: even safety-bypass 150→100 must wait after demotion.
				// (Deep exile >200→100 is not gated by softUnburyDwellGateReason.)
				var demotedAt time.Time
				if lastSpareDemotion != nil {
					demotedAt = lastSpareDemotion[act.AccountID]
				}
				if reason := softUnburyDwellGateReason(acc.Priority, next, demotedAt, now); reason != "" {
					a.State = AIActionRejected
					a.RejectReason = reason
					_, _ = p.Repo.CreateAction(ctx, a)
					continue
				}
				// Monopoly front (p<50 e.g. default 1) → band: never treat as gated demotion.
				safetyClamp := isPrioritySafetyBypass(acc.Priority, next)
				if !safetyClamp {
					costOK := costJustifiedSpareDemotion(acc, accounts, cfg)
					if reason := priorityDemotionGateReasonEx(acc, next, longSt, recentSt, costOK); reason != "" {
						a.State = AIActionRejected
						a.RejectReason = reason
						_, _ = p.Repo.CreateAction(ctx, a)
						continue
					}
					if reason := priorityCostPromotionGateReason(acc, next, accounts, cfg); reason != "" {
						a.State = AIActionRejected
						a.RejectReason = reason
						_, _ = p.Repo.CreateAction(ctx, a)
						continue
					}
				}
			}
		}
		if act.Op == AIOpSetWeight {
			if next, err := parseIntValue(act.Value); err == nil {
				costOK := costJustifiedSpareDemotion(acc, accounts, cfg) || isExpensiveVsPeers(acc, accounts, AICostExpensiveRatio)
				if reason := weightCrushGateReasonEx(acc, next, longSt, recentSt, costOK && costPressureActive(cfg)); reason != "" {
					a.State = AIActionRejected
					a.RejectReason = reason
					_, _ = p.Repo.CreateAction(ctx, a)
					continue
				}
			}
		}
		if act.Op == AIOpDisable {
			var pr *activationResult
			if probes != nil {
				if v, ok := probes[act.AccountID]; ok {
					cp := v
					pr = &cp
				}
			}
			if reason := disableHealthyGateReasonEx(acc, longSt, recentSt, pr); reason != "" {
				a.State = AIActionRejected
				a.RejectReason = reason
				_, _ = p.Repo.CreateAction(ctx, a)
				continue
			}
		}
		if act.Op == AIOpSwitchUpstreamGroup {
			if reason := gateSwitchUpstreamGroupReason(acc, recentSt, cfg); reason != "" {
				a.State = AIActionRejected
				a.RejectReason = reason
				_, _ = p.Repo.CreateAction(ctx, a)
				continue
			}
		}
		if act.Op == AIOpSetRPMLimit {
			cur := accountBaseRPM(acc)
			next, err := parseIntValue(act.Value)
			if err == nil {
				// recent 429 heuristic: rate-limited flag or error samples mentioning 429
				has429 := acc.IsRateLimited()
				healthy := acc.Status == StatusActive && acc.Schedulable && !acc.AIDisabled && !acc.IsOverloaded()
				if reason := rpmLoosenGateReason(cur, next, has429, healthy); reason != "" {
					a.State = AIActionRejected
					a.RejectReason = reason
					_, _ = p.Repo.CreateAction(ctx, a)
					continue
				}
			}
		}
		if act.Confidence < cfg.ConfidenceThreshold {
			a.State = AIActionSuggested
			_, _ = p.Repo.CreateAction(ctx, a)
			continue
		}
		if applied >= cfg.MaxActionsPerRun {
			a.State = AIActionSuggested
			a.RejectReason = "超过单轮上限,降级为建议"
			_, _ = p.Repo.CreateAction(ctx, a)
			continue
		}
		if last, _ := p.Repo.LastAppliedAt(ctx, act.AccountID); last != nil {
			// Recovery enable / un-bury / monopoly-front clamp must bypass cooldown —
			// otherwise soft-unbury or p=1→100 is blocked for 15m after any prior
			// touch and summaries loop "紧急修复" forever (observed in prod).
			bypassCool := act.Op == AIOpEnable || act.Op == AIOpRelease || act.Op == AIOpUnlock
			// Depleted wallet: allow disable immediately (health SR is irrelevant).
			if !bypassCool && act.Op == AIOpDisable && isBalanceDepleted(acc) {
				bypassCool = true
			}
			if !bypassCool && act.Op == AIOpSetPriority {
				if next, err := parseIntValue(act.Value); err == nil && isPrioritySafetyBypass(acc.Priority, next) {
					bypassCool = true
				}
			}
			if !bypassCool {
				cool := time.Duration(cfg.ChannelCooldownMinutes) * time.Minute
				// Explicit 0 = operator wants no cooldown. Only when cooldown>0 but
				// very small, raise demotion/disable floor to AIDemotionMinCooldown so
				// 1-minute ticks cannot thrash the same account every run.
				if cfg.ChannelCooldownMinutes > 0 && isDemotionLike(act, acc) && cool < AIDemotionMinCooldown {
					cool = AIDemotionMinCooldown
				}
				if cool > 0 && now.Sub(*last) < cool {
					a.State = AIActionRejected
					a.RejectReason = "同账号冷静期内"
					_, _ = p.Repo.CreateAction(ctx, a)
					continue
				}
			}
		}
		if act.Op == AIOpDisable {
			if reason := simulateMinAvailable(acc, accounts, cfg.MinAvailablePerGroup); reason != "" {
				a.State = AIActionRejected
				a.RejectReason = reason
				_, _ = p.Repo.CreateAction(ctx, a)
				continue
			}
		}
		if act.Op == AIOpSetWeight || act.Op == AIOpSetPriority {
			if reason := amplitudeOK(acc, act, cfg); reason != "" {
				// Last-resort un-burial: if model/inject asked for observation tier
				// but amplitude still rejects (stale cfg), force via ApplyAIOp enable path
				// is not enough for already-enabled accounts — reject only if not un-burial.
				a.State = AIActionRejected
				a.RejectReason = reason
				_, _ = p.Repo.CreateAction(ctx, a)
				continue
			}
		}
		if suggestOnly {
			a.State = AIActionSuggested
			a.RejectReason = "suggest_only 模式"
			_, _ = p.Repo.CreateAction(ctx, a)
			continue
		}

		before, after, err := p.ApplyAIOp(ctx, act.AccountID, act.Op, act.Value)
		if err != nil {
			a.State = AIActionRejected
			a.RejectReason = err.Error()
			_, _ = p.Repo.CreateAction(ctx, a)
			continue
		}
		a.Before = before
		a.After = after
		a.State = AIActionApplied
		_, _ = p.Repo.CreateAction(ctx, a)
		applied++
		if act.Op == AIOpDisable {
			for i := range accounts {
				if accounts[i].ID == act.AccountID {
					accounts[i].AIDisabled = true
				}
			}
		}
	}
}

// RollbackAction rolls back one applied action.
func (p *AIPilotService) RollbackAction(ctx context.Context, id int64) error {
	a, err := p.Repo.GetAction(ctx, id)
	if err != nil {
		return err
	}
	if a.State != AIActionApplied {
		return fmt.Errorf("只能回滚已执行的动作")
	}
	if err := p.RollbackAIOp(ctx, a.AccountID, a.Op, a.Before, a.After); err != nil {
		return err
	}
	now := time.Now()
	return p.Repo.UpdateActionState(ctx, id, AIActionRolledBack, "", &now)
}

// ApproveAction applies a suggested action (still goes through gates lightly).
func (p *AIPilotService) ApproveAction(ctx context.Context, id int64) error {
	cfg := p.loadSettings(ctx)
	a, err := p.Repo.GetAction(ctx, id)
	if err != nil {
		return err
	}
	if a.State != AIActionSuggested {
		return fmt.Errorf("只能批准建议中的动作")
	}
	if reason := opGateReason(cfg, a.Op); reason != "" {
		return fmt.Errorf("%s", reason)
	}
	before, after, err := p.ApplyAIOp(ctx, a.AccountID, a.Op, a.After)
	if err != nil {
		return err
	}
	_ = before
	_ = after
	return p.Repo.UpdateActionState(ctx, id, AIActionApplied, "", nil)
}

// DismissAction dismisses a suggestion.
func (p *AIPilotService) DismissAction(ctx context.Context, id int64) error {
	a, err := p.Repo.GetAction(ctx, id)
	if err != nil {
		return err
	}
	if a.State != AIActionSuggested {
		return fmt.Errorf("只能驳回建议中的动作")
	}
	return p.Repo.UpdateActionState(ctx, id, AIActionDismissed, "dismissed", nil)
}

func (p *AIPilotService) buildSnapshot(ctx context.Context, from, to time.Time, cfg AIAutopilotSettings) (
	map[string]any, []Account, map[int64]bool, map[int64]activationResult,
	map[int64]AccountTrafficStats, map[int64]AccountTrafficStats, error,
) {
	managed := map[int64]bool{}
	for _, id := range cfg.ManagedGroupIDs {
		managed[id] = true
	}

	accounts, err := p.Accounts.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	// Autopilot is account-layer pool routing: only OpenAI API-key relay accounts.
	// Prod has thousands of oauth sticks; dumping them into the LLM snapshot made
	// every CCH chat/completions request multi-minute (and abort-499).
	// Also skip control-plane LLM accounts (exclude_from_schedule, e.g. GPTX): they
	// power the pilot itself and must not be rebalanced as pool channels.
	{
		filtered := accounts[:0]
		for i := range accounts {
			if !accounts[i].IsOpenAIApiKey() {
				continue
			}
			if accounts[i].IsExcludedFromSchedule() {
				continue
			}
			filtered = append(filtered, accounts[i])
		}
		accounts = filtered
	}

	// Filter managed groups when configured.
	if len(cfg.ManagedGroupIDs) > 0 {
		filtered := accounts[:0]
		for i := range accounts {
			acc := accounts[i]
			if len(acc.GroupIDs) == 0 {
				continue
			}
			// Include if any group is managed (OR), not require all — pool accounts
			// may sit in Pro+Team simultaneously.
			anyIn := false
			for _, gid := range acc.GroupIDs {
				if managed[gid] {
					anyIn = true
					break
				}
			}
			if anyIn {
				filtered = append(filtered, acc)
			}
		}
		accounts = filtered
	}

	ids := make([]int64, 0, len(accounts))
	nameByID := map[int64]string{}
	accountsByID := map[int64]*Account{}
	for i := range accounts {
		ids = append(ids, accounts[i].ID)
		nameByID[accounts[i].ID] = accounts[i].Name
		accountsByID[accounts[i].ID] = &accounts[i]
	}
	traffic, _ := p.Repo.AggregateAccountTraffic(ctx, from, to, ids)

	// Recent window: short look at "right now" so a brief blip doesn't poison the long window.
	recentFrom := to.Add(-time.Duration(cfg.RecentWindowMinutes) * time.Minute)
	if recentFrom.Before(from) {
		recentFrom = from
	}
	recentTraffic, _ := p.Repo.AggregateAccountTraffic(ctx, recentFrom, to, ids)

	// Pre-decision recovery probes (UpstreamRouter parity): AI-disabled / weight=0 / idle
	// get a live check so the model can enable again and gates have fresh evidence.
	probeMemo := map[int64]activationResult{}
	if cfg.ActivationProbeOn() {
		p.setPhase("probing", 0)
		reqs := collectRecoveryProbeRequests(accounts, traffic, cfg.ActivationProbeMaxPerRun)
		for _, r := range p.runActivationProbes(ctx, cfg, reqs, accountsByID, nameByID) {
			probeMemo[r.AccountID] = r
		}
	}

	// Group availability summary.
	groupAvail := map[int64]map[string]any{}
	for i := range accounts {
		acc := &accounts[i]
		for _, gid := range acc.GroupIDs {
			g := groupAvail[gid]
			if g == nil {
				g = map[string]any{"id": gid, "channelCount": 0, "availableCount": 0}
				groupAvail[gid] = g
			}
			g["channelCount"] = g["channelCount"].(int) + 1
			if acc.Status == StatusActive && acc.Schedulable && !acc.AIDisabled {
				g["availableCount"] = g["availableCount"].(int) + 1
			}
		}
	}
	groups := make([]map[string]any, 0, len(groupAvail))
	for _, g := range groupAvail {
		groups = append(groups, g)
	}

	chs := make([]map[string]any, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		st := traffic[acc.ID]
		successRate := 1.0
		if st.Requests+st.Errors > 0 {
			successRate = float64(st.Successes) / float64(st.Requests+st.Errors)
		}
		rst := recentTraffic[acc.ID]
		recentRate := 1.0
		if rst.Requests+rst.Errors > 0 {
			recentRate = float64(rst.Successes) / float64(rst.Requests+rst.Errors)
		}
		sampleLimit := 3
		if cfg.RecentSampleLimit > 0 && cfg.RecentSampleLimit < sampleLimit {
			sampleLimit = cfg.RecentSampleLimit
		}
		samples, _ := p.Repo.RecentErrorSamples(ctx, from, acc.ID, sampleLimit)
		ro := readOnlyReason(acc, cfg, time.Now(), managed)
		idle := st.Requests+st.Errors == 0
		// Soft-refresh money: 余额 ~1m cache, 倍率 ~5m cache (see AIBalance/AIRateCacheMaxAge).
		_, _, _, _ = p.ResolveAccountMoney(ctx, acc)
		money := moneyView(acc)
		chs = append(chs, map[string]any{
			"id": acc.ID, "name": acc.Name, "groups": acc.GroupIDs,
			"priority": acc.Priority, "weight": acc.EffectiveScheduleWeight(),
			"concurrency": acc.Concurrency, "baseRpm": accountBaseRPM(acc),
			"readOnly": ro != "", "readOnlyReason": ro,
			"state": map[string]any{
				"status": acc.Status, "schedulable": acc.Schedulable,
				"aiDisabled": acc.AIDisabled, "aiManaged": acc.AIManaged,
				"rateLimited": acc.IsRateLimited(), "overloaded": acc.IsOverloaded(),
				"tempUnschedulable": acc.TempUnschedulableUntil != nil && time.Now().Before(*acc.TempUnschedulableUntil),
				"tempUnschedReason": acc.TempUnschedulableReason,
			},
			"traffic": map[string]any{
				"requests": st.Requests, "errors": st.Errors,
				"successRate":   successRate,
				"avgDurationMs": st.AvgDuration, "avgTtfbMs": st.AvgFirstToken,
			},
			"recentTraffic": map[string]any{
				"windowMinutes": cfg.RecentWindowMinutes,
				"requests":      rst.Requests, "errors": rst.Errors,
				"successRate":   recentRate,
				"avgDurationMs": rst.AvgDuration, "avgTtfbMs": rst.AvgFirstToken,
			},
			"errors":     map[string]any{"samples": samples},
			"activation": activationView(acc, probeMemo[acc.ID], idle, cfg),
			"money":      money,
			"upstreamGroup": func() UpstreamGroupView {
				ug := buildUpstreamGroupView(acc)
				if ug.SwitchEnabled && ug.Switchable && len(ug.Candidates) == 0 {
					if cands, err := p.ListUpstreamGroupCandidates(ctx, acc); err == nil {
						ug.Candidates = cands
					} else if ug.Reason == "" {
						ug.Reason = "候选组拉取失败: " + truncateStr(err.Error(), 80)
					}
				}
				return ug
			}(),
		})
	}
	// Replace groups summary with peers-enriched block (same ids).
	groups = buildGroupPeers(chs, groupAvail)

	allowed, denied := []string{}, []string{}
	for _, op := range []string{AIOpSetPriority, AIOpSetWeight, AIOpDisable, AIOpEnable, AIOpSetRPMLimit, AIOpSetMaxConcurrency, AIOpRelease, AIOpUnlock, AIOpSwitchUpstreamGroup} {
		if cfg.OpAllowed(op) {
			allowed = append(allowed, op)
		} else {
			denied = append(denied, op)
		}
	}

	// Cross-run memory — UpstreamRouter buildRecentMemory parity:
	// recent N runs + action results so the model avoids thrash / re-do loops.
	// Do NOT strip this for "payload size"; original keeps full applied/suggested/rejected.
	memory := []map[string]any{}
	memN := cfg.MemoryRuns
	if memN <= 0 {
		memN = 30
	}
	if memN > 100 {
		memN = 100
	}
	if memN > 0 && p.Repo != nil {
		runs, _ := p.Repo.ListRuns(ctx, memN, 0)
		for _, r := range runs {
			acts := []map[string]any{}
			applied, suggested, rejected := 0, 0, 0
			if list, err := p.Repo.ListActionsByRun(ctx, r.ID); err == nil {
				for _, a := range list {
					switch a.State {
					case AIActionApplied:
						applied++
					case AIActionSuggested:
						suggested++
					case AIActionRejected:
						rejected++
					}
					acts = append(acts, map[string]any{
						"accountId": a.AccountID, "accountName": a.AccountName,
						"op": a.Op, "before": a.Before, "after": a.After,
						"state": a.State, "confidence": a.Confidence,
						"reason":       truncateStr(a.Reason, 160),
						"rejectReason": truncateStr(a.RejectReason, 100),
					})
				}
			}
			memory = append(memory, map[string]any{
				"id": r.ID, "ts": r.TS.UnixMilli(), "trigger": r.Trigger, "status": r.Status,
				"summary": truncateStr(r.Summary, 300),
				"error":   truncateStr(r.Error, 160),
				"counts":  map[string]int{"actions": len(acts), "applied": applied, "suggested": suggested, "rejected": rejected},
				"actions": acts,
			})
		}
	}

	snap := map[string]any{
		"now":                 to.UnixMilli(),
		"windowMinutes":       cfg.WindowMinutes,
		"recentWindowMinutes": cfg.RecentWindowMinutes,
		"trendBucketMinutes":  cfg.TrendBucketMinutes,
		"policy": map[string]any{
			"minAvailablePerGroup":        cfg.MinAvailablePerGroup,
			"maxActions":                  cfg.MaxActionsPerRun,
			"cooldownMinutes":             cfg.ChannelCooldownMinutes,
			"manualImmunityHours":         cfg.ManualImmunityHours,
			"confidenceThreshold":         cfg.ConfidenceThreshold,
			"weightMaxDeltaPercent":       cfg.WeightMaxDeltaPercent,
			"weightMaxDeltaAbs":           cfg.WeightMaxDeltaAbs,
			"priorityMaxDelta":            cfg.PriorityMaxDelta,
			"applyMode":                   cfg.ApplyMode,
			"allowedOps":                  allowed,
			"deniedOps":                   denied,
			"activationProbeEnabled":      cfg.ActivationProbeOn(),
			"activationProbeMaxTtfbMs":    cfg.ActivationProbeMaxTtfbMs,
			"activationProbeFreshMinutes": cfg.ActivationProbeFreshMinutes,
			"maxProbeTurns":               cfg.MaxProbeTurns,
			"platform":                    "openai",
			"routingModel":                "priority 越小越优先; schedule_weight 只在 Top-K/同层内调比例; disable 写 ai_disabled 不改人工 schedulable",
			"windowGuidance":              "长窗看稳定性; 近况窗看此刻。两者冲突时以近况为准。",
			"activationGuide": map[string]any{
				"what":     "channels[].activation 是拉起来之前的现场证据。没流量/被 AI 停用时统计是空白,空白不等于健康",
				"rule":     "enable 或 weight 0→非0 之前必须看 activation.verdict; fail/unknown 不要提",
				"recover":  "aiDisabled=true 且 activation.verdict=pass|slow → 应 enable 恢复; 探测失败才继续停用; enable 会把 priority>500 解埋到观察层 200",
				"enforced": "护栏会拒绝无新鲜探测背书的 enable; 后端也会在探测通过且模型漏提时自动补 enable+解埋",
			},
			"rateConfidenceGuide": map[string]any{
				"level1_high":   "source=imported/newapi/sub2api/billing_probe: money.compositeRateMultiplier=rate/recharge 一级高置信,比价优先信它",
				"level2_medium": "source=custom 且 rate≠1: 人工非默认倍率,二级置信",
				"level3_low":    "默认 rate=1 或估算: 三级低置信;仅辅助",
				"useField":      "money.rateConfidence.trustedComposite + level; groups[].peers[].compositeRate 已是 trusted 值",
				"recharge":      "1:10 充值 → rechargeMultiplier=10 → composite=rate/10(更便宜)",
			},
			"scoreGuide": map[string]any{
				"weights": map[string]any{
					"stabilityPct":  cfg.ScoreWeightStability,
					"latencyPct":    cfg.ScoreWeightLatency,
					"throughputPct": cfg.ScoreWeightThroughput,
					"costPct":       cfg.ScoreWeightCost,
					"note":          "设置可改;后端按 composite 重算 cost 维并重算 overall,set_weight 跟 overall",
				},
				"weightOp": "同 priority 层 set_weight 大致按 overall 比例,不是纯成本均分;性价比权重高时更偏向便宜号",
				"costBackend": map[string]any{
					"costDim":        "scores[].cost 由后端按 money.rateConfidence.trustedComposite 池内比价写死,禁止模型编造 peer 倍率",
					"unknownRate":    "无导入倍率(default_one) cost≈40 中偏低,不要当成 0.06 便宜号",
					"pressureOn":     costPressureActive(cfg),
					"pressureRule":   "cost 权重≥20% 时:贵号禁止抬回主层;极贵健康号可沉备援;禁止自动解埋贵号",
					"expensiveRatio": AICostExpensiveRatio,
					"veryExpensive":  AICostVeryExpensiveRatio,
				},
			},
			"balanceGuide": map[string]any{
				"field":      "money.balanceStatus + money.balanceUsd",
				"gate":       "depleted 硬门禁:禁止 enable/release/unlock",
				"scheduling": "原版对齐:余额进快照给模型做分流,不是内核硬编码。同层:余额低→降 weight/后置;余额充裕且便宜→抬 weight;depleted→disable 或深沉",
				"lowHintUSD": 5,
				"cache":      "~5m 级缓存",
				"sources":    "sk GET /v1/usage(sub2api); dashboard billing(one-api); mgmt /api/user/self",
			},
		},
		"memory":   memory,
		"groups":   groups,
		"channels": chs, // keep key name channels for prompt compatibility; values are accounts
	}
	return snap, accounts, managed, probeMemo, traffic, recentTraffic, nil
}

type decision struct {
	Summary       string           `json:"summary"`
	Actions       []decisionAction `json:"actions"`
	Observations  []any            `json:"observations"`
	Notices       []any            `json:"notices"`
	ProbeRequests []probeRequest   `json:"probeRequests"`
	Scores        []AIAccountScore `json:"scores"`
}

// flexibleDecision is an intermediate decode that tolerates loose model JSON.
type flexibleDecision struct {
	Summary       string            `json:"summary"`
	Actions       []json.RawMessage `json:"actions"`
	Observations  any               `json:"observations"`
	Notices       any               `json:"notices"`
	ProbeRequests []json.RawMessage `json:"probeRequests"`
	Scores        []json.RawMessage `json:"scores"`
}

func parseDecision(content string) (decision, error) {
	content = strings.TrimSpace(content)
	// Strip ```json fences (UpstreamRouter style: find first fence anywhere)
	if i := strings.Index(content, "```"); i >= 0 {
		content = content[i+3:]
		content = strings.TrimPrefix(content, "json")
		content = strings.TrimPrefix(content, "JSON")
		if j := strings.Index(content, "```"); j >= 0 {
			content = content[:j]
		}
		content = strings.TrimSpace(content)
	}
	// Find first JSON object
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return decision{}, fmt.Errorf("响应中无 JSON 对象")
	}
	content = content[start : end+1]

	var raw flexibleDecision
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return decision{}, err
	}

	d := decision{Summary: raw.Summary}
	d.Observations = coerceAnySlice(raw.Observations)
	d.Notices = coerceAnySlice(raw.Notices)

	for _, item := range raw.Actions {
		act, err := parseDecisionAction(item)
		if err != nil {
			continue
		}
		act.Op = strings.TrimSpace(act.Op)
		act.Value = strings.TrimSpace(act.Value)
		d.Actions = append(d.Actions, act)
	}
	for _, item := range raw.ProbeRequests {
		pr, err := parseProbeRequest(item)
		if err != nil || pr.AccountID <= 0 {
			continue
		}
		d.ProbeRequests = append(d.ProbeRequests, pr)
	}
	for _, item := range raw.Scores {
		sc, err := parseScoreEntry(item)
		if err != nil || sc.AccountID <= 0 {
			continue
		}
		d.Scores = append(d.Scores, NormalizeAccountScore(sc))
	}
	return d, nil
}

func parseProbeRequest(raw json.RawMessage) (probeRequest, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return probeRequest{}, err
	}
	id := anyToInt64(m["accountId"])
	if id == 0 {
		id = anyToInt64(m["channelId"])
	}
	if id == 0 {
		id = anyToInt64(m["account_id"])
	}
	juice := false
	switch v := m["juice"].(type) {
	case bool:
		juice = v
	case string:
		juice = strings.EqualFold(v, "true") || v == "1"
	default:
		juice = anyToFloat64(m["juice"]) != 0
	}
	return probeRequest{
		AccountID: id,
		Reason:    anyToString(m["reason"]),
		Juice:     juice,
	}, nil
}

func parseScoreEntry(raw json.RawMessage) (AIAccountScore, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return AIAccountScore{}, err
	}
	id := anyToInt64(m["accountId"])
	if id == 0 {
		id = anyToInt64(m["channelId"])
	}
	if id == 0 {
		id = anyToInt64(m["account_id"])
	}
	return AIAccountScore{
		AccountID:  id,
		Stability:  anyToFloat64(m["stability"]),
		Latency:    anyToFloat64(m["latency"]),
		Throughput: anyToFloat64(m["throughput"]),
		Cost:       anyToFloat64(m["cost"]),
		Overall:    anyToFloat64(m["overall"]),
		Confidence: anyToFloat64(m["confidence"]),
		Note:       anyToString(m["note"]),
	}, nil
}

func coerceAnySlice(v any) []any {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case []any:
		return t
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []any{t}
	default:
		return []any{t}
	}
}

func parseDecisionAction(raw json.RawMessage) (decisionAction, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return decisionAction{}, err
	}
	var a decisionAction
	// accountId or channelId (UpstreamRouter compatibility)
	a.AccountID = anyToInt64(m["accountId"])
	if a.AccountID == 0 {
		a.AccountID = anyToInt64(m["channelId"])
	}
	if a.AccountID == 0 {
		a.AccountID = anyToInt64(m["account_id"])
	}
	a.Op = anyToString(m["op"])
	a.Value = anyToString(m["value"])
	a.Reason = anyToString(m["reason"])
	a.Confidence = anyToFloat64(m["confidence"])
	return a, nil
}

func anyToString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// Prefer integer-looking numbers without .0
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

func anyToInt64(v any) int64 {
	if v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	default:
		return 0
	}
}

func anyToFloat64(v any) float64 {
	if v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	default:
		return 0
	}
}

func chatCompletionsURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

func (p *AIPilotService) callLLM(ctx context.Context, cfg AIAutopilotSettings, snapshotJSON string) (content string, inTok, outTok int, err error) {
	messages := []map[string]string{
		{"role": "system", "content": aiPilotSystemPrompt},
		{"role": "system", "content": renderOpPermissions(cfg)},
		{"role": "user", "content": snapshotJSON},
	}
	return p.callLLMMessages(ctx, cfg, messages)
}

func renderOpPermissions(cfg AIAutopilotSettings) string {
	var b strings.Builder
	b.WriteString("【本轮动作权限】\n")
	for _, op := range []string{AIOpSetPriority, AIOpSetWeight, AIOpDisable, AIOpEnable, AIOpSetRPMLimit, AIOpSetMaxConcurrency, AIOpRelease, AIOpUnlock} {
		if cfg.OpAllowed(op) {
			b.WriteString("【允许】" + op + "\n")
		} else {
			b.WriteString("【已关闭】" + op + "\n")
		}
	}
	b.WriteString("applyMode=" + cfg.ApplyMode + "\n")
	return b.String()
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Used to silence unused import if strconv only used in ops.
var _ = strconv.Itoa

const aiPilotSystemPrompt = `你是 sub2api 号池的运维助手(自动驾驶)。目标:在保证分组可用性的前提下,把流量导向更健康的 OpenAI 账号。

你操作的单位是 account(快照里 channels 数组的每一项其实是账号)。
选路现实:
- priority 越小越优先,且是**严格分层**:只要 priority 更小的层还有可用号,更大 priority 的号**永远接不到请求**
- 后端会把 set_priority 钳在 [50,200](对齐原版 80–120 思路,默认层 100);若账号已是 priority=1 等小于50,应 set_priority 到 100,后端会绕过冷静期/单次 delta 完成纠正(也可由后端自动注入)
- 因此**禁止**把某一个账号单独提到顶层而把其他可用号沉深 —— 那等于全池只跑一个供应商
- 健康池:尽量 2–4 个号同在 priority≈100,用 set_weight 按**综合分**分流(不是纯按成本均分)
- schedule_weight(字段 weight)只在**同一 priority 层**内调分流比例
- 禁止用 weight=0 当软停,要停就用 disable
- disable/enable 只切换 aiDisabled,绝不等于人工 schedulable/status
- state.schedulable=false 是人工/系统调度状态,你不能靠 enable 修好,只能写 observations
- sticky/first_output 失败解绑已有系统处理;你负责结构性降权/停用/恢复,不要建议 temp_unsched 连环冷却
- traffic 是长窗; recentTraffic 是近况窗。两者冲突时以近况为准
- memory 是历史分析,避免来回拧同一账号
- activation 是现场探测:停用/零流量账号没有统计时必须看它;空白流量 ≠ 仍坏

【打分与同层分流 —— 必须遵守】
- scores 四维权重以 **policy.scoreGuide.weights** 为准(管理员可在设置改;默认 稳40/延迟30/吞吐20/性价比10)
  overall ≈ stab*wS + lat*wL + thr*wT + cost*wC (后端按设置重算)
- 同层 set_weight **大致按 overall 比例分配**,不是「都 1」
  例:同层 A overall=80、B overall=40 → weight 可约 2:1
- **cost 维由后端按 composite 池内比价写死**(模型分仅作参考会被覆盖);禁止把无倍率号编造成 peer 的 0.06
- 无导入倍率(rateConfidence.level=3/default_one)不是便宜号
- 性价比权重≥20% 时后端硬门禁:贵号禁止抬回主层≤100;极贵号可健康沉备援;禁止自动解埋贵号
- 稳/延迟/吞吐:看 traffic + recentTraffic(近况优先)

【余额参与调度 —— 原版对齐】
- money.balanceStatus / money.balanceUsd 已写入快照(与 UpstreamRouter 一样给模型用,不是内核硬切流量)
- depleted → 禁止 enable/恢复;应 **disable**(余额耗尽硬门禁,后端会放行 disable 即使长窗 SR 仍高)
- balanceUsd 偏低(约 <$5,见 policy.balanceGuide.lowHintUSD)且仍有流量 → 同层降 weight,把量让给余额更充裕的 peer;低余额≠depleted,不要 disable
- balance 充裕 + 便宜(composite 低) → 同层应拿更高 weight
- unlimited 视为余额不约束

【禁止备援死循环 / 解埋再沉震荡】
- 后端在「刚下沉到备援(≥150)」后有约 25 分钟备援驻留:期间自动解埋与 set_priority 拉回主层会被拒绝(防 429 pending thrash 100↔150)
- 驻留期内请用 set_weight 调同层/备援分流,不要反复 set_priority 100↔150
- priority≥150 在严格分层下几乎接不到请求 → 近窗必然空白 → **禁止**再据此 set_priority 更深

【上游分组切换 switch_upstream_group — 可选,极保守】
- 仅当 channels[].upstreamGroup.switchable=true 且 policy 允许该 op 时才可调用
- value 必须是 candidates 里 eligible=true 的 id/name(已过滤 Claude/denylist/「随时拉闸」等高风险组名)
- 后端: **临时 key 多次探测 → 软摘流 → 改生产 key → 再生产多次探测,失败回滚并拉黑 → 删临时 key;切后强制留在备援观察**
- 若刚切组后近窗 503/硬失败: 后端会自动回滚 last_good 并 disable,勿再切更便宜组
- 禁止建议不在 candidates 的组;省≥15% 且 30m 驻留

- 近窗 0 请求且长窗成功率仍高 → 不是故障,是分层后果
- **禁止**对 p≥150 的可用号只 set_weight 不抬 priority:同层 weight 再大也吃不到主层流量
- **禁止**用「主层已有 2–4 个/满池」拒绝把**已知便宜且健康**的号从 ≥150 抬回 100
  (2–4 是多样性目标,不是容量上限;便宜稳号卡在 150=性价比设置失效)
- 慢(TTFB 高)但成功率高 → 同层降 weight(且勿压到 0/1),不要 priority 沉到 150+
- 只有近窗/长窗**硬失败**(成功率明显崩、错误成片)才允许沉到 ≥150 或 disable
- 后端会:拒绝无硬故障的备援下沉;长窗健康或已知便宜号 150–200 自动拉回 100;下沉类动作有最短冷静期
- 健康池保持 2–4 个号同在 priority≈100,用 weight 分流,不要每轮把人踢进 150 再解埋

原则:
1) **同层多号**:每组至少保留 2–4 个可用账号在相近 priority(建议都在 50–150 一带),用 set_weight 按 overall 分流
2) 只有明确坏号才 set_priority 沉到更深一层;不要把「稍差但可用」沉到万级
3) disable 是最后手段,且必须考虑 minAvailablePerGroup
4) **恢复与停用同等重要**:每一轮都要扫 aiDisabled=true 的账号。
   - activation.verdict=pass|slow → 应 enable;后端会把 priority 过深解埋到观察层 100
   - activation.verdict=fail|unknown → 保持停用
5) 性价比/cost 必须看 money.rateConfidence.trustedComposite 与 groups[].peers 比价
   - compositeRate = rateMultiplier/rechargeMultiplier
   - **池内最便宜/次便宜且 probe pass** 若仍在 p≥150 → 应 set_priority 100,不要只 +weight
6) money.balanceStatus=depleted 时不要 enable/恢复流量;并优先处理低余额号的分流
7) 所有**可用**渠道都健康且没有待恢复号时,actions 才应为空
8) reason 写具体指标数值(含 balanceUsd / composite 时更好);confidence 反映把握
9) 某个问题只有被关掉的动作能解决时,写进 observations 说明现状

多轮:证据不足时可输出 probeRequests 让后端先探测再继续,例如
{"summary":"","probeRequests":[{"accountId":1,"reason":"想确认是否恢复"}],"actions":[],"scores":[],"observations":[]}

最终轮必须输出 JSON(不要 markdown):
{"summary":"...","actions":[{"accountId":1,"op":"set_priority","value":"100","reason":"...","confidence":0.8}],"scores":[{"accountId":1,"stability":80,"latency":70,"throughput":75,"cost":85,"overall":76.5,"confidence":0.85,"note":"..."}],"observations":[{"group":"1","note":"..."}],"notices":[],"probeRequests":[]}
op 只能取自 policy.allowedOps。value 用字符串。scores 各维 0–100,越高越好;有新证据的账号都应打分;overall 按设置权重自洽。`
