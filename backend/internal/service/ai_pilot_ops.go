package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ApplyAIOp mutates an account for a phase-1 op and returns before/after snapshots.
func (p *AIPilotService) ApplyAIOp(ctx context.Context, accountID int64, op, value string) (before, after string, err error) {
	acc, err := p.Accounts.GetByID(ctx, accountID)
	if err != nil {
		return "", "", err
	}
	if acc == nil {
		return "", "", fmt.Errorf("account %d not found", accountID)
	}

	switch op {
	case AIOpSetPriority:
		before = strconv.Itoa(acc.Priority)
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return "", "", fmt.Errorf("priority 无效: %w", err)
		}
		// Clamp to UpstreamRouter-style band [50,200]. Outside this range,
		// layered routing either monopolizes (sole p=1) or starves (p=90000).
		if n > AIMaxPriority {
			n = AIMaxPriority
		}
		if n < AIMinPriority {
			n = AIMinPriority
		}
		acc.Priority = n
		after = strconv.Itoa(n)
		return before, after, p.Accounts.Update(ctx, acc)

	case AIOpSetWeight:
		before = strconv.Itoa(acc.EffectiveScheduleWeight())
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 0 {
			return "", "", fmt.Errorf("weight 无效")
		}
		acc.ScheduleWeight = n
		after = strconv.Itoa(n)
		return before, after, p.Accounts.Update(ctx, acc)

	case AIOpDisable:
		before = strconv.FormatBool(acc.AIDisabled)
		if acc.AIDisabled {
			return before, before, nil
		}
		acc.AIDisabled = true
		after = "true"
		return before, after, p.Accounts.Update(ctx, acc)

	case AIOpEnable:
		before = strconv.FormatBool(acc.AIDisabled)
		if !acc.AIDisabled {
			return before, before, nil
		}
		acc.AIDisabled = false
		// Un-bury: AI often set_priority to 9000+ when disabling; enable alone
		// leaves the account permanently off traffic. Lift to observation tier.
		if ShouldUnburyPriority(acc.Priority) {
			acc.Priority = RecoveryObservationPriority(acc.Priority)
		}
		if acc.ScheduleWeight <= 0 {
			acc.ScheduleWeight = 10
		}
		after = fmt.Sprintf("false;priority=%d;weight=%d", acc.Priority, acc.EffectiveScheduleWeight())
		return before, after, p.Accounts.Update(ctx, acc)

	case AIOpSetMaxConcurrency:
		before = strconv.Itoa(acc.Concurrency)
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 0 {
			return "", "", fmt.Errorf("concurrency 无效")
		}
		if n == 0 {
			n = 1
		}
		acc.Concurrency = n
		after = strconv.Itoa(n)
		return before, after, p.Accounts.Update(ctx, acc)

	case AIOpSetRPMLimit:
		before = fmt.Sprintf("%v", accountBaseRPM(acc))
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 0 {
			return "", "", fmt.Errorf("rpm 无效")
		}
		if acc.Extra == nil {
			acc.Extra = map[string]any{}
		}
		if n == 0 {
			delete(acc.Extra, "base_rpm")
			after = "0"
		} else {
			acc.Extra["base_rpm"] = n
			after = strconv.Itoa(n)
		}
		return before, after, p.Accounts.Update(ctx, acc)

	case AIOpRelease:
		before = releaseStateSnapshot(acc)
		acc.RateLimitedAt = nil
		acc.RateLimitResetAt = nil
		acc.OverloadUntil = nil
		acc.TempUnschedulableUntil = nil
		acc.TempUnschedulableReason = ""
		after = "released"
		return before, after, p.Accounts.Update(ctx, acc)

	case AIOpUnlock:
		before = releaseStateSnapshot(acc)
		acc.RateLimitedAt = nil
		acc.RateLimitResetAt = nil
		acc.OverloadUntil = nil
		acc.TempUnschedulableUntil = nil
		acc.TempUnschedulableReason = ""
		acc.ErrorMessage = ""
		if acc.Status == StatusError {
			acc.Status = StatusActive
		}
		after = "unlocked"
		return before, after, p.Accounts.Update(ctx, acc)

	default:
		return "", "", fmt.Errorf("未知 op %q", op)
	}
}

// RollbackAIOp restores a field if current still matches after.
func (p *AIPilotService) RollbackAIOp(ctx context.Context, accountID int64, op, before, after string) error {
	acc, err := p.Accounts.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if acc == nil {
		return fmt.Errorf("account %d not found", accountID)
	}
	// If value drifted, skip overwrite.
	current := currentOpValue(acc, op)
	if after != "" && current != after && op != AIOpRelease && op != AIOpUnlock {
		return fmt.Errorf("账号已被手动修改,回滚已跳过(current=%s after=%s)", current, after)
	}
	now := time.Now()
	acc.ManualTouchedAt = &now

	switch op {
	case AIOpSetPriority:
		n, _ := strconv.Atoi(before)
		acc.Priority = n
	case AIOpSetWeight:
		n, _ := strconv.Atoi(before)
		acc.ScheduleWeight = n
	case AIOpDisable, AIOpEnable:
		acc.AIDisabled = before == "true"
	case AIOpSetMaxConcurrency:
		n, _ := strconv.Atoi(before)
		if n > 0 {
			acc.Concurrency = n
		}
	case AIOpSetRPMLimit:
		if acc.Extra == nil {
			acc.Extra = map[string]any{}
		}
		n, _ := strconv.Atoi(before)
		if n <= 0 {
			delete(acc.Extra, "base_rpm")
		} else {
			acc.Extra["base_rpm"] = n
		}
	case AIOpRelease, AIOpUnlock:
		// Cannot perfectly restore timed windows; leave note via error if needed.
		return fmt.Errorf("release/unlock 回滚不会恢复限流窗口,请人工确认")
	default:
		return fmt.Errorf("未知 op %q", op)
	}
	return p.Accounts.Update(ctx, acc)
}

func currentOpValue(acc *Account, op string) string {
	switch op {
	case AIOpSetPriority:
		return strconv.Itoa(acc.Priority)
	case AIOpSetWeight:
		return strconv.Itoa(acc.EffectiveScheduleWeight())
	case AIOpDisable, AIOpEnable:
		return strconv.FormatBool(acc.AIDisabled)
	case AIOpSetMaxConcurrency:
		return strconv.Itoa(acc.Concurrency)
	case AIOpSetRPMLimit:
		return fmt.Sprintf("%v", accountBaseRPM(acc))
	default:
		return ""
	}
}

func accountBaseRPM(acc *Account) int {
	if acc == nil || acc.Extra == nil {
		return 0
	}
	switch v := acc.Extra["base_rpm"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	default:
		return 0
	}
}

func releaseStateSnapshot(acc *Account) string {
	parts := []string{}
	if acc.IsRateLimited() {
		parts = append(parts, "rate_limited")
	}
	if acc.IsOverloaded() {
		parts = append(parts, "overload")
	}
	if acc.TempUnschedulableUntil != nil && time.Now().Before(*acc.TempUnschedulableUntil) {
		parts = append(parts, "temp_unsched:"+acc.TempUnschedulableReason)
	}
	if len(parts) == 0 {
		return "clear"
	}
	return strings.Join(parts, ",")
}
