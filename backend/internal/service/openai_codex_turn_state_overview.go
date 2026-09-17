package service

import (
	"context"
	"strings"
	"time"
)

type CodexTurnStateCellStatus string

const (
	CodexTurnStateCellHealthy  CodexTurnStateCellStatus = "healthy"
	CodexTurnStateCellStale    CodexTurnStateCellStatus = "stale"
	CodexTurnStateCellDegraded CodexTurnStateCellStatus = "degraded"
	CodexTurnStateCellCooling  CodexTurnStateCellStatus = "cooling"
	CodexTurnStateCellMissing  CodexTurnStateCellStatus = "missing"
	CodexTurnStateCellUnparsed CodexTurnStateCellStatus = "unparsed"
)

type CodexTurnStateCell struct {
	Model                    string                   `json:"model"`
	AutoCached               bool                     `json:"auto_cached"`
	HasValue                 bool                     `json:"has_value"`
	ValueLength              int                      `json:"value_length"`
	Health                   *CodexTurnStateHealth    `json:"health,omitempty"`
	CapturedAt               string                   `json:"captured_at,omitempty"`
	ExpiresAt                string                   `json:"expires_at,omitempty"`
	RemainingSeconds         int64                    `json:"remaining_seconds"`
	Expired                  bool                     `json:"expired"`
	CooldownReason           string                   `json:"cooldown_reason,omitempty"`
	CooldownResetAt          string                   `json:"cooldown_reset_at,omitempty"`
	CooldownRemainingSeconds int64                    `json:"cooldown_remaining_seconds"`
	CooldownBackoffLevel     int                      `json:"cooldown_backoff_level"`
	RefreshConsecutiveFails  int                      `json:"refresh_consecutive_fails"`
	RefreshTotalAttempts     int                      `json:"refresh_total_attempts"`
	RefreshTotalSuccesses    int                      `json:"refresh_total_successes"`
	RefreshLastPingCount     int                      `json:"refresh_last_ping_count"`
	RefreshLastDurationMs    int64                    `json:"refresh_last_duration_ms"`
	RefreshLastAttemptAt     string                   `json:"refresh_last_attempt_at,omitempty"`
	RefreshLastSuccessAt     string                   `json:"refresh_last_success_at,omitempty"`
	RefreshFailureKind       string                   `json:"refresh_failure_kind,omitempty"`
	RefreshFailureDetail     string                   `json:"refresh_failure_detail,omitempty"`
	RefreshDegradedCipher    int                      `json:"refresh_degraded_cipher_len"`
	RefreshExpectedCipher    int                      `json:"refresh_expected_cipher_len"`
	Status                   CodexTurnStateCellStatus `json:"status"`
}

type CodexTurnStateAccountOverview struct {
	AccountID int64                `json:"account_id"`
	Name      string               `json:"name"`
	PlanType  string               `json:"plan_type"`
	Cells     []CodexTurnStateCell `json:"cells"`
}

type CodexTurnStateOverviewSummary struct {
	Accounts        int `json:"accounts"`
	Cells           int `json:"cells"`
	Healthy         int `json:"healthy"`
	Degraded        int `json:"degraded"`
	Expired         int `json:"expired"`
	Missing         int `json:"missing"`
	CoolingDown     int `json:"cooling_down"`
	ChronicFailures int `json:"chronic_failures"`
}

type CodexTurnStateOverview struct {
	GeneratedAt string                         `json:"generated_at"`
	Config      CodexTurnStateCacheConfig      `json:"config"`
	Summary     CodexTurnStateOverviewSummary  `json:"summary"`
	Accounts    []CodexTurnStateAccountOverview `json:"accounts"`
}

func (s *OpenAIGatewayService) CodexTurnStateOverview(ctx context.Context) (*CodexTurnStateOverview, error) {
	cfg := s.codexTurnStateCacheConfig(ctx).Normalized()
	now := time.Now()
	out := &CodexTurnStateOverview{
		GeneratedAt: now.Format(time.RFC3339),
		Config:      cfg,
		Accounts:    []CodexTurnStateAccountOverview{},
	}
	if s == nil || s.accountRepo == nil {
		return out, nil
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	stats := map[int64]map[string]CodexTurnStateRefreshStat{}
	for _, stat := range s.CodexTurnStateRefreshStats() {
		byModel := stats[stat.AccountID]
		if byModel == nil {
			byModel = map[string]CodexTurnStateRefreshStat{}
			stats[stat.AccountID] = byModel
		}
		byModel[strings.ToLower(stat.Model)] = stat
	}
	ttl := cfg.TTL()
	for i := range accounts {
		account := accounts[i]
		if !account.UsesOpenAICodexProtocol() {
			continue
		}
		models := map[string]struct{}{}
		for _, model := range cfg.Models {
			models[model] = struct{}{}
		}
		for model := range extraStringMap(account.Extra, extraKeyCodexTurnStates) {
			models[strings.ToLower(strings.TrimSpace(model))] = struct{}{}
		}
		if len(models) == 0 {
			continue
		}
		row := CodexTurnStateAccountOverview{
			AccountID: account.ID,
			Name:      account.Name,
			PlanType:  account.codexPlanType(),
		}
		for model := range models {
			if model == "" {
				continue
			}
			cell := s.buildCodexTurnStateCell(&account, model, cfg, ttl, now, stats[account.ID][model])
			row.Cells = append(row.Cells, cell)
			out.Summary.Cells++
			switch cell.Status {
			case CodexTurnStateCellHealthy:
				out.Summary.Healthy++
			case CodexTurnStateCellDegraded, CodexTurnStateCellUnparsed:
				out.Summary.Degraded++
			case CodexTurnStateCellStale:
				out.Summary.Expired++
			case CodexTurnStateCellCooling:
				out.Summary.CoolingDown++
			case CodexTurnStateCellMissing:
				out.Summary.Missing++
			}
			if cell.RefreshConsecutiveFails >= codexTurnStateChronicFailureThresh {
				out.Summary.ChronicFailures++
			}
		}
		out.Accounts = append(out.Accounts, row)
		out.Summary.Accounts++
	}
	return out, nil
}

func (s *OpenAIGatewayService) buildCodexTurnStateCell(account *Account, model string, cfg CodexTurnStateCacheConfig, ttl time.Duration, now time.Time, stat CodexTurnStateRefreshStat) CodexTurnStateCell {
	value := account.GetCodexTurnState(model)
	cell := CodexTurnStateCell{
		Model:                   model,
		AutoCached:              cfg.CoversModel(model),
		HasValue:                value != "",
		ValueLength:             len(value),
		RefreshConsecutiveFails: stat.ConsecutiveFails,
		RefreshTotalAttempts:    stat.TotalAttempts,
		RefreshTotalSuccesses:   stat.TotalSuccesses,
		RefreshLastPingCount:    stat.LastPingCount,
		RefreshLastDurationMs:   stat.LastDurationMs,
		RefreshFailureKind:      stat.LastFailureKind,
		RefreshFailureDetail:    stat.LastFailureDetail,
		RefreshDegradedCipher:   stat.LastDegradedCipherLen,
		RefreshExpectedCipher:   stat.LastExpectedCipherLen,
	}
	if !stat.LastAttemptAt.IsZero() {
		cell.RefreshLastAttemptAt = stat.LastAttemptAt.Format(time.RFC3339)
	}
	if !stat.LastSuccessAt.IsZero() {
		cell.RefreshLastSuccessAt = stat.LastSuccessAt.Format(time.RFC3339)
	}
	if value != "" {
		cell.Health = InspectCodexTurnStateHealth(value, account.codexPlanType())
		captured := extraTimeMap(account.Extra, extraKeyCodexTurnStateCapturedAt)[model]
		if !captured.IsZero() {
			cell.CapturedAt = captured.Format(time.RFC3339)
			if ttl > 0 {
				expires := captured.Add(ttl)
				cell.ExpiresAt = expires.Format(time.RFC3339)
				remain := int64(expires.Sub(now).Seconds())
				if remain < 0 {
					remain = 0
					cell.Expired = true
				}
				cell.RemainingSeconds = remain
			}
		}
	}
	if cool, ok := s.getCodexTurnStateCache().cooldownFor(account.ID, model); ok {
		cell.CooldownReason = cool.Reason
		cell.CooldownResetAt = cool.Until.Format(time.RFC3339)
		remain := int64(cool.Until.Sub(now).Seconds())
		if remain < 0 {
			remain = 0
		}
		cell.CooldownRemainingSeconds = remain
		cell.CooldownBackoffLevel = cool.BackoffLevel
	}
	cell.Status = classifyCodexTurnStateCell(cell)
	return cell
}

func classifyCodexTurnStateCell(cell CodexTurnStateCell) CodexTurnStateCellStatus {
	if cell.CooldownReason != "" {
		return CodexTurnStateCellCooling
	}
	if !cell.HasValue {
		return CodexTurnStateCellMissing
	}
	if cell.Health != nil && cell.Health.Error != "" {
		return CodexTurnStateCellUnparsed
	}
	if cell.Health != nil && cell.Health.Degraded {
		return CodexTurnStateCellDegraded
	}
	if cell.Expired {
		return CodexTurnStateCellStale
	}
	return CodexTurnStateCellHealthy
}
