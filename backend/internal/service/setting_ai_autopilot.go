package service

import (
	"context"
	"encoding/json"
	"strings"
)

// GetAIAutopilotSettings loads autopilot config from settings store.
//
// Important: start from a zero value (not DefaultAIAutopilotSettings) before
// Unmarshal. Default used to share one *bool across all op switches; json
// unmarshals into existing pointees, so every switch would collapse to the last
// decoded field and "uncheck + save" would appear broken.
func (s *SettingService) GetAIAutopilotSettings(ctx context.Context) AIAutopilotSettings {
	if s == nil || s.settingRepo == nil {
		return DefaultAIAutopilotSettings().Normalize()
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyAIAutopilot)
	if err != nil || strings.TrimSpace(raw) == "" {
		return DefaultAIAutopilotSettings().Normalize()
	}
	var cfg AIAutopilotSettings
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return DefaultAIAutopilotSettings().Normalize()
	}
	return cfg.Normalize()
}

// SetAIAutopilotSettings persists autopilot config (masks are not applied here;
// callers should preserve existing API key when the form sends a masked value).
func (s *SettingService) SetAIAutopilotSettings(ctx context.Context, cfg AIAutopilotSettings) error {
	cfg = cfg.Normalize()
	// Preserve existing API key when client sends empty or masked value.
	existing := s.GetAIAutopilotSettings(ctx)
	if cfg.APIKey == "" || strings.Contains(cfg.APIKey, "*") {
		cfg.APIKey = existing.APIKey
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.settingRepo.Set(ctx, SettingKeyAIAutopilot, string(raw))
}

// MaskAIAutopilotSettings returns a copy safe for API responses.
func MaskAIAutopilotSettings(cfg AIAutopilotSettings) AIAutopilotSettings {
	if cfg.APIKey != "" {
		cfg.APIKey = maskSecret(cfg.APIKey)
	}
	// Ensure op switches never alias after mask (defensive copy of pointers).
	return cfg.Normalize()
}

func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + strings.Repeat("*", len(s)-8) + s[len(s)-4:]
}
