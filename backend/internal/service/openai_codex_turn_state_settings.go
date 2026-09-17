package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (s *SettingService) GetCodexTurnStateCacheConfig(ctx context.Context) (CodexTurnStateCacheConfig, error) {
	cfg := DefaultCodexTurnStateCacheConfig()
	if s == nil || s.settingRepo == nil {
		return cfg, nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyCodexTurnStateCacheSettings)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return cfg, fmt.Errorf("get codex turn-state cache settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return cfg, nil
	}
	var stored CodexTurnStateCacheConfig
	if json.Unmarshal([]byte(value), &stored) != nil {
		return cfg, nil
	}
	return stored.Normalized(), nil
}

func (s *SettingService) SetCodexTurnStateCacheConfig(ctx context.Context, cfg CodexTurnStateCacheConfig) (CodexTurnStateCacheConfig, error) {
	if s == nil || s.settingRepo == nil {
		return DefaultCodexTurnStateCacheConfig(), errors.New("setting service unavailable")
	}
	normalized := cfg.Normalized()
	if err := normalized.ValidateIPv6ProxyURL(); err != nil {
		return normalized, err
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return normalized, fmt.Errorf("marshal codex turn-state cache settings: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyCodexTurnStateCacheSettings, string(payload)); err != nil {
		return normalized, err
	}
	return normalized, nil
}
