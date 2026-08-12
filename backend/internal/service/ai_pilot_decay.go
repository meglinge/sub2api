package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const decayTrigger = "decay"
const decayReasonPrefix = "[TTL衰减] "

// decayStaleDemotions reverts demotions that starved/worsened, or aged past TTL
// without supporting evidence (UpstreamRouter parity).
func (p *AIPilotService) decayStaleDemotions(ctx context.Context, cfg AIAutopilotSettings) int {
	if p == nil || p.Repo == nil || cfg.DemotionTTLMinutes <= 0 {
		return 0
	}
	ttl := time.Duration(cfg.DemotionTTLMinutes) * time.Minute
	since := time.Now().Add(-7 * 24 * time.Hour)
	acts, err := p.Repo.ListActionsPendingDecay(ctx, since, 100)
	if err != nil || len(acts) == 0 {
		return 0
	}
	now := time.Now()
	reverted := 0
	for _, a := range acts {
		if !isDemotionAction(a) {
			_ = p.Repo.MarkActionDecayed(ctx, a.ID, now)
			continue
		}
		v := verdictOfOutcome(a.Outcome)
		switch v {
		case verdictEffective:
			_ = p.Repo.MarkActionDecayed(ctx, a.ID, now)
			continue
		case verdictStarved, verdictWorse:
			// revert immediately
		default:
			if now.Sub(a.TS) < ttl {
				continue
			}
		}
		if err := p.revertDemotion(ctx, a); err != nil {
			if p.Log != nil {
				p.Log.Warn("demotion decay revert failed", "id", a.ID, "err", err)
			}
			_ = p.Repo.MarkActionDecayed(ctx, a.ID, now)
			continue
		}
		_ = p.Repo.MarkActionDecayed(ctx, a.ID, now)
		reverted++
	}
	if reverted > 0 && p.Log != nil {
		p.Log.Info("ai pilot demotion decay", "reverted", reverted, "trigger", decayTrigger)
	}
	return reverted
}

func (p *AIPilotService) revertDemotion(ctx context.Context, a AIAction) error {
	acc, err := p.Accounts.GetByID(ctx, a.AccountID)
	if err != nil || acc == nil {
		return err
	}
	switch a.Op {
	case AIOpSetPriority:
		before, after, ok := parseIntPair(a.Before, a.After)
		if !ok || after <= before {
			return nil
		}
		// Only revert if still at the demoted value (or deeper).
		if acc.Priority < after {
			return nil
		}
		// Converge toward observation tier, not past it (model may have set lower intentionally).
		target := before
		if target < AIObservationPriority {
			target = AIObservationPriority
		}
		if target > AIMaxPriority {
			target = AIMaxPriority
		}
		if acc.Priority == target {
			return nil
		}
		acc.Priority = target
		return p.Accounts.Update(ctx, acc)
	case AIOpSetWeight:
		before, after, ok := parseIntPair(a.Before, a.After)
		if !ok || after >= before {
			return nil
		}
		if acc.EffectiveScheduleWeight() > after {
			return nil // already raised
		}
		target := before
		if target < 10 {
			target = 10
		}
		acc.ScheduleWeight = target
		return p.Accounts.Update(ctx, acc)
	case AIOpDisable:
		// Only auto-enable if still disabled and outcome was starved/worse (not hard-fail thrash).
		if !acc.AIDisabled {
			return nil
		}
		v := verdictOfOutcome(a.Outcome)
		if v != verdictStarved && v != verdictWorse {
			// TTL-only path for disable is riskier; leave for model unless starved.
			if v != "" {
				return nil
			}
		}
		acc.AIDisabled = false
		if acc.Priority > AIMaxPriority {
			acc.Priority = AIMaxPriority
		}
		if acc.ScheduleWeight <= 0 {
			acc.ScheduleWeight = 10
		}
		return p.Accounts.Update(ctx, acc)
	default:
		return nil
	}
}

// formatDecayNote is for logs/tests.
func formatDecayNote(a AIAction) string {
	return fmt.Sprintf("%s%s %s→%s", decayReasonPrefix, a.Op, a.Before, a.After)
}

// rejectNoopPriority drops set_priority when value equals current (avoids 200→200 spam).
func rejectNoopPriority(acc *Account, act decisionAction) string {
	if act.Op != AIOpSetPriority || acc == nil {
		return ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(act.Value))
	if err != nil {
		return ""
	}
	if n == acc.Priority {
		return "priority 未变化,跳过空动作"
	}
	return ""
}
