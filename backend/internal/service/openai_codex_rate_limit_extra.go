package service

import (
	"context"
	"time"
)

func codexUsagePercentExhausted(value *float64) bool {
	return value != nil && *value >= 100-1e-9
}

func codexRateLimitResetAtFromSnapshot(snapshot *OpenAICodexUsageSnapshot, fallbackNow time.Time) *time.Time {
	if snapshot == nil {
		return nil
	}
	normalized := snapshot.Normalize()
	if normalized == nil {
		return nil
	}
	baseTime := codexSnapshotBaseTime(snapshot, fallbackNow)
	if codexUsagePercentExhausted(normalized.Used7dPercent) && normalized.Reset7dSeconds != nil {
		resetAt := baseTime.Add(time.Duration(*normalized.Reset7dSeconds) * time.Second)
		return &resetAt
	}
	if codexUsagePercentExhausted(normalized.Used5hPercent) && normalized.Reset5hSeconds != nil {
		resetAt := baseTime.Add(time.Duration(*normalized.Reset5hSeconds) * time.Second)
		return &resetAt
	}
	return nil
}

func codexRateLimitResetAtFromExtra(extra map[string]any, now time.Time) *time.Time {
	if len(extra) == 0 {
		return nil
	}
	if progress := buildCodexUsageProgressFromExtra(extra, "7d", now); progress != nil && codexUsagePercentExhausted(&progress.Utilization) && progress.ResetsAt != nil && now.Before(*progress.ResetsAt) {
		resetAt := progress.ResetsAt.UTC()
		return &resetAt
	}
	if progress := buildCodexUsageProgressFromExtra(extra, "5h", now); progress != nil && codexUsagePercentExhausted(&progress.Utilization) && progress.ResetsAt != nil && now.Before(*progress.ResetsAt) {
		resetAt := progress.ResetsAt.UTC()
		return &resetAt
	}
	return nil
}

func applyOpenAICodexRateLimitFromExtra(account *Account, now time.Time) (*time.Time, bool) {
	if account == nil || !account.IsOpenAI() {
		return nil, false
	}
	resetAt := codexRateLimitResetAtFromExtra(account.Extra, now)
	if resetAt == nil {
		return nil, false
	}
	if account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) && !account.RateLimitResetAt.Before(*resetAt) {
		return account.RateLimitResetAt, false
	}
	account.RateLimitResetAt = resetAt
	return resetAt, true
}

func syncOpenAICodexRateLimitFromExtra(ctx context.Context, repo AccountRepository, account *Account, now time.Time) *time.Time {
	resetAt, changed := applyOpenAICodexRateLimitFromExtra(account, now)
	if !changed || resetAt == nil || repo == nil || account == nil || account.ID <= 0 {
		return resetAt
	}
	_ = repo.SetRateLimited(ctx, account.ID, *resetAt)
	return resetAt
}
