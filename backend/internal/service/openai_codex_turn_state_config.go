package service

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	defaultCodexTurnStateCacheTTLMinutes           = 43
	minCodexTurnStateCacheTTLMinutes               = 1
	maxCodexTurnStateCacheTTLMinutes               = 24 * 60
	maxCodexTurnStateCacheModels                   = 64
	defaultCodexTurnStateCacheMaxTries             = 8
	minCodexTurnStateCacheMaxTries                 = 1
	maxCodexTurnStateCacheMaxTries                 = 32
	maxCodexTurnStateCacheCountries                = 32
	codexTurnStateRegionPlaceholder                = "{XX}"
	codexTurnStateRefreshModeBlocking              = "blocking"
	codexTurnStateRefreshModeAsync                 = "async"
	defaultCodexTurnStateFailureCooldownSeconds    = 60
	minCodexTurnStateFailureCooldownSeconds        = 5
	maxCodexTurnStateFailureCooldownSeconds        = 30 * 60
	codexTurnStateCooldownReason                   = "turn_state_degraded"
	codexTurnStateFernetVersion              byte  = 0x80
	codexTurnStateHealthyCipherLen                 = 160
	codexTurnStateTeamHealthyCipherLen             = 192
	codexTurnStatePingTimeout                      = 45 * time.Second
	codexTurnStatePingBodyLimit                    = 4096
	codexTurnStateCacheWaiterKey                   = "|"
)

// CodexTurnStateCacheConfig 是最后一跳 X-Codex-Turn-State 自动缓存配置。
// 默认关闭：未勾选模型时不注入、不 ping，避免 API Key 中转被误伤。
type CodexTurnStateCacheConfig struct {
	Enabled                bool     `json:"enabled"`
	IPv6ProxyURL           string   `json:"ipv6_proxy_url"`
	Models                 []string `json:"models"`
	TTLMinutes             int      `json:"ttl_minutes"`
	Countries              []string `json:"countries"`
	MaxPingTries           int      `json:"max_ping_tries"`
	RefreshMode            string   `json:"refresh_mode"`
	FailureCooldownSeconds int      `json:"failure_cooldown_seconds"`
}

func DefaultCodexTurnStateCacheConfig() CodexTurnStateCacheConfig {
	return CodexTurnStateCacheConfig{
		TTLMinutes:             defaultCodexTurnStateCacheTTLMinutes,
		MaxPingTries:           defaultCodexTurnStateCacheMaxTries,
		RefreshMode:            codexTurnStateRefreshModeBlocking,
		FailureCooldownSeconds: defaultCodexTurnStateFailureCooldownSeconds,
	}
}

func (c CodexTurnStateCacheConfig) Normalized() CodexTurnStateCacheConfig {
	out := c
	out.IPv6ProxyURL = strings.TrimSpace(out.IPv6ProxyURL)
	out.Models = uniqueLowerTrimmed(out.Models, maxCodexTurnStateCacheModels)
	out.Countries = uniqueLowerTrimmed(out.Countries, maxCodexTurnStateCacheCountries)
	if out.TTLMinutes < minCodexTurnStateCacheTTLMinutes {
		out.TTLMinutes = defaultCodexTurnStateCacheTTLMinutes
	}
	if out.TTLMinutes > maxCodexTurnStateCacheTTLMinutes {
		out.TTLMinutes = maxCodexTurnStateCacheTTLMinutes
	}
	if out.MaxPingTries < minCodexTurnStateCacheMaxTries {
		out.MaxPingTries = defaultCodexTurnStateCacheMaxTries
	}
	if out.MaxPingTries > maxCodexTurnStateCacheMaxTries {
		out.MaxPingTries = maxCodexTurnStateCacheMaxTries
	}
	switch strings.ToLower(strings.TrimSpace(out.RefreshMode)) {
	case codexTurnStateRefreshModeAsync:
		out.RefreshMode = codexTurnStateRefreshModeAsync
	default:
		out.RefreshMode = codexTurnStateRefreshModeBlocking
	}
	if out.FailureCooldownSeconds <= 0 {
		out.FailureCooldownSeconds = defaultCodexTurnStateFailureCooldownSeconds
	}
	if out.FailureCooldownSeconds < minCodexTurnStateFailureCooldownSeconds {
		out.FailureCooldownSeconds = minCodexTurnStateFailureCooldownSeconds
	}
	if out.FailureCooldownSeconds > maxCodexTurnStateFailureCooldownSeconds {
		out.FailureCooldownSeconds = maxCodexTurnStateFailureCooldownSeconds
	}
	return out
}

func (c CodexTurnStateCacheConfig) TTL() time.Duration {
	return time.Duration(c.Normalized().TTLMinutes) * time.Minute
}

func (c CodexTurnStateCacheConfig) FailureCooldown() time.Duration {
	return time.Duration(c.Normalized().FailureCooldownSeconds) * time.Second
}

func (c CodexTurnStateCacheConfig) EnabledForTraffic() bool {
	n := c.Normalized()
	return n.Enabled && len(n.Models) > 0
}

func (c CodexTurnStateCacheConfig) CoversModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || !c.EnabledForTraffic() {
		return false
	}
	for _, item := range c.Normalized().Models {
		if item == model {
			return true
		}
	}
	return false
}

func (c CodexTurnStateCacheConfig) AsyncRefresh() bool {
	return c.Normalized().RefreshMode == codexTurnStateRefreshModeAsync
}

func (c CodexTurnStateCacheConfig) ValidateIPv6ProxyURL() error {
	raw := strings.TrimSpace(c.IPv6ProxyURL)
	if raw == "" {
		return nil
	}
	sample := strings.ReplaceAll(raw, codexTurnStateRegionPlaceholder, "us")
	parsed, err := url.Parse(sample)
	if err != nil {
		return fmt.Errorf("invalid ipv6_proxy_url: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "socks5", "socks5h", "http", "https":
		return nil
	default:
		return errInvalidCodexTurnStateProxyScheme
	}
}

func (c CodexTurnStateCacheConfig) PingProxyAttempts() []string {
	n := c.Normalized()
	template := n.IPv6ProxyURL
	tries := n.MaxPingTries
	if tries < 1 {
		tries = 1
	}
	out := make([]string, 0, tries)
	if template == "" || !strings.Contains(template, codexTurnStateRegionPlaceholder) || len(n.Countries) == 0 {
		for i := 0; i < tries; i++ {
			out = append(out, template)
		}
		return out
	}
	for i := 0; i < tries; i++ {
		country := n.Countries[i%len(n.Countries)]
		out = append(out, strings.ReplaceAll(template, codexTurnStateRegionPlaceholder, country))
	}
	return out
}

func uniqueLowerTrimmed(values []string, max int) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if max > 0 && len(out) >= max {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
