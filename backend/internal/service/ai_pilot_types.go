package service

import (
	"context"
	"time"
)

// AIPilotStore persists autopilot runs/actions, scores, and traffic aggregates.
type AIPilotStore interface {
	CreateRun(ctx context.Context, run AIRun) (AIRun, error)
	CreateAction(ctx context.Context, a AIAction) (AIAction, error)
	ListRuns(ctx context.Context, limit int, beforeID int64) ([]AIRun, error)
	// ListRunsAsc returns the oldest-first slice of the most recent `limit` runs (for trends).
	ListRunsAsc(ctx context.Context, limit int) ([]AIRun, error)
	GetRun(ctx context.Context, id int64) (*AIRun, error)
	ListActionsByRun(ctx context.Context, runID int64) ([]AIAction, error)
	// ListActionsInRecentRuns returns all actions belonging to the most recent `runLimit` runs.
	ListActionsInRecentRuns(ctx context.Context, runLimit int) ([]AIAction, error)
	GetAction(ctx context.Context, id int64) (*AIAction, error)
	UpdateActionState(ctx context.Context, id int64, state, rejectReason string, rolledBackAt *time.Time) error
	ListSuggestions(ctx context.Context, limit int) ([]AIAction, error)
	// DismissSuggestions marks pending suggestions as dismissed (reason required).
	// If olderThan is non-nil, only rows with ts before that time are updated.
	// If all=true, all suggested rows are dismissed (used when switching to auto mode).
	DismissSuggestions(ctx context.Context, all bool, olderThan *time.Time, reason string) (int64, error)
	LastAppliedAt(ctx context.Context, accountID int64) (*time.Time, error)
	// LastSpareDemotions returns the latest applied demotion-into-spare (set_priority
	// with after>before and after≥150) per account since `since`. Used to enforce
	// AISoftUnburyDwell against 100↔150 thrash.
	LastSpareDemotions(ctx context.Context, accountIDs []int64, since time.Time) (map[int64]time.Time, error)
	AggregateAccountTraffic(ctx context.Context, from, to time.Time, accountIDs []int64) (map[int64]AccountTrafficStats, error)
	AggregateAccountTrafficOne(ctx context.Context, accountID int64, from, to time.Time) (AccountTrafficStats, error)
	RecentErrorSamples(ctx context.Context, from time.Time, accountID int64, limit int) ([]string, error)

	ListActionsPendingOutcome(ctx context.Context, since time.Time, limit int) ([]AIAction, error)
	NextActionTS(ctx context.Context, accountID int64, afterTS time.Time) (*time.Time, error)
	SetActionOutcome(ctx context.Context, id int64, outcome string, at time.Time) error
	ListActionsPendingDecay(ctx context.Context, since time.Time, limit int) ([]AIAction, error)
	MarkActionDecayed(ctx context.Context, id int64, at time.Time) error

	AppendAccountScores(ctx context.Context, runID int64, items []AIAccountScore) error
	LatestAccountScores(ctx context.Context) (map[int64]AIAccountScore, error)
	AccountScoreHistory(ctx context.Context, accountID int64, limit int) ([]AIAccountScore, error)
	PruneAccountScores(ctx context.Context, keepDays int) (int64, error)
}

// AccountTrafficStats is a window aggregate for snapshot.
type AccountTrafficStats struct {
	AccountID     int64
	Requests      int
	Successes     int
	Errors        int
	AvgDuration   float64
	AvgFirstToken float64
	Samples       []string
}

// AI run / action states (aligned with UpstreamRouter semantics).
const (
	AIRunOK          = "ok"
	AIRunParseFailed = "parse_failed"
	AIRunLLMError    = "llm_error"
	AIRunSkipped     = "skipped"

	AIActionApplied    = "applied"
	AIActionSuggested  = "suggested"
	AIActionRejected   = "rejected"
	AIActionRolledBack = "rolled_back"
	AIActionDismissed  = "dismissed"

	AIOpDisable             = "disable"
	AIOpEnable              = "enable"
	AIOpSetPriority         = "set_priority"
	AIOpSetWeight           = "set_weight"
	AIOpRelease             = "release"
	AIOpUnlock              = "unlock"
	AIOpSetRPMLimit         = "set_rpm_limit"
	AIOpSetMaxConcurrency   = "set_max_concurrency"
	AIOpSwitchUpstreamGroup = "switch_upstream_group"

	// SettingKeyAIAutopilot stores JSON AIAutopilotSettings in settings table.
	SettingKeyAIAutopilot = "ai_autopilot"

	// DefaultActivationProbePrompt is the short probe body for enable checks.
	DefaultActivationProbePrompt = "Reply with exactly: ok"
)

// AIAutopilotSettings is panel-stored configuration for the pilot.
// Field set mirrors UpstreamRouter config.AI so operators get the same knobs.
type AIAutopilotSettings struct {
	Enabled   bool   `json:"enabled"`
	Source    string `json:"source"` // external | self
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	Model     string `json:"model"`
	SelfGroup string `json:"self_group"`

	// ApplyMode: auto | suggest_only
	ApplyMode string `json:"apply_mode"`

	IntervalMinutes int `json:"interval_minutes"`
	TimeoutSeconds  int `json:"timeout_seconds"`
	WindowMinutes   int `json:"window_minutes"`

	// Two-stage observation windows (long + recent).
	RecentWindowMinutes int `json:"recent_window_minutes"`
	RecentSampleLimit   int `json:"recent_sample_limit"`
	TrendBucketMinutes  int `json:"trend_bucket_minutes"`

	// MemoryRuns feeds recent analysis summaries into the snapshot (cap 100).
	MemoryRuns int `json:"memory_runs"`

	MinAvailablePerGroup   int     `json:"min_available_per_group"`
	MaxActionsPerRun       int     `json:"max_actions_per_run"`
	ChannelCooldownMinutes int     `json:"channel_cooldown_minutes"`
	ManualImmunityHours    int     `json:"manual_immunity_hours"`
	ConfidenceThreshold    float64 `json:"confidence_threshold"`

	WeightMaxDeltaPercent int `json:"weight_max_delta_percent"`
	WeightMaxDeltaAbs     int `json:"weight_max_delta_abs"`
	PriorityMaxDelta      int `json:"priority_max_delta"`

	// Score dimension weights as percentages (default 40/30/20/10). Renormalized if sum≠100.
	// Controls overall = Σ dim * weight and what the model is told for set_weight rebalance.
	ScoreWeightStability  int `json:"score_weight_stability"`
	ScoreWeightLatency    int `json:"score_weight_latency"`
	ScoreWeightThroughput int `json:"score_weight_throughput"`
	ScoreWeightCost       int `json:"score_weight_cost"`

	MaxInputTokens int `json:"max_input_tokens"`
	DailyRunBudget int `json:"daily_run_budget"`

	// ManagedGroupIDs empty = all openai groups.
	ManagedGroupIDs []int64 `json:"managed_group_ids"`

	// Activation probe (stored + policy; full multi-turn probe is phased).
	ActivationProbeEnabled        *bool  `json:"activation_probe_enabled"`
	ActivationProbeTimeoutSeconds int    `json:"activation_probe_timeout_seconds"`
	ActivationProbeMaxTtfbMs      int    `json:"activation_probe_max_ttfb_ms"`
	ActivationProbeMaxPerRun      int    `json:"activation_probe_max_per_run"`
	ActivationProbeFreshMinutes   int    `json:"activation_probe_fresh_minutes"`
	ActivationProbePrompt         string `json:"activation_probe_prompt"`
	ActivationProbeJuice          bool   `json:"activation_probe_juice"`
	MaxProbeTurns                 int    `json:"max_probe_turns"`

	// Op permission gates (nil/true = on).
	OpDisable     *bool `json:"op_disable"`
	OpEnable      *bool `json:"op_enable"`
	OpSetPriority *bool `json:"op_set_priority"`
	OpSetWeight   *bool `json:"op_set_weight"`
	OpSetRPM      *bool `json:"op_set_rpm_limit"`
	OpSetMaxConc  *bool `json:"op_set_max_concurrency"`
	// OpRelease / OpUnlock map to UpstreamRouter allowRecoverBlocked / allowRecoverLockdown.
	OpRelease *bool `json:"op_release"`
	OpUnlock  *bool `json:"op_unlock"`
	// OpSwitchUpstreamGroup: change new-api token.group or sub2api panel key.group_id.
	// Default false — opt-in after operator configures panel credentials per account.
	OpSwitchUpstreamGroup *bool `json:"op_switch_upstream_group"`

	// OutcomeMinSamples / OutcomeMaxWaitMinutes: settle applied actions after enough
	// post-change traffic or max wait (UpstreamRouter closed-loop feedback).
	OutcomeMinSamples     int `json:"outcome_min_samples"`
	OutcomeMaxWaitMinutes int `json:"outcome_max_wait_minutes"`
	// DemotionTTLMinutes: auto-revert demotions without supporting evidence (0 = off).
	DemotionTTLMinutes int `json:"demotion_ttl_minutes"`
}

// DefaultAIAutopilotSettings returns factory defaults.
// Each *bool field MUST get its own allocation (via boolPtr) — a shared
// pointer collapses every switch to the last JSON field decoded on Unmarshal.
func DefaultAIAutopilotSettings() AIAutopilotSettings {
	return AIAutopilotSettings{
		Enabled:         false,
		Source:          "external",
		Model:           "claude-sonnet-4-6",
		ApplyMode:       "suggest_only",
		IntervalMinutes: 15,
		// UpstreamRouter default floor: TimeoutSeconds < 30 → 120.
		// Stream completions on CCH can take 2–3 min for full decision JSON.
		TimeoutSeconds:                300,
		WindowMinutes:                 60,
		RecentWindowMinutes:           3,
		RecentSampleLimit:             100,
		TrendBucketMinutes:            1,
		MemoryRuns:                    30,
		MinAvailablePerGroup:          1,
		MaxActionsPerRun:              3,
		ChannelCooldownMinutes:        30,
		ManualImmunityHours:           2,
		ConfidenceThreshold:           0.7,
		WeightMaxDeltaPercent:         1000,
		WeightMaxDeltaAbs:             100000,
		PriorityMaxDelta:              100,
		ScoreWeightStability:          40,
		ScoreWeightLatency:            30,
		ScoreWeightThroughput:         20,
		ScoreWeightCost:               10,
		MaxInputTokens:                60000,
		DailyRunBudget:                200,
		ActivationProbeEnabled:        boolPtr(true),
		ActivationProbeTimeoutSeconds: 20,
		ActivationProbeMaxTtfbMs:      15000,
		ActivationProbeMaxPerRun:      8,
		ActivationProbeFreshMinutes:   10,
		ActivationProbePrompt:         DefaultActivationProbePrompt,
		ActivationProbeJuice:          false,
		MaxProbeTurns:                 5,
		OpDisable:                     boolPtr(true),
		OpEnable:                      boolPtr(true),
		OpSetPriority:                 boolPtr(true),
		OpSetWeight:                   boolPtr(true),
		OpSetRPM:                      boolPtr(true),
		OpSetMaxConc:                  boolPtr(true),
		OpRelease:                     boolPtr(true),
		OpUnlock:                      boolPtr(true),
		// Off by default: requires per-account credentials + quality gates.
		OpSwitchUpstreamGroup: boolPtr(false),
		OutcomeMinSamples:     20,
		OutcomeMaxWaitMinutes: 5,
		DemotionTTLMinutes:    25,
	}
}

func (s AIAutopilotSettings) Normalize() AIAutopilotSettings {
	d := DefaultAIAutopilotSettings()
	if s.Source == "" {
		s.Source = d.Source
	}
	if s.ApplyMode == "" {
		s.ApplyMode = d.ApplyMode
	}
	if s.Model == "" {
		s.Model = d.Model
	}
	if s.IntervalMinutes <= 0 {
		s.IntervalMinutes = d.IntervalMinutes
	}
	// UpstreamRouter: TimeoutSeconds < 30 → 120. No artificial upper cap —
	// large analysis legitimately takes minutes; client-side short caps cause CCH 499.
	if s.TimeoutSeconds < 30 {
		s.TimeoutSeconds = 120
	}
	if s.MaxProbeTurns <= 0 {
		s.MaxProbeTurns = d.MaxProbeTurns
	}
	// Hard ceiling 2: each probe turn is a full LLM call (often 2–4 min on gpt-5.5).
	// Snapshot pre-probes + injectRecovery cover most enable paths without multi-turn.
	if s.MaxProbeTurns > 2 {
		s.MaxProbeTurns = 2
	}
	if s.OutcomeMinSamples <= 0 {
		s.OutcomeMinSamples = d.OutcomeMinSamples
	}
	if s.OutcomeMaxWaitMinutes <= 0 {
		s.OutcomeMaxWaitMinutes = d.OutcomeMaxWaitMinutes
	}
	if s.DemotionTTLMinutes < 0 {
		s.DemotionTTLMinutes = d.DemotionTTLMinutes
	}
	if s.MemoryRuns <= 0 {
		s.MemoryRuns = d.MemoryRuns
	}
	if s.MemoryRuns > 100 {
		s.MemoryRuns = 100
	}
	if s.ActivationProbeMaxPerRun > 16 {
		s.ActivationProbeMaxPerRun = 16
	}
	if s.WindowMinutes <= 0 {
		s.WindowMinutes = d.WindowMinutes
	}
	if s.RecentWindowMinutes <= 0 {
		s.RecentWindowMinutes = d.RecentWindowMinutes
	}
	if s.RecentSampleLimit <= 0 {
		s.RecentSampleLimit = d.RecentSampleLimit
	}
	if s.TrendBucketMinutes <= 0 {
		s.TrendBucketMinutes = d.TrendBucketMinutes
	}
	if s.MemoryRuns <= 0 {
		s.MemoryRuns = d.MemoryRuns
	}
	if s.MemoryRuns > 100 {
		s.MemoryRuns = 100
	}
	if s.MinAvailablePerGroup < 0 {
		s.MinAvailablePerGroup = d.MinAvailablePerGroup
	}
	if s.MaxActionsPerRun <= 0 {
		s.MaxActionsPerRun = d.MaxActionsPerRun
	}
	if s.ChannelCooldownMinutes < 0 {
		s.ChannelCooldownMinutes = d.ChannelCooldownMinutes
	}
	if s.ManualImmunityHours < 0 {
		s.ManualImmunityHours = d.ManualImmunityHours
	}
	if s.ConfidenceThreshold <= 0 || s.ConfidenceThreshold > 1 {
		s.ConfidenceThreshold = d.ConfidenceThreshold
	}
	// 0 = unlimited for delta caps (UpstreamRouter semantics).
	if s.WeightMaxDeltaPercent < 0 {
		s.WeightMaxDeltaPercent = d.WeightMaxDeltaPercent
	}
	if s.WeightMaxDeltaAbs < 0 {
		s.WeightMaxDeltaAbs = d.WeightMaxDeltaAbs
	}
	if s.PriorityMaxDelta < 0 {
		s.PriorityMaxDelta = d.PriorityMaxDelta
	}
	// Score weights: clamp to [0,100]; all-zero → defaults; otherwise keep and let ScoreWeights() renorm.
	clampPct := func(v, def int) int {
		if v < 0 {
			return 0
		}
		if v > 100 {
			return 100
		}
		return v
	}
	s.ScoreWeightStability = clampPct(s.ScoreWeightStability, d.ScoreWeightStability)
	s.ScoreWeightLatency = clampPct(s.ScoreWeightLatency, d.ScoreWeightLatency)
	s.ScoreWeightThroughput = clampPct(s.ScoreWeightThroughput, d.ScoreWeightThroughput)
	s.ScoreWeightCost = clampPct(s.ScoreWeightCost, d.ScoreWeightCost)
	if s.ScoreWeightStability+s.ScoreWeightLatency+s.ScoreWeightThroughput+s.ScoreWeightCost == 0 {
		s.ScoreWeightStability = d.ScoreWeightStability
		s.ScoreWeightLatency = d.ScoreWeightLatency
		s.ScoreWeightThroughput = d.ScoreWeightThroughput
		s.ScoreWeightCost = d.ScoreWeightCost
	}
	if s.MaxInputTokens <= 0 {
		s.MaxInputTokens = d.MaxInputTokens
	}
	if s.DailyRunBudget < 0 {
		s.DailyRunBudget = d.DailyRunBudget
	}
	if s.ActivationProbeTimeoutSeconds <= 0 {
		s.ActivationProbeTimeoutSeconds = d.ActivationProbeTimeoutSeconds
	}
	if s.ActivationProbeMaxTtfbMs < 0 {
		s.ActivationProbeMaxTtfbMs = d.ActivationProbeMaxTtfbMs
	}
	if s.ActivationProbeMaxPerRun <= 0 {
		s.ActivationProbeMaxPerRun = d.ActivationProbeMaxPerRun
	}
	if s.ActivationProbeFreshMinutes <= 0 {
		s.ActivationProbeFreshMinutes = d.ActivationProbeFreshMinutes
	}
	if stringsTrimSpaceEmpty(s.ActivationProbePrompt) {
		s.ActivationProbePrompt = d.ActivationProbePrompt
	}
	if s.MaxProbeTurns <= 0 {
		s.MaxProbeTurns = d.MaxProbeTurns
	}
	// Always allocate fresh pointers so fields never alias each other after Normalize.
	boolOr := func(p *bool, def bool) *bool {
		if p == nil {
			return boolPtr(def)
		}
		return boolPtr(*p)
	}
	s.ActivationProbeEnabled = boolOr(s.ActivationProbeEnabled, true)
	s.OpDisable = boolOr(s.OpDisable, true)
	s.OpEnable = boolOr(s.OpEnable, true)
	s.OpSetPriority = boolOr(s.OpSetPriority, true)
	s.OpSetWeight = boolOr(s.OpSetWeight, true)
	s.OpSetRPM = boolOr(s.OpSetRPM, true)
	s.OpSetMaxConc = boolOr(s.OpSetMaxConc, true)
	s.OpRelease = boolOr(s.OpRelease, true)
	s.OpUnlock = boolOr(s.OpUnlock, true)
	s.OpSwitchUpstreamGroup = boolOr(s.OpSwitchUpstreamGroup, false)
	return s
}

func stringsTrimSpaceEmpty(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

func (s AIAutopilotSettings) ActivationProbeOn() bool {
	return s.ActivationProbeEnabled == nil || *s.ActivationProbeEnabled
}

func (s AIAutopilotSettings) opOn(p *bool) bool {
	return p == nil || *p
}

func (s AIAutopilotSettings) OpAllowed(op string) bool {
	switch op {
	case AIOpDisable:
		return s.opOn(s.OpDisable)
	case AIOpEnable:
		return s.opOn(s.OpEnable)
	case AIOpSetPriority:
		return s.opOn(s.OpSetPriority)
	case AIOpSetWeight:
		return s.opOn(s.OpSetWeight)
	case AIOpSetRPMLimit:
		return s.opOn(s.OpSetRPM)
	case AIOpSetMaxConcurrency:
		return s.opOn(s.OpSetMaxConc)
	case AIOpRelease:
		return s.opOn(s.OpRelease)
	case AIOpUnlock:
		return s.opOn(s.OpUnlock)
	case AIOpSwitchUpstreamGroup:
		return s.opOn(s.OpSwitchUpstreamGroup)
	default:
		return false
	}
}

// AIRun is one analysis batch.
type AIRun struct {
	ID             int64      `json:"id"`
	TS             time.Time  `json:"ts"`
	Trigger        string     `json:"trigger"`
	Model          string     `json:"model"`
	WindowFrom     *time.Time `json:"window_from,omitempty"`
	WindowTo       *time.Time `json:"window_to,omitempty"`
	Status         string     `json:"status"`
	Error          string     `json:"error"`
	Summary        string     `json:"summary"`
	Observations   string     `json:"observations"`
	InputTokens    int        `json:"input_tokens"`
	OutputTokens   int        `json:"output_tokens"`
	CostMicros     int64      `json:"cost_micros"`
	LatencyMs      int        `json:"latency_ms"`
	Turns          int        `json:"turns"`
	RawRequest     string     `json:"raw_request,omitempty"`
	RawResponse    string     `json:"raw_response,omitempty"`
	SuggestedCount int        `json:"suggested_count"`
	Actions        []AIAction `json:"actions,omitempty"`
}

// AIAction is one proposed/applied mutation.
type AIAction struct {
	ID           int64      `json:"id"`
	RunID        int64      `json:"run_id"`
	TS           time.Time  `json:"ts"`
	AccountID    int64      `json:"account_id"`
	AccountName  string     `json:"account_name"`
	Op           string     `json:"op"`
	Before       string     `json:"before"`
	After        string     `json:"after"`
	Reason       string     `json:"reason"`
	Confidence   float64    `json:"confidence"`
	State        string     `json:"state"`
	RejectReason string     `json:"reject_reason"`
	Outcome      string     `json:"outcome"`
	BaselineJSON string     `json:"baseline_json,omitempty"`
	OutcomeAt    *time.Time `json:"outcome_at,omitempty"`
	DecayedAt    *time.Time `json:"decayed_at,omitempty"`
	RolledBackAt *time.Time `json:"rolled_back_at,omitempty"`
}

// AIPilotStatus is live progress for the panel.
type AIPilotStatus struct {
	// Enabled is the master switch for the background schedule loop.
	Enabled bool `json:"enabled"`
	// Scheduled means the loop is running (always true after process start).
	Scheduled bool `json:"scheduled"`
	// Running is true while one analyze cycle is in flight.
	Running         bool   `json:"running"`
	StartedAt       int64  `json:"started_at"`
	Trigger         string `json:"trigger"`
	Phase           string `json:"phase"`
	Turn            int    `json:"turn"`
	MaxTurns        int    `json:"max_turns"`
	IntervalMinutes int    `json:"interval_minutes"`
	ApplyMode       string `json:"apply_mode"`
}

// AIAccountScore is one scoring snapshot for an OpenAI account (0–100 dims).
type AIAccountScore struct {
	ID          int64     `json:"id"`
	TS          time.Time `json:"ts"`
	RunID       int64     `json:"run_id"`
	AccountID   int64     `json:"account_id"`
	AccountName string    `json:"account_name"`
	Stability   float64   `json:"stability"`
	Latency     float64   `json:"latency"`
	Throughput  float64   `json:"throughput"`
	Cost        float64   `json:"cost"`
	Overall     float64   `json:"overall"`
	Confidence  float64   `json:"confidence"`
	Note        string    `json:"note"`
}

// AIHistory is the payload for the trends tab.
type AIHistory struct {
	Runs     []AIHistoryRun     `json:"runs"`
	Actions  []AIAction         `json:"actions"`
	Accounts []AIHistoryAccount `json:"accounts"`
}

// AIHistoryRun is a compact run row on the trends timeline.
type AIHistoryRun struct {
	ID      int64     `json:"id"`
	TS      time.Time `json:"ts"`
	Trigger string    `json:"trigger"`
	Status  string    `json:"status"`
}

// AIHistoryAccount is the current account state for step-series end points.
type AIHistoryAccount struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	GroupIDs    []int64 `json:"group_ids"`
	Weight      int     `json:"weight"`
	Priority    int     `json:"priority"`
	Schedulable bool    `json:"schedulable"`
	AIDisabled  bool    `json:"ai_disabled"`
	AIManaged   bool    `json:"ai_managed"`
	Status      string  `json:"status"`
}

// AIScoreRow is latest score + live account state for the scores table.
type AIScoreRow struct {
	AccountID   int64          `json:"account_id"`
	Name        string         `json:"name"`
	GroupIDs    []int64        `json:"group_ids"`
	Weight      int            `json:"weight"`
	Priority    int            `json:"priority"`
	Schedulable bool           `json:"schedulable"`
	AIDisabled  bool           `json:"ai_disabled"`
	AIManaged   bool           `json:"ai_managed"`
	Status      string         `json:"status"`
	RateLimited bool           `json:"rate_limited"`
	Scored      bool           `json:"scored"`
	Score       AIAccountScore `json:"score"`
}

// BulkSuggestionResult counts batch approve/dismiss outcomes.
type BulkSuggestionResult struct {
	Done   int      `json:"done"`
	Failed int      `json:"failed"`
	Errors []string `json:"errors,omitempty"`
}
