//go:build unit

package service

import (
	"context"
	"testing"
	"time"
)

type decayAccountStore struct {
	acc *Account
}

func (s *decayAccountStore) GetByID(context.Context, int64) (*Account, error) { return s.acc, nil }
func (s *decayAccountStore) Update(_ context.Context, a *Account) error {
	s.acc = a
	return nil
}
func (s *decayAccountStore) UpdateExtra(context.Context, int64, map[string]any) error { return nil }
func (s *decayAccountStore) ListByPlatform(context.Context, string) ([]Account, error) {
	return nil, nil
}

func TestDecayNeverAutoEnablesDisable(t *testing.T) {
	acc := &Account{
		ID: 1, Status: StatusActive, Schedulable: true,
		AIDisabled: true, Priority: 100, ScheduleWeight: 10,
	}
	store := &decayAccountStore{acc: acc}
	repo := &aiPilotStoreMem{}
	p := &AIPilotService{Accounts: store, Repo: repo}

	// Seed a pending disable demotion with starved outcome (would have auto-enabled before).
	now := time.Now()
	a := AIAction{
		ID: 1, AccountID: 1, Op: AIOpDisable,
		Before: "false", After: "true",
		State: AIActionApplied, Outcome: verdictPrefix + verdictStarved + " | test",
		TS: now.Add(-10 * time.Minute),
	}
	repo.pendingDecay = []AIAction{a}

	// Force DemotionTTL so decay runs.
	n := p.decayStaleDemotions(context.Background(), AIAutopilotSettings{DemotionTTLMinutes: 25})
	if n != 0 {
		t.Fatalf("expected 0 reverts for disable, got %d", n)
	}
	if !store.acc.AIDisabled {
		t.Fatal("disable must not be auto-enabled by decay")
	}
}

func TestDecayDoesNotRevertWeightZeroSoftQuarantine(t *testing.T) {
	acc := &Account{
		ID: 2, Status: StatusActive, Schedulable: true,
		AIDisabled: false, Priority: 150, ScheduleWeight: 0,
	}
	store := &decayAccountStore{acc: acc}
	repo := &aiPilotStoreMem{}
	p := &AIPilotService{Accounts: store, Repo: repo}

	a := AIAction{
		ID: 2, AccountID: 2, Op: AIOpSetWeight,
		Before: "10", After: "0",
		State: AIActionApplied, Outcome: verdictPrefix + verdictStarved + " | soft q",
		TS: time.Now().Add(-10 * time.Minute),
	}
	repo.pendingDecay = []AIAction{a}

	n := p.decayStaleDemotions(context.Background(), AIAutopilotSettings{DemotionTTLMinutes: 25})
	if n != 0 {
		t.Fatalf("expected 0 reverts for weight=0, got %d", n)
	}
	if store.acc.ScheduleWeight != 0 {
		t.Fatalf("weight=0 soft quarantine must stick, got %d", store.acc.ScheduleWeight)
	}
}

func TestPostSlotAdmissionVetoAIDisabledAndWeightZero(t *testing.T) {
	ctx := context.Background()
	if got := postSlotAdmissionVetoReason(ctx, &Account{
		Status: StatusActive, Schedulable: true, AIDisabled: true, ScheduleWeight: 10,
	}); got != "ai_disabled" {
		t.Fatalf("want ai_disabled, got %q", got)
	}
	if got := postSlotAdmissionVetoReason(ctx, &Account{
		Status: StatusActive, Schedulable: true, AIManaged: true, ScheduleWeight: 0,
	}); got != "schedule_weight_zero" {
		t.Fatalf("want schedule_weight_zero, got %q", got)
	}
	// Unmanaged weight=0 is not soft-quarantine (test fixtures / non-pilot).
	if got := postSlotAdmissionVetoReason(ctx, &Account{
		Status: StatusActive, Schedulable: true, ScheduleWeight: 0,
	}); got != "" {
		t.Fatalf("want empty for unmanaged weight0, got %q", got)
	}
	if got := postSlotAdmissionVetoReason(ctx, &Account{
		Status: StatusActive, Schedulable: true, AIManaged: true, ScheduleWeight: 10,
	}); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}
