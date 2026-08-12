package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const decayTrigger = "decay"
const decayReasonPrefix = "[TTL衰减] "

// outcome/decay only consider recent actions. Settling or reverting 7-day-old
// demotions (often against deleted accounts) caused mass silent re-enables and
// thrash against intentional soft quarantine / disable.
const (
	outcomeLookbackHours = 24
	decayLookbackHours   = 24
)

// decayStaleDemotions reverts demotions that aged past TTL without supporting
// evidence (UpstreamRouter parity) — but never undoes intentional hard stops:
//   - AIOpDisable: never auto-enable (model/recovery inject owns re-enable)
//   - weight→0 soft quarantine: starved is the *goal*, not a failure to revert
func (p *AIPilotService) decayStaleDemotions(ctx context.Context, cfg AIAutopilotSettings) int {
	if p == nil || p.Repo == nil || cfg.DemotionTTLMinutes <= 0 {
		return 0
	}
	ttl := time.Duration(cfg.DemotionTTLMinutes) * time.Minute
	since := time.Now().Add(-time.Duration(decayLookbackHours) * time.Hour)
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
		// Disable is a hard stop; decay must not silently re-enable (was the
		// main thrash driver: disable → starved → auto enable → disable…).
		if a.Op == AIOpDisable {
			_ = p.Repo.MarkActionDecayed(ctx, a.ID, now)
			continue
		}
		// Cost soft quarantine / ultimate spare must stick until pilot unburies
		// (cheap peers boom), not until "starved" looks bad to TTL decay.
		if isCostIsolationDemotion(a) {
			_ = p.Repo.MarkActionDecayed(ctx, a.ID, now)
			continue
		}
		// weight→0 soft quarantine: zero post-change traffic is expected.
		if a.Op == AIOpSetWeight {
			_, after, ok := parseIntPair(a.Before, a.After)
			if ok && after <= 0 {
				_ = p.Repo.MarkActionDecayed(ctx, a.ID, now)
				continue
			}
		}
		// Spare/observation demotions (p→≥150) stick; unbury is injectRecovery /
		// soft-unbury, not decay (decay was thrashing 150↔100 under cost pressure).
		if a.Op == AIOpSetPriority {
			_, after, ok := parseIntPair(a.Before, a.After)
			if ok && after >= AIObservationPriority {
				_ = p.Repo.MarkActionDecayed(ctx, a.ID, now)
				continue
			}
		}
		v := verdictOfOutcome(a.Outcome)
		switch v {
		case verdictEffective:
			_ = p.Repo.MarkActionDecayed(ctx, a.ID, now)
			continue
		case verdictStarved, verdictWorse:
			// revert immediately for priority/weight demotions that hurt
		default:
			if now.Sub(a.TS) < ttl {
				continue
			}
		}
		if err := p.revertDemotion(ctx, a); err != nil {
			// Deleted accounts are common in the backlog — mark decayed quietly.
			if isAccountNotFoundErr(err) {
				_ = p.Repo.MarkActionDecayed(ctx, a.ID, now)
				continue
			}
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

// isCostIsolationDemotion detects pilot cost soft-quarantine demotions that
// must never be TTL-reverted (they intentionally starve expensive accounts).
func isCostIsolationDemotion(a AIAction) bool {
	r := strings.ToLower(a.Reason)
	if strings.Contains(r, "性价比") || strings.Contains(r, "软隔离") ||
		(strings.Contains(r, "cost") && strings.Contains(r, "isolat")) ||
		(strings.Contains(r, "composite") && (strings.Contains(r, "极贵") || strings.Contains(r, "过贵") || strings.Contains(r, "最低价"))) {
		return true
	}
	// Injected soft quarantine always lands at weight 0 and/or p≥150.
	if a.Op == AIOpSetWeight {
		_, after, ok := parseIntPair(a.Before, a.After)
		if ok && after <= 0 {
			return true
		}
	}
	return false
}

func isAccountNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrAccountNotFound) {
		return true
	}
	// Cover both typed and stringy ent/app errors without importing ent here.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "account not found") ||
		(strings.Contains(msg, "not found") && strings.Contains(msg, "account"))
}

func (p *AIPilotService) revertDemotion(ctx context.Context, a AIAction) error {
	if p.Accounts == nil {
		return fmt.Errorf("accounts store nil")
	}
	acc, err := p.Accounts.GetByID(ctx, a.AccountID)
	if err != nil {
		return err
	}
	if acc == nil {
		return ErrAccountNotFound
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
		// Soft quarantine weight=0 is never auto-raised here (handled above).
		if after <= 0 {
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
		// Hard-disabled: never auto-enable via decay.
		return nil
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
