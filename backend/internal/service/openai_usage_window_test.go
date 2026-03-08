package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type openAIUsageWindowSettingRepoStub struct {
	values map[string]string
}

func (r *openAIUsageWindowSettingRepoStub) Get(_ context.Context, key string) (*Setting, error) {
	if v, ok := r.values[key]; ok {
		return &Setting{Key: key, Value: v}, nil
	}
	return nil, ErrSettingNotFound
}

func (r *openAIUsageWindowSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if v, ok := r.values[key]; ok {
		return v, nil
	}
	return "", ErrSettingNotFound
}

func (r *openAIUsageWindowSettingRepoStub) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func (r *openAIUsageWindowSettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if v, ok := r.values[key]; ok {
			result[key] = v
		}
	}
	return result, nil
}

func (r *openAIUsageWindowSettingRepoStub) SetMultiple(_ context.Context, settings map[string]string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *openAIUsageWindowSettingRepoStub) GetAll(_ context.Context) (map[string]string, error) {
	result := make(map[string]string, len(r.values))
	for key, value := range r.values {
		result[key] = value
	}
	return result, nil
}

func (r *openAIUsageWindowSettingRepoStub) Delete(_ context.Context, key string) error {
	delete(r.values, key)
	return nil
}

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

func TestOpenAIGatewayService_OpenAIUsageWindowConfig_PrefersSettingService(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.UsageWindow.Yellow5HPercent = 85
	cfg.Gateway.OpenAIWS.UsageWindow.Yellow7DPercent = 90
	cfg.Gateway.OpenAIWS.UsageWindow.SnapshotStaleSeconds = 1800

	settingRepo := &openAIUsageWindowSettingRepoStub{values: map[string]string{
		SettingKeyOpenAIUsageWindowYellow5HPercent:   "70",
		SettingKeyOpenAIUsageWindowYellow7DPercent:   "88",
		SettingKeyOpenAIUsageWindowSnapshotStaleSecs: "900",
	}}
	settingService := NewSettingService(settingRepo, cfg)

	svc := &OpenAIGatewayService{cfg: cfg}
	svc.SetSettingService(settingService)

	windowCfg := svc.openAIUsageWindowConfig()
	require.Equal(t, 70.0, windowCfg.Yellow5hPercent)
	require.Equal(t, 88.0, windowCfg.Yellow7dPercent)
	require.Equal(t, 15*time.Minute, windowCfg.StaleTTL)

	updatedSettings := &SystemSettings{
		OpenAIUsageWindowYellow5HPercent:      72,
		OpenAIUsageWindowYellow7DPercent:      86,
		OpenAIUsageWindowSnapshotStaleSeconds: 600,
	}
	require.NoError(t, settingService.UpdateSettings(context.Background(), updatedSettings))

	windowCfg = svc.openAIUsageWindowConfig()
	require.Equal(t, 72.0, windowCfg.Yellow5hPercent)
	require.Equal(t, 86.0, windowCfg.Yellow7dPercent)
	require.Equal(t, 10*time.Minute, windowCfg.StaleTTL)
}
