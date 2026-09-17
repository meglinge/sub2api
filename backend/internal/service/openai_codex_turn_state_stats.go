package service

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	codexTurnStateRefreshEventCap      = 300
	codexTurnStateChronicFailureThresh = 3
)

type CodexTurnStateRefreshStat struct {
	AccountID              int64     `json:"account_id"`
	Model                  string    `json:"model"`
	ConsecutiveFails       int       `json:"consecutive_fails"`
	TotalAttempts          int       `json:"total_attempts"`
	TotalSuccesses         int       `json:"total_successes"`
	LastPingCount          int       `json:"last_ping_count"`
	LastDurationMs         int64     `json:"last_duration_ms"`
	LastAttemptAt          time.Time `json:"last_attempt_at"`
	LastSuccessAt          time.Time `json:"last_success_at"`
	LastFailureKind        string    `json:"last_failure_kind,omitempty"`
	LastFailureDetail      string    `json:"last_failure_detail,omitempty"`
	LastDegradedCipherLen  int       `json:"last_degraded_cipher_len"`
	LastExpectedCipherLen  int       `json:"last_expected_cipher_len"`
}

type CodexTurnStateRefreshEvent struct {
	Seq         int64     `json:"seq"`
	At          time.Time `json:"at"`
	AccountID   int64     `json:"account_id"`
	Model       string    `json:"model"`
	OK          bool      `json:"ok"`
	PingCount   int       `json:"ping_count"`
	DurationMs  int64     `json:"duration_ms"`
	FailureKind string    `json:"failure_kind,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	CipherLen   int       `json:"cipher_len"`
	ExpectedLen int       `json:"expected_cipher_len"`
}

type CodexTurnStateCooldown struct {
	Until         time.Time
	Reason        string
	BackoffLevel  int
}

func (c *codexTurnStateCache) recordRefresh(account *Account, model string, pings int, elapsed time.Duration, err error) {
	if c == nil || account == nil || pings <= 0 {
		return
	}
	model = strings.ToLower(strings.TrimSpace(model))
	key := fmt.Sprintf("%d%s%s", account.ID, codexTurnStateCacheWaiterKey, model)
	now := time.Now()
	event := CodexTurnStateRefreshEvent{
		At:         now,
		AccountID:  account.ID,
		Model:      model,
		OK:         err == nil,
		PingCount:  pings,
		DurationMs: elapsed.Milliseconds(),
	}
	var degraded *CodexTurnStateDegradedError
	if err != nil {
		event.Detail = err.Error()
		event.FailureKind = "refresh_failed"
		if errors.As(err, &degraded) {
			event.FailureKind = "degraded"
			event.CipherLen = degraded.Health.CipherLen
			event.ExpectedLen = degraded.Health.ExpectedCipherLen
		}
	}

	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	stat := c.stats[key]
	if stat == nil {
		stat = &CodexTurnStateRefreshStat{AccountID: account.ID, Model: model}
		c.stats[key] = stat
	}
	stat.TotalAttempts++
	stat.LastPingCount = pings
	stat.LastDurationMs = elapsed.Milliseconds()
	stat.LastAttemptAt = now
	if err == nil {
		stat.ConsecutiveFails = 0
		stat.TotalSuccesses++
		stat.LastSuccessAt = now
		stat.LastFailureKind = ""
		stat.LastFailureDetail = ""
	} else {
		stat.ConsecutiveFails++
		stat.LastFailureKind = event.FailureKind
		stat.LastFailureDetail = event.Detail
		if degraded != nil {
			stat.LastDegradedCipherLen = degraded.Health.CipherLen
			stat.LastExpectedCipherLen = degraded.Health.ExpectedCipherLen
		}
	}
	c.eventSeq++
	event.Seq = c.eventSeq
	if len(c.events) < codexTurnStateRefreshEventCap {
		c.events = append(c.events, event)
		return
	}
	c.events[c.eventHead] = event
	c.eventHead = (c.eventHead + 1) % codexTurnStateRefreshEventCap
}

func (c *codexTurnStateCache) snapshotStats() []CodexTurnStateRefreshStat {
	if c == nil {
		return nil
	}
	c.statsMu.RLock()
	defer c.statsMu.RUnlock()
	out := make([]CodexTurnStateRefreshStat, 0, len(c.stats))
	for _, stat := range c.stats {
		if stat != nil {
			out = append(out, *stat)
		}
	}
	return out
}

func (c *codexTurnStateCache) snapshotEvents(limit int) []CodexTurnStateRefreshEvent {
	if c == nil {
		return nil
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 300 {
		limit = 300
	}
	c.statsMu.RLock()
	defer c.statsMu.RUnlock()
	n := len(c.events)
	if n == 0 {
		return nil
	}
	ordered := make([]CodexTurnStateRefreshEvent, 0, n)
	if n < cap(c.events) || c.eventHead == 0 {
		ordered = append(ordered, c.events...)
	} else {
		ordered = append(ordered, c.events[c.eventHead:]...)
		ordered = append(ordered, c.events[:c.eventHead]...)
	}
	for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
		ordered[i], ordered[j] = ordered[j], ordered[i]
	}
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered
}

func (c *codexTurnStateCache) statFor(accountID int64, model string) CodexTurnStateRefreshStat {
	key := fmt.Sprintf("%d%s%s", accountID, codexTurnStateCacheWaiterKey, strings.ToLower(strings.TrimSpace(model)))
	c.statsMu.RLock()
	defer c.statsMu.RUnlock()
	if stat := c.stats[key]; stat != nil {
		return *stat
	}
	return CodexTurnStateRefreshStat{AccountID: accountID, Model: model}
}

func (c *codexTurnStateCache) setCooldown(accountID int64, model string, until time.Time, level int) {
	if c == nil || accountID <= 0 {
		return
	}
	model = strings.ToLower(strings.TrimSpace(model))
	key := fmt.Sprintf("%d%s%s", accountID, codexTurnStateCacheWaiterKey, model)
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	if c.cooldowns == nil {
		c.cooldowns = map[string]CodexTurnStateCooldown{}
	}
	c.cooldowns[key] = CodexTurnStateCooldown{
		Until:        until,
		Reason:       codexTurnStateCooldownReason,
		BackoffLevel: level,
	}
}

func (c *codexTurnStateCache) clearCooldown(accountID int64, model string) bool {
	if c == nil {
		return false
	}
	key := fmt.Sprintf("%d%s%s", accountID, codexTurnStateCacheWaiterKey, strings.ToLower(strings.TrimSpace(model)))
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	if _, ok := c.cooldowns[key]; !ok {
		return false
	}
	delete(c.cooldowns, key)
	return true
}

func (c *codexTurnStateCache) cooldownFor(accountID int64, model string) (CodexTurnStateCooldown, bool) {
	if c == nil {
		return CodexTurnStateCooldown{}, false
	}
	key := fmt.Sprintf("%d%s%s", accountID, codexTurnStateCacheWaiterKey, strings.ToLower(strings.TrimSpace(model)))
	c.statsMu.RLock()
	defer c.statsMu.RUnlock()
	cool, ok := c.cooldowns[key]
	if !ok || (!cool.Until.IsZero() && time.Now().After(cool.Until)) {
		return CodexTurnStateCooldown{}, false
	}
	return cool, true
}

func (c *codexTurnStateCache) isCooling(accountID int64, model string) bool {
	_, ok := c.cooldownFor(accountID, model)
	return ok
}
