package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEvaluateOpenAIUsageWindow_NoSnapshotIsUnknown(t *testing.T) {
	eval := evaluateOpenAIUsageWindow(&Account{
		ID:       1,
		Platform: PlatformOpenAI,
	}, time.Now())

	require.Equal(t, openAIUsageWindowStateUnknown, eval.State)
	require.True(t, eval.NeedsProbe)
}

func TestEvaluateOpenAIUsageWindow_FutureResetRemainsRed(t *testing.T) {
	now := time.Now().UTC()
	resetAt := now.Add(20 * time.Minute).Format(time.RFC3339)
	updatedAt := now.Format(time.RFC3339)

	eval := evaluateOpenAIUsageWindow(&Account{
		ID:       2,
		Platform: PlatformOpenAI,
		Extra: map[string]any{
			"codex_5h_used_percent":  100.0,
			"codex_5h_reset_at":      resetAt,
			"codex_usage_updated_at": updatedAt,
		},
	}, now)

	require.Equal(t, openAIUsageWindowStateRed, eval.State)
	require.False(t, eval.NeedsProbe)
}

func TestEvaluateOpenAIUsageWindow_PastResetTurnsUnknown(t *testing.T) {
	now := time.Now().UTC()
	resetAt := now.Add(-2 * time.Minute).Format(time.RFC3339)
	updatedAt := now.Add(-10 * time.Minute).Format(time.RFC3339)

	eval := evaluateOpenAIUsageWindow(&Account{
		ID:       3,
		Platform: PlatformOpenAI,
		Extra: map[string]any{
			"codex_5h_used_percent":  100.0,
			"codex_5h_reset_at":      resetAt,
			"codex_usage_updated_at": updatedAt,
		},
	}, now)

	require.Equal(t, openAIUsageWindowStateUnknown, eval.State)
	require.True(t, eval.NeedsProbe)
}

func TestEvaluateOpenAIUsageWindow_StaleYellowTurnsUnknown(t *testing.T) {
	now := time.Now().UTC()
	updatedAt := now.Add(-(openAIUsageWindowStaleTTLDefault + 5*time.Minute)).Format(time.RFC3339)

	eval := evaluateOpenAIUsageWindow(&Account{
		ID:       4,
		Platform: PlatformOpenAI,
		Extra: map[string]any{
			"codex_7d_used_percent":  95.0,
			"codex_usage_updated_at": updatedAt,
		},
	}, now)

	require.Equal(t, openAIUsageWindowStateUnknown, eval.State)
	require.True(t, eval.NeedsProbe)
}

func TestEvaluateOpenAIUsageWindowWithConfig_UsesCustomThresholds(t *testing.T) {
	now := time.Now().UTC()
	updatedAt := now.Format(time.RFC3339)

	eval := evaluateOpenAIUsageWindowWithConfig(&Account{
		ID:       5,
		Platform: PlatformOpenAI,
		Extra: map[string]any{
			"codex_5h_used_percent":  75.0,
			"codex_usage_updated_at": updatedAt,
		},
	}, now, openAIUsageWindowConfig{
		Yellow5hPercent: 70,
		Yellow7dPercent: 90,
		StaleTTL:        30 * time.Minute,
	})

	require.Equal(t, openAIUsageWindowStateYellow, eval.State)
	require.False(t, eval.NeedsProbe)
}
