package service

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	extraKeyCodexTurnStates          = "codex_turn_states"
	extraKeyCodexTurnStateCapturedAt = "codex_turn_state_captured_at"
	maxCodexTurnStateLen             = 8192
	maxCodexTurnStateModels          = 64
)

// GetCodexTurnState 返回该账号为指定模型缓存的上游 X-Codex-Turn-State。
func (a *Account) GetCodexTurnState(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if a == nil || model == "" {
		return ""
	}
	return strings.TrimSpace(extraStringMap(a.Extra, extraKeyCodexTurnStates)[model])
}

// CodexTurnStateFresh 判断缓存 blob 是否仍在 TTL 内。空值不新鲜；缺时间戳视为新鲜。
func (a *Account) CodexTurnStateFresh(model string, ttl time.Duration, now time.Time) (string, bool) {
	value := a.GetCodexTurnState(model)
	if value == "" {
		return "", false
	}
	if ttl <= 0 || now.IsZero() {
		return value, true
	}
	captured := extraTimeMap(a.Extra, extraKeyCodexTurnStateCapturedAt)[strings.ToLower(strings.TrimSpace(model))]
	if captured.IsZero() || now.Sub(captured) < ttl {
		return value, true
	}
	return value, false
}

// SetCodexTurnState 写入单个模型的 blob 与捕获时间（仅内存 Extra）。
func (a *Account) SetCodexTurnState(model, value string, capturedAt time.Time) {
	model = strings.ToLower(strings.TrimSpace(model))
	value = strings.TrimSpace(value)
	if a == nil || model == "" || value == "" {
		return
	}
	if capturedAt.IsZero() {
		capturedAt = time.Now()
	}
	if a.Extra == nil {
		a.Extra = make(map[string]any, 2)
	}
	states := extraStringMap(a.Extra, extraKeyCodexTurnStates)
	if states == nil {
		states = map[string]string{}
	}
	if len(value) > maxCodexTurnStateLen {
		return
	}
	states[model] = value
	if len(states) > maxCodexTurnStateModels {
		delete(states, model)
		return
	}
	captured := extraInt64Map(a.Extra, extraKeyCodexTurnStateCapturedAt)
	if captured == nil {
		captured = map[string]int64{}
	}
	captured[model] = capturedAt.Unix()
	a.Extra[extraKeyCodexTurnStates] = states
	a.Extra[extraKeyCodexTurnStateCapturedAt] = captured
}

// ClearCodexTurnState 丢掉该模型的缓存。
func (a *Account) ClearCodexTurnState(model string) {
	model = strings.ToLower(strings.TrimSpace(model))
	if a == nil || model == "" || a.Extra == nil {
		return
	}
	states := extraStringMap(a.Extra, extraKeyCodexTurnStates)
	captured := extraInt64Map(a.Extra, extraKeyCodexTurnStateCapturedAt)
	if len(states) == 0 && len(captured) == 0 {
		return
	}
	delete(states, model)
	delete(captured, model)
	a.Extra[extraKeyCodexTurnStates] = states
	a.Extra[extraKeyCodexTurnStateCapturedAt] = captured
}

func extraStringMap(extra map[string]any, key string) map[string]string {
	if extra == nil {
		return nil
	}
	raw, ok := extra[key]
	if !ok || raw == nil {
		return nil
	}
	switch typed := raw.(type) {
	case map[string]string:
		out := make(map[string]string, len(typed))
		for k, v := range typed {
			if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
				continue
			}
			out[k] = v
		}
		return out
	case map[string]any:
		out := make(map[string]string, len(typed))
		for k, v := range typed {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			s, _ := v.(string)
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			out[k] = s
		}
		return out
	default:
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil
		}
		var decoded map[string]string
		if json.Unmarshal(encoded, &decoded) != nil {
			return nil
		}
		return decoded
	}
}

func extraInt64Map(extra map[string]any, key string) map[string]int64 {
	if extra == nil {
		return nil
	}
	raw, ok := extra[key]
	if !ok || raw == nil {
		return nil
	}
	switch typed := raw.(type) {
	case map[string]int64:
		out := make(map[string]int64, len(typed))
		for k, v := range typed {
			if strings.TrimSpace(k) == "" || v <= 0 {
				continue
			}
			out[k] = v
		}
		return out
	case map[string]any:
		out := make(map[string]int64, len(typed))
		for k, v := range typed {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			switch n := v.(type) {
			case int64:
				if n > 0 {
					out[k] = n
				}
			case float64:
				if n > 0 {
					out[k] = int64(n)
				}
			case json.Number:
				if i, err := n.Int64(); err == nil && i > 0 {
					out[k] = i
				}
			case string:
				if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil && i > 0 {
					out[k] = i
				}
			}
		}
		return out
	default:
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil
		}
		var decoded map[string]int64
		if json.Unmarshal(encoded, &decoded) != nil {
			return nil
		}
		return decoded
	}
}

func extraTimeMap(extra map[string]any, key string) map[string]time.Time {
	raw := extraInt64Map(extra, key)
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(raw))
	for k, ts := range raw {
		if ts <= 0 {
			continue
		}
		out[k] = time.Unix(ts, 0)
	}
	return out
}

func (a *Account) codexPlanType() string {
	if a == nil {
		return ""
	}
	return a.GetCredential("plan_type")
}
