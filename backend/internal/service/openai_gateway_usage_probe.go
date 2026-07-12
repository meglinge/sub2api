package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// shouldProbeOpenAICodexSnapshot 基于进程内缓存节流 OpenAI Codex usage snapshot 探测，
// 每个账号在 openAIProbeCacheTTL 内至多触发一次探测。
func (s *OpenAIGatewayService) shouldProbeOpenAICodexSnapshot(accountID int64, now time.Time) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	if cached, ok := s.openaiUsageProbeCache.Load(accountID); ok {
		if ts, ok := cached.(time.Time); ok && now.Sub(ts) < openAIProbeCacheTTL {
			return false
		}
	}
	s.openaiUsageProbeCache.Store(accountID, now)
	return true
}

// maybeRefreshOpenAIUsageWindowAsync 在 usage window 评估需要探测时，异步刷新账号的 Codex usage snapshot。
func (s *OpenAIGatewayService) maybeRefreshOpenAIUsageWindowAsync(account *Account, eval openAIUsageWindowEvaluation) {
	if s == nil || account == nil || !eval.NeedsProbe {
		return
	}
	if !account.IsOpenAIOAuth() {
		return
	}
	if strings.TrimSpace(account.GetOpenAIAccessToken()) == "" {
		return
	}
	now := time.Now()
	if !s.shouldProbeOpenAICodexSnapshot(account.ID, now) {
		return
	}

	accountCopy := *account
	go func() {
		probeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		probeSvc := &AccountUsageService{
			accountRepo: s.accountRepo,
		}
		if _, err := probeSvc.probeOpenAICodexSnapshot(probeCtx, &accountCopy); err != nil {
			logger.LegacyPrintf("service.openai_gateway", "openai codex usage probe failed: account=%d err=%v", accountCopy.ID, err)
		}
	}()
}

// SetSettingService 注入 SettingService（供 usage window 阈值配置读取）。
func (s *OpenAIGatewayService) SetSettingService(settingService *SettingService) {
	if s == nil {
		return
	}
	s.settingService = settingService
}

// openAIUsageWindowConfig 返回当前生效的 usage window 阈值配置，优先取 SettingService 动态配置，
// 回退到静态 config，再回退到内置默认值。
func (s *OpenAIGatewayService) openAIUsageWindowConfig() openAIUsageWindowConfig {
	if s != nil && s.settingService != nil {
		return s.settingService.GetOpenAIUsageWindowConfig(context.Background())
	}
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
