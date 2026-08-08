package service

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type decisionAction struct {
	AccountID  int64   `json:"accountId"`
	Op         string  `json:"op"`
	Value      string  `json:"value"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
}

func opGateReason(cfg AIAutopilotSettings, op string) string {
	if !cfg.OpAllowed(op) {
		return "动作权限已关闭: " + op
	}
	return ""
}

func readOnlyReason(acc *Account, cfg AIAutopilotSettings, now time.Time, managed map[int64]bool) string {
	if acc == nil {
		return "账号不存在"
	}
	if !acc.AIManaged {
		return "ai_managed=false"
	}
	if len(cfg.ManagedGroupIDs) > 0 {
		// All account groups must be managed; if any group outside whitelist, read-only.
		ok := false
		for _, gid := range acc.GroupIDs {
			if managed[gid] {
				ok = true
				break
			}
		}
		// If account has no groups, only allow when whitelist empty (already false here).
		if !ok && len(acc.GroupIDs) > 0 {
			// require every group in whitelist
			for _, gid := range acc.GroupIDs {
				if !managed[gid] {
					return "分组不在自动驾驶白名单"
				}
			}
		}
		if len(acc.GroupIDs) == 0 {
			return "账号未绑定分组"
		}
	}
	if acc.Platform != PlatformOpenAI && acc.Platform != "openai" {
		// Phase 1: OpenAI only
		return "非 OpenAI 账号"
	}
	if acc.ManualTouchedAt != nil {
		until := acc.ManualTouchedAt.Add(time.Duration(cfg.ManualImmunityHours) * time.Hour)
		if now.Before(until) {
			return "人工改动免疫期内"
		}
	}
	return ""
}

func amplitudeOK(acc *Account, act decisionAction, cfg AIAutopilotSettings) string {
	switch act.Op {
	case AIOpSetWeight:
		n, err := strconv.Atoi(strings.TrimSpace(act.Value))
		if err != nil || n < 0 {
			return "weight 无效"
		}
		cur := acc.EffectiveScheduleWeight()
		if cur <= 0 {
			cur = 10
		}
		delta := math.Abs(float64(n - cur))
		if cfg.WeightMaxDeltaAbs > 0 && int(delta) > cfg.WeightMaxDeltaAbs {
			return fmt.Sprintf("weight 单次变化超过绝对值上限 %d", cfg.WeightMaxDeltaAbs)
		}
		if cfg.WeightMaxDeltaPercent > 0 {
			maxDelta := float64(cur) * float64(cfg.WeightMaxDeltaPercent) / 100.0
			if maxDelta < 1 {
				maxDelta = 1
			}
			if delta > maxDelta {
				return fmt.Sprintf("weight 单次变化超过相对上限 %d%%", cfg.WeightMaxDeltaPercent)
			}
		}
	case AIOpSetPriority:
		n, err := strconv.Atoi(strings.TrimSpace(act.Value))
		if err != nil {
			return "priority 无效"
		}
		// Safety clamps (deep exile unbury OR monopoly front p=1→band) must bypass
		// PriorityMaxDelta — otherwise enable-only recovery / p=1 fix loops forever.
		if isPrioritySafetyBypass(acc.Priority, n) {
			return ""
		}
		// Safety ceiling: misconfigured PriorityMaxDelta (e.g. 1e9) caused minute-by-minute thrash.
		maxDelta := cfg.PriorityMaxDelta
		if maxDelta <= 0 || maxDelta > AIPriorityDeltaSafety {
			maxDelta = AIPriorityDeltaSafety
		}
		if absInt(n-acc.Priority) > maxDelta {
			return fmt.Sprintf("priority 单次变化超过上限 %d", maxDelta)
		}
	}
	return ""
}

// isRecoveryUnburyPriority is true when lifting a buried/spare priority toward
// the observation band (injectRecoveryEnables / soft spare unbury). Bypasses
// PriorityMaxDelta / safety ceiling so 9000→100 is not rejected.
func isRecoveryUnburyPriority(current, target int) bool {
	if !(ShouldUnburyPriority(current) || ShouldSoftUnburySpareTier(current)) {
		return false
	}
	// Must be a lift into the main/observation band, not a further demotion.
	return target < current && target >= AIMinPriority && target <= AIObservationPriority
}

// isOutOfBandPriorityClamp is true when current priority is *in front of* the
// autopilot band (e.g. user-default priority=1) and target brings it into
// [AIMinPriority, AIMaxPriority]. Strict layering means sole p=1 monopolizes
// the pool; AI must be able to demote into the band in one step without being
// blocked by PriorityMaxDelta (1→100) or demotion cooldown.
func isOutOfBandPriorityClamp(current, target int) bool {
	if current >= AIMinPriority {
		return false
	}
	// Target must land in the allowed autopilot band (ApplyAIOp also clamps).
	return target >= AIMinPriority && target <= AIMaxPriority
}

// isPrioritySafetyBypass covers both deep-exile unbury and monopoly-front clamp.
func isPrioritySafetyBypass(current, target int) bool {
	return isRecoveryUnburyPriority(current, target) || isOutOfBandPriorityClamp(current, target)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// simulateMinAvailable returns reject reason if disable would drop any group below min.
func simulateMinAvailable(acc *Account, all []Account, min int) string {
	if min <= 0 || acc == nil {
		return ""
	}
	for _, gid := range acc.GroupIDs {
		avail := 0
		for i := range all {
			a := &all[i]
			inGroup := false
			for _, g := range a.GroupIDs {
				if g == gid {
					inGroup = true
					break
				}
			}
			if !inGroup {
				continue
			}
			// Count as available if active+schedulable+!ai_disabled (ignore transient RL).
			if a.ID == acc.ID {
				continue // after disable this one is gone
			}
			if a.Status == StatusActive && a.Schedulable && !a.AIDisabled {
				avail++
			}
		}
		if avail < min {
			return fmt.Sprintf("分组 %d 可用账号将低于保底 %d", gid, min)
		}
	}
	return ""
}
