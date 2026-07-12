package service

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// OpenAI usage window 调度阈值配置的进程内缓存（60s TTL），与 version bounds 等缓存同构。
type cachedOpenAIUsageWindow struct {
	cfg       openAIUsageWindowConfig
	expiresAt int64
}

var openAIUsageWindowCache atomic.Value // *cachedOpenAIUsageWindow
var openAIUsageWindowSF singleflight.Group

const openAIUsageWindowCacheTTL = 60 * time.Second
const openAIUsageWindowErrorTTL = 5 * time.Second

func (s *SettingService) getFloat64OrDefault(settings map[string]string, key string, defaultValue float64) float64 {
	if value, ok := settings[key]; ok && strings.TrimSpace(value) != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func (s *SettingService) getIntOrDefault(settings map[string]string, key string, defaultValue int) int {
	if value, ok := settings[key]; ok && strings.TrimSpace(value) != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func (s *SettingService) defaultOpenAIUsageWindowConfig() openAIUsageWindowConfig {
	cfg := defaultOpenAIUsageWindowConfig()
	if s == nil || s.cfg == nil {
		return cfg
	}
	if s.cfg.Gateway.OpenAIWS.UsageWindow.Yellow5HPercent > 0 {
		cfg.Yellow5hPercent = s.cfg.Gateway.OpenAIWS.UsageWindow.Yellow5HPercent
	}
	if s.cfg.Gateway.OpenAIWS.UsageWindow.Yellow7DPercent > 0 {
		cfg.Yellow7dPercent = s.cfg.Gateway.OpenAIWS.UsageWindow.Yellow7DPercent
	}
	if s.cfg.Gateway.OpenAIWS.UsageWindow.SnapshotStaleSeconds > 0 {
		cfg.StaleTTL = time.Duration(s.cfg.Gateway.OpenAIWS.UsageWindow.SnapshotStaleSeconds) * time.Second
	}
	return cfg
}

func (s *SettingService) refreshOpenAIUsageWindowCache(cfg openAIUsageWindowConfig, ttl time.Duration) {
	openAIUsageWindowCache.Store(&cachedOpenAIUsageWindow{
		cfg:       cfg,
		expiresAt: time.Now().Add(ttl).UnixNano(),
	})
}

// GetOpenAIUsageWindowConfig 返回 OpenAI usage window 调度阈值配置（带 60s 进程内缓存 + singleflight）。
func (s *SettingService) GetOpenAIUsageWindowConfig(ctx context.Context) openAIUsageWindowConfig {
	defaultCfg := s.defaultOpenAIUsageWindowConfig()
	if cached, ok := openAIUsageWindowCache.Load().(*cachedOpenAIUsageWindow); ok && cached != nil {
		if cached.expiresAt > time.Now().UnixNano() {
			return cached.cfg
		}
	}

	const sfKey = "openai_usage_window_config"
	value, err, _ := openAIUsageWindowSF.Do(sfKey, func() (any, error) {
		if cached, ok := openAIUsageWindowCache.Load().(*cachedOpenAIUsageWindow); ok && cached != nil {
			if cached.expiresAt > time.Now().UnixNano() {
				return cached.cfg, nil
			}
		}
		if s == nil || s.settingRepo == nil {
			s.refreshOpenAIUsageWindowCache(defaultCfg, openAIUsageWindowCacheTTL)
			return defaultCfg, nil
		}
		queryCtx := ctx
		if queryCtx == nil {
			queryCtx = context.Background()
		}
		values, err := s.settingRepo.GetMultiple(queryCtx, []string{
			SettingKeyOpenAIUsageWindowYellow5HPercent,
			SettingKeyOpenAIUsageWindowYellow7DPercent,
			SettingKeyOpenAIUsageWindowSnapshotStaleSecs,
		})
		if err != nil {
			s.refreshOpenAIUsageWindowCache(defaultCfg, openAIUsageWindowErrorTTL)
			return defaultCfg, err
		}

		cfg := defaultCfg
		cfg.Yellow5hPercent = s.getFloat64OrDefault(values, SettingKeyOpenAIUsageWindowYellow5HPercent, defaultCfg.Yellow5hPercent)
		cfg.Yellow7dPercent = s.getFloat64OrDefault(values, SettingKeyOpenAIUsageWindowYellow7DPercent, defaultCfg.Yellow7dPercent)
		cfg.StaleTTL = time.Duration(s.getIntOrDefault(values, SettingKeyOpenAIUsageWindowSnapshotStaleSecs, int(defaultCfg.StaleTTL/time.Second))) * time.Second
		s.refreshOpenAIUsageWindowCache(cfg, openAIUsageWindowCacheTTL)
		return cfg, nil
	})
	if err != nil {
		return defaultCfg
	}
	if cfg, ok := value.(openAIUsageWindowConfig); ok {
		return cfg
	}
	return defaultCfg
}
