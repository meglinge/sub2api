package service

import (
	"strconv"
	"testing"
	"time"
)

func TestIsSchedulable_AIDisabled(t *testing.T) {
	t.Parallel()
	acc := &Account{Status: StatusActive, Schedulable: true, AIDisabled: true, ScheduleWeight: 10}
	if acc.IsSchedulable() {
		t.Fatal("ai_disabled account must not be schedulable")
	}
	acc.AIDisabled = false
	if !acc.IsSchedulable() {
		t.Fatal("expected schedulable")
	}
}

func TestOpGateReason(t *testing.T) {
	t.Parallel()
	off := false
	cfg := DefaultAIAutopilotSettings()
	cfg.OpDisable = &off
	if reason := opGateReason(cfg, AIOpDisable); reason == "" {
		t.Fatal("expected disabled op to be rejected")
	}
	if reason := opGateReason(cfg, AIOpSetPriority); reason != "" {
		t.Fatalf("priority should be allowed: %s", reason)
	}
}

func TestAmplitudeOK_Weight(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	cfg.WeightMaxDeltaPercent = 50
	cfg.WeightMaxDeltaAbs = 50
	acc := &Account{ScheduleWeight: 10}
	if reason := amplitudeOK(acc, decisionAction{Op: AIOpSetWeight, Value: "100"}, cfg); reason == "" {
		t.Fatal("expected large weight jump rejected")
	}
	if reason := amplitudeOK(acc, decisionAction{Op: AIOpSetWeight, Value: "12"}, cfg); reason != "" {
		t.Fatalf("small jump should pass: %s", reason)
	}
}

func TestOutOfBandPriorityClamp_MonopolyFront(t *testing.T) {
	t.Parallel()
	// User-default priority=1 must be clampable to observation band in one step.
	if !isOutOfBandPriorityClamp(1, 100) {
		t.Fatal("1→100 should be out-of-band clamp")
	}
	if !isOutOfBandPriorityClamp(1, 50) {
		t.Fatal("1→50 should be out-of-band clamp")
	}
	if isOutOfBandPriorityClamp(100, 150) {
		t.Fatal("normal demotion is not out-of-band clamp")
	}
	if isOutOfBandPriorityClamp(1, 1) {
		t.Fatal("target still out of band is not a clamp")
	}
	if !isPrioritySafetyBypass(1, 100) {
		t.Fatal("safety bypass should include monopoly clamp")
	}

	cfg := DefaultAIAutopilotSettings()
	cfg.PriorityMaxDelta = 50 // would reject |100-1|=99 without bypass
	acc := &Account{Priority: 1}
	if reason := amplitudeOK(acc, decisionAction{Op: AIOpSetPriority, Value: "100"}, cfg); reason != "" {
		t.Fatalf("monopoly clamp must bypass delta: %s", reason)
	}
	// Normal in-band jump still gated
	acc.Priority = 100
	if reason := amplitudeOK(acc, decisionAction{Op: AIOpSetPriority, Value: "200"}, cfg); reason == "" {
		t.Fatal("100→200 delta 100 should reject with maxDelta 50")
	}

	// Demotion health gate must not block p=1→100
	long := AccountTrafficStats{Requests: 100, Successes: 100, Errors: 0}
	recent := AccountTrafficStats{Requests: 50, Successes: 50, Errors: 0}
	if reason := priorityDemotionGateReasonEx(acc, 100, long, recent, false); reason == "" {
		// acc still 100 in this branch — reset
	}
	acc.Priority = 1
	if reason := priorityDemotionGateReasonEx(acc, 100, long, recent, false); reason != "" {
		t.Fatalf("1→100 clamp must not hit demotion gate: %s", reason)
	}
}

func TestInjectMonopolyFrontClamp(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	accounts := []Account{
		{ID: 6399, Name: "Sy", Priority: 1, Status: StatusActive, Schedulable: true, ScheduleWeight: 10},
		{ID: 6154, Name: "peer", Priority: 100, Status: StatusActive, Schedulable: true, ScheduleWeight: 100},
	}
	d := decision{}
	n := injectRecoveryEnables(&d, accounts, nil, nil, nil, cfg)
	if n < 1 {
		t.Fatalf("expected monopoly inject, n=%d acts=%+v", n, d.Actions)
	}
	found := false
	for _, a := range d.Actions {
		if a.AccountID == 6399 && a.Op == AIOpSetPriority && a.Value == strconv.Itoa(AIObservationPriority) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected set_priority %d for Sy, acts=%+v", AIObservationPriority, d.Actions)
	}
}

func TestSimulateMinAvailable(t *testing.T) {
	t.Parallel()
	all := []Account{
		{ID: 1, Status: StatusActive, Schedulable: true, GroupIDs: []int64{9}},
		{ID: 2, Status: StatusActive, Schedulable: true, AIDisabled: true, GroupIDs: []int64{9}},
	}
	target := &all[0]
	if reason := simulateMinAvailable(target, all, 1); reason == "" {
		t.Fatal("disabling last available account must be rejected")
	}
}

func TestReadOnlyReason_ManualImmunity(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	cfg.ManualImmunityHours = 2
	now := time.Now()
	touched := now.Add(-30 * time.Minute)
	acc := &Account{
		Platform: PlatformOpenAI, AIManaged: true,
		ManualTouchedAt: &touched, GroupIDs: []int64{1},
	}
	if reason := readOnlyReason(acc, cfg, now, nil); reason == "" {
		t.Fatal("expected manual immunity")
	}
}
