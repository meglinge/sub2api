package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexTurnStateCache struct {
	cfg atomic.Pointer[CodexTurnStateCacheConfig]

	mu        sync.Mutex
	persistMu sync.Mutex
	waiters   map[string]*codexTurnStateRefreshWaiter

	statsMu   sync.RWMutex
	stats     map[string]*CodexTurnStateRefreshStat
	events    []CodexTurnStateRefreshEvent
	eventHead int
	eventSeq  int64
	cooldowns map[string]CodexTurnStateCooldown
}

type codexTurnStateRefreshWaiter struct {
	done  chan struct{}
	value string
	err   error
}

func (s *OpenAIGatewayService) getCodexTurnStateCache() *codexTurnStateCache {
	if s == nil {
		return nil
	}
	s.codexTurnStateCacheOnce.Do(func() {
		if s.codexTurnStateCache == nil {
			s.codexTurnStateCache = &codexTurnStateCache{
				waiters:   map[string]*codexTurnStateRefreshWaiter{},
				stats:     map[string]*CodexTurnStateRefreshStat{},
				cooldowns: map[string]CodexTurnStateCooldown{},
			}
		}
	})
	return s.codexTurnStateCache
}

func (s *OpenAIGatewayService) codexTurnStateCacheConfig(ctx context.Context) CodexTurnStateCacheConfig {
	cache := s.getCodexTurnStateCache()
	if cache != nil {
		if loaded := cache.cfg.Load(); loaded != nil {
			return *loaded
		}
	}
	cfg := DefaultCodexTurnStateCacheConfig()
	if s != nil && s.settingService != nil {
		if stored, err := s.settingService.GetCodexTurnStateCacheConfig(ctx); err == nil {
			cfg = stored
		}
	}
	if cache != nil {
		copied := cfg
		cache.cfg.Store(&copied)
	}
	return cfg
}

func (s *OpenAIGatewayService) SetCodexTurnStateCacheConfig(cfg CodexTurnStateCacheConfig) {
	cache := s.getCodexTurnStateCache()
	if cache == nil {
		return
	}
	normalized := cfg.Normalized()
	cache.cfg.Store(&normalized)
}

func (s *OpenAIGatewayService) isCodexTurnStateCellCooling(account *Account, model string) bool {
	if s == nil || s.codexTurnStateCache == nil || account == nil || !account.UsesOpenAICodexProtocol() {
		return false
	}
	canonical := canonicalOpenAIAccountSchedulingModel(account, model)
	return s.codexTurnStateCache.isCooling(account.ID, canonical)
}

func (s *OpenAIGatewayService) CodexTurnStateRefreshStats() []CodexTurnStateRefreshStat {
	cache := s.getCodexTurnStateCache()
	if cache == nil {
		return nil
	}
	return cache.snapshotStats()
}

func (s *OpenAIGatewayService) CodexTurnStateRefreshEvents(limit int) []CodexTurnStateRefreshEvent {
	cache := s.getCodexTurnStateCache()
	if cache == nil {
		return nil
	}
	return cache.snapshotEvents(limit)
}

func (s *OpenAIGatewayService) ClearCodexTurnStateCooldown(account *Account, model string) bool {
	cache := s.getCodexTurnStateCache()
	if cache == nil || account == nil {
		return false
	}
	return cache.clearCooldown(account.ID, model)
}

func (s *OpenAIGatewayService) InvalidateCodexTurnState(ctx context.Context, account *Account, model string) {
	if account == nil {
		return
	}
	account.ClearCodexTurnState(model)
	s.persistCodexTurnState(ctx, account)
}

func (s *OpenAIGatewayService) ForceRefreshCodexTurnState(ctx context.Context, account *Account, model string) error {
	if s == nil || account == nil || !account.UsesOpenAICodexProtocol() {
		return errors.New("仅 ChatGPT Codex 协议账号支持 turn-state 刷新")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("缺少 model")
	}
	account.ClearCodexTurnState(model)
	s.getCodexTurnStateCache().clearCooldown(account.ID, model)
	_, err := s.refreshCodexTurnState(ctx, account, model, s.codexTurnStateCacheConfig(ctx))
	return err
}

func (s *OpenAIGatewayService) prepareCodexTurnState(ctx context.Context, c *gin.Context, account *Account, body []byte) ([]byte, error) {
	if s == nil || account == nil || !account.UsesOpenAICodexProtocol() {
		return body, nil
	}
	cfg := s.codexTurnStateCacheConfig(ctx)
	if !cfg.EnabledForTraffic() {
		return body, nil
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if !cfg.CoversModel(model) {
		return body, nil
	}
	if err := s.ensureCodexTurnStateReady(ctx, account, model, cfg); err != nil {
		return body, err
	}
	body = injectStoredCodexTurnStateBody(ctx, account, body)
	if c != nil && c.Request != nil {
		if stored := account.GetCodexTurnState(model); stored != "" {
			c.Request.Header.Set(openAICodexTurnStateHeader, stored)
		}
	}
	return body, nil
}

func (s *OpenAIGatewayService) ensureCodexTurnStateReady(ctx context.Context, account *Account, model string, cfg CodexTurnStateCacheConfig) error {
	if skipStoredCodexTurnState(ctx) {
		return nil
	}
	cache := s.getCodexTurnStateCache()
	if cache == nil {
		return nil
	}
	if cache.isCooling(account.ID, model) {
		return s.codexTurnStateUnavailableError(account, "turn-state cooling")
	}
	now := time.Now()
	if stored, fresh := account.CodexTurnStateFresh(model, cfg.TTL(), now); stored != "" && fresh {
		if err := verifyCodexTurnStatePingIntelligence(stored, account.codexPlanType()); err == nil {
			return nil
		}
		account.ClearCodexTurnState(model)
	} else if stored != "" && cfg.AsyncRefresh() {
		go func() {
			bg := context.WithoutCancel(ctx)
			_, _ = s.refreshCodexTurnState(bg, account, model, cfg)
		}()
		return nil
	}
	if cfg.AsyncRefresh() && account.GetCodexTurnState(model) == "" {
		go func() {
			bg := context.WithoutCancel(ctx)
			_, _ = s.refreshCodexTurnState(bg, account, model, cfg)
		}()
		return s.codexTurnStateUnavailableError(account, "turn-state missing")
	}
	_, err := s.refreshCodexTurnState(ctx, account, model, cfg)
	return err
}

func (s *OpenAIGatewayService) refreshCodexTurnState(ctx context.Context, account *Account, model string, cfg CodexTurnStateCacheConfig) (string, error) {
	cache := s.getCodexTurnStateCache()
	if cache == nil {
		return "", errors.New("turn-state cache unavailable")
	}
	key := fmt.Sprintf("%d%s%s", account.ID, codexTurnStateCacheWaiterKey, strings.ToLower(strings.TrimSpace(model)))
	cache.mu.Lock()
	if waiter, ok := cache.waiters[key]; ok {
		cache.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-waiter.done:
			return waiter.value, waiter.err
		}
	}
	waiter := &codexTurnStateRefreshWaiter{done: make(chan struct{})}
	cache.waiters[key] = waiter
	cache.mu.Unlock()

	start := time.Now()
	value, pings, err := s.pingCodexTurnState(ctx, account, model, cfg)
	cache.recordRefresh(account, model, pings, time.Since(start), err)
	if err == nil {
		account.SetCodexTurnState(model, value, time.Now())
		s.persistCodexTurnState(ctx, account)
		cache.clearCooldown(account.ID, model)
		s.clearOpenAIAccountModelTransientState(account.ID, model)
	} else {
		stat := cache.statFor(account.ID, model)
		level := stat.ConsecutiveFails
		if level < 1 {
			level = 1
		}
		cooldown := cfg.FailureCooldown()
		for i := 1; i < level && cooldown < 30*time.Minute; i++ {
			cooldown *= 2
		}
		if cooldown > 30*time.Minute {
			cooldown = 30 * time.Minute
		}
		cache.setCooldown(account.ID, model, time.Now().Add(cooldown), level)
	}

	cache.mu.Lock()
	waiter.value = value
	waiter.err = err
	close(waiter.done)
	delete(cache.waiters, key)
	cache.mu.Unlock()
	if err != nil {
		return "", s.codexTurnStateUnavailableError(account, err.Error())
	}
	return value, nil
}

func (s *OpenAIGatewayService) persistCodexTurnState(ctx context.Context, account *Account) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}
	cache := s.getCodexTurnStateCache()
	if cache != nil {
		cache.persistMu.Lock()
		defer cache.persistMu.Unlock()
	}
	states := extraStringMap(account.Extra, extraKeyCodexTurnStates)
	captured := extraInt64Map(account.Extra, extraKeyCodexTurnStateCapturedAt)
	if states == nil {
		states = map[string]string{}
	}
	if captured == nil {
		captured = map[string]int64{}
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		extraKeyCodexTurnStates:          states,
		extraKeyCodexTurnStateCapturedAt: captured,
	}); err != nil {
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] persist turn-state failed account=%d err=%v", account.ID, err)
	}
}

func (s *OpenAIGatewayService) observeInboundCodexTurnState(account *Account, headers http.Header) {
	if s == nil || account == nil || headers == nil || !account.UsesOpenAICodexProtocol() {
		return
	}
	token := strings.TrimSpace(headers.Get(openAICodexTurnStateHeader))
	if token == "" {
		return
	}
	// 没有模型上下文时只校验：若当前任意缓存值与上游不一致，等下次请求再刷。
	states := extraStringMap(account.Extra, extraKeyCodexTurnStates)
	for model, stored := range states {
		if stored != "" && stored != token {
			if err := verifyCodexTurnStatePingIntelligence(token, account.codexPlanType()); err == nil {
				account.SetCodexTurnState(model, token, time.Now())
				s.persistCodexTurnState(context.Background(), account)
			} else {
				account.ClearCodexTurnState(model)
				s.persistCodexTurnState(context.Background(), account)
			}
			return
		}
	}
}

func (s *OpenAIGatewayService) injectStoredCodexTurnStateHeaders(ctx context.Context, account *Account, model string, headers http.Header) http.Header {
	if skipStoredCodexTurnState(ctx) || account == nil || headers == nil {
		return headers
	}
	stored := account.GetCodexTurnState(model)
	if stored == "" {
		return headers
	}
	headers.Set(openAICodexTurnStateHeader, stored)
	return headers
}

func injectStoredCodexTurnStateBody(ctx context.Context, account *Account, body []byte) []byte {
	if skipStoredCodexTurnState(ctx) || account == nil || len(body) == 0 {
		return body
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	stored := account.GetCodexTurnState(model)
	if stored == "" {
		return body
	}
	if meta := gjson.GetBytes(body, "client_metadata"); meta.IsObject() {
		if updated, err := sjson.SetBytes(body, "client_metadata.x-codex-turn-state", stored); err == nil {
			return updated
		}
	}
	return body
}

func skipStoredCodexTurnState(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	skip, _ := ctx.Value(skipStoredCodexTurnStateKey{}).(bool)
	return skip
}

func withSkipStoredCodexTurnState(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, skipStoredCodexTurnStateKey{}, true)
}

func (s *OpenAIGatewayService) pingCodexTurnState(ctx context.Context, account *Account, model string, cfg CodexTurnStateCacheConfig) (string, int, error) {
	attempts := cfg.PingProxyAttempts()
	if len(attempts) == 0 {
		attempts = []string{""}
	}
	var lastErr error
	for i, proxyURL := range attempts {
		select {
		case <-ctx.Done():
			return "", i, ctx.Err()
		default:
		}
		value, err := s.pingCodexTurnStateOnce(ctx, account, model, proxyURL)
		if err == nil {
			return value, i + 1, nil
		}
		lastErr = err
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] turn-state ping failed account=%d model=%s try=%d/%d err=%v", account.ID, model, i+1, len(attempts), err)
	}
	if lastErr == nil {
		lastErr = errors.New("turn-state ping failed")
	}
	return "", len(attempts), lastErr
}

func (s *OpenAIGatewayService) pingCodexTurnStateOnce(ctx context.Context, account *Account, model string, proxyURL string) (string, error) {
	if s == nil || s.httpUpstream == nil {
		return "", errors.New("http upstream unavailable")
	}
	pingCtx, cancel := context.WithTimeout(withSkipStoredCodexTurnState(ctx), codexTurnStatePingTimeout)
	defer cancel()
	token, _, err := s.GetAccessToken(pingCtx, account)
	if err != nil {
		return "", err
	}
	payload := createOpenAITestPayload(model, true)
	payload["instructions"] = openai.DefaultInstructions
	encoded, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return "", marshalErr
	}
	req, err := http.NewRequestWithContext(pingCtx, http.MethodPost, chatgptCodexURL, bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Host = "chatgpt.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	applyOpenAICodexProbeHeaders(req.Header)
	authHeaders, err := s.buildOpenAIAuthenticationHeaders(pingCtx, account, token)
	if err != nil {
		return "", err
	}
	for key, values := range authHeaders {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if err := resolveAndSetOpenAIChatGPTAccountHeaders(pingCtx, s.accountRepo, req.Header, account); err != nil {
		return "", err
	}
	enforceCodexIdentityHeadersWithUA(req.Header, s.codexIdentityOverrideUA(account))
	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("ping status %d: %s", resp.StatusCode, strings.TrimSpace(string(limited)))
	}
	value := collectCodexTurnStateFromPing(resp)
	if err := verifyCodexTurnStatePingIntelligence(value, account.codexPlanType()); err != nil {
		return "", err
	}
	return value, nil
}

func collectCodexTurnStateFromPing(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	if value := strings.TrimSpace(resp.Header.Get(openAICodexTurnStateHeader)); value != "" {
		return value
	}
	reader := bufio.NewReader(io.LimitReader(resp.Body, codexTurnStatePingBodyLimit))
	for {
		line, err := reader.ReadString('\n')
		if value := extractCodexTurnStateFromSSELine(line); value != "" {
			return value
		}
		if err != nil {
			return ""
		}
	}
}

func extractCodexTurnStateFromSSELine(line string) string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return ""
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return ""
	}
	paths := []string{
		"response.metadata.x-codex-turn-state",
		"metadata.x-codex-turn-state",
		"x-codex-turn-state",
	}
	for _, path := range paths {
		if value := strings.TrimSpace(gjson.Get(data, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func (s *OpenAIGatewayService) codexTurnStateUnavailableError(account *Account, message string) error {
	return s.newOpenAIAccountFailoverError(account, http.StatusServiceUnavailable, nil, nil, message, false, false)
}
