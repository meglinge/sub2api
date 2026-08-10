package service

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

type memoryAccountRepo struct {
	mu   sync.Mutex
	byID map[int64]*Account
}

func newMemoryAccountRepo(accs ...*Account) *memoryAccountRepo {
	m := &memoryAccountRepo{byID: map[int64]*Account{}}
	for _, a := range accs {
		cp := *a
		if a.Extra != nil {
			cp.Extra = map[string]any{}
			for k, v := range a.Extra {
				cp.Extra[k] = v
			}
		}
		m.byID[a.ID] = &cp
	}
	return m
}

func (m *memoryAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.byID[id]
	if a == nil {
		return nil, nil
	}
	cp := *a
	if a.Extra != nil {
		cp.Extra = map[string]any{}
		for k, v := range a.Extra {
			cp.Extra[k] = v
		}
	}
	return &cp, nil
}

func (m *memoryAccountRepo) Update(_ context.Context, account *Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *account
	if account.Extra != nil {
		cp.Extra = map[string]any{}
		for k, v := range account.Extra {
			cp.Extra[k] = v
		}
	}
	m.byID[account.ID] = &cp
	return nil
}

func (m *memoryAccountRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.byID[id]
	if a == nil {
		return nil
	}
	if a.Extra == nil {
		a.Extra = map[string]any{}
	}
	for k, v := range updates {
		a.Extra[k] = v
	}
	return nil
}

func (m *memoryAccountRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Account{}
	for _, a := range m.byID {
		if a.Platform == platform {
			cp := *a
			out = append(out, cp)
		}
	}
	return out, nil
}

type aiPilotStoreMem struct {
	mu      sync.Mutex
	actions []AIAction
	nextID  int64
}

func (s *aiPilotStoreMem) CreateRun(_ context.Context, run AIRun) (AIRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	run.ID = s.nextID
	return run, nil
}
func (s *aiPilotStoreMem) CreateAction(_ context.Context, a AIAction) (AIAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	a.ID = s.nextID
	s.actions = append(s.actions, a)
	return a, nil
}
func (s *aiPilotStoreMem) ListRuns(context.Context, int, int64) ([]AIRun, error) { return nil, nil }
func (s *aiPilotStoreMem) ListRunsAsc(context.Context, int) ([]AIRun, error)     { return nil, nil }
func (s *aiPilotStoreMem) GetRun(context.Context, int64) (*AIRun, error)         { return nil, nil }
func (s *aiPilotStoreMem) ListActionsByRun(context.Context, int64) ([]AIAction, error) {
	return nil, nil
}
func (s *aiPilotStoreMem) ListActionsInRecentRuns(context.Context, int) ([]AIAction, error) {
	return nil, nil
}
func (s *aiPilotStoreMem) GetAction(context.Context, int64) (*AIAction, error) { return nil, nil }
func (s *aiPilotStoreMem) UpdateActionState(context.Context, int64, string, string, *time.Time) error {
	return nil
}
func (s *aiPilotStoreMem) ListSuggestions(context.Context, int) ([]AIAction, error) { return nil, nil }
func (s *aiPilotStoreMem) DismissSuggestions(context.Context, bool, *time.Time, string) (int64, error) {
	return 0, nil
}
func (s *aiPilotStoreMem) LastAppliedAt(context.Context, int64) (*time.Time, error) {
	return nil, nil
}
func (s *aiPilotStoreMem) LastSpareDemotions(context.Context, []int64, time.Time) (map[int64]time.Time, error) {
	return nil, nil
}
func (s *aiPilotStoreMem) AggregateAccountTraffic(context.Context, time.Time, time.Time, []int64) (map[int64]AccountTrafficStats, error) {
	return nil, nil
}
func (s *aiPilotStoreMem) RecentErrorSamples(context.Context, time.Time, int64, int) ([]string, error) {
	return nil, nil
}
func (s *aiPilotStoreMem) AppendAccountScores(context.Context, int64, []AIAccountScore) error {
	return nil
}
func (s *aiPilotStoreMem) LatestAccountScores(context.Context) (map[int64]AIAccountScore, error) {
	return nil, nil
}
func (s *aiPilotStoreMem) AccountScoreHistory(context.Context, int64, int) ([]AIAccountScore, error) {
	return nil, nil
}
func (s *aiPilotStoreMem) PruneAccountScores(context.Context, int) (int64, error) { return 0, nil }

func TestAmplitudeOK_AllowsRecoveryUnbury9000to100(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	cfg.PriorityMaxDelta = 100 // production-like cap that would reject delta 8800
	acc := &Account{Priority: 9000, AIManaged: true, Platform: PlatformOpenAI}
	// Observation tier is 100 (not 200) in current band.
	if reason := amplitudeOK(acc, decisionAction{Op: AIOpSetPriority, Value: strconv.Itoa(AIObservationPriority)}, cfg); reason != "" {
		t.Fatalf("recovery unbury must bypass amplitude, got %q", reason)
	}
	// Soft spare 150→100 also bypasses
	accSpare := &Account{Priority: 150}
	if reason := amplitudeOK(accSpare, decisionAction{Op: AIOpSetPriority, Value: "100"}, cfg); reason != "" {
		t.Fatalf("soft spare unbury must bypass, got %q", reason)
	}
	// Normal large jump still blocked
	acc2 := &Account{Priority: 10}
	if reason := amplitudeOK(acc2, decisionAction{Op: AIOpSetPriority, Value: "500"}, cfg); reason == "" {
		t.Fatal("non-recovery large jump should reject")
	}
}

func TestApplyDecisionActions_UnburyPriorityActuallyApplies(t *testing.T) {
	t.Parallel()
	// Already-enabled, priority buried at 9000 — inject set_priority 200 must APPLY
	// through amplitudeOK + ApplyAIOp with default PriorityMaxDelta=100.
	acc := &Account{
		ID: 77, Name: "buried", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, AIManaged: true, AIDisabled: false,
		Priority: 9000, ScheduleWeight: 10, Concurrency: 10,
		GroupIDs: []int64{2},
	}
	repo := newMemoryAccountRepo(acc)
	store := &aiPilotStoreMem{}
	p := &AIPilotService{
		Accounts: repo,
		Repo:     store,
	}
	cfg := DefaultAIAutopilotSettings()
	cfg.ApplyMode = "auto"
	cfg.PriorityMaxDelta = 100 // the failing production default
	cfg.ConfidenceThreshold = 0.5
	cfg.MaxActionsPerRun = 10
	cfg.ChannelCooldownMinutes = 0
	off := false
	cfg.ActivationProbeEnabled = &off

	d := decision{}
	n := injectRecoveryEnables(&d, []Account{*acc}, nil, nil, nil, cfg, nil, time.Time{})
	if n < 1 {
		t.Fatalf("inject should emit set_priority, n=%d d=%+v", n, d.Actions)
	}
	var priAct *decisionAction
	for i := range d.Actions {
		if d.Actions[i].Op == AIOpSetPriority {
			priAct = &d.Actions[i]
			break
		}
	}
	wantPri := strconv.Itoa(AIObservationPriority)
	if priAct == nil || priAct.Value != wantPri {
		t.Fatalf("expected set_priority %s, got %+v", wantPri, d.Actions)
	}
	if reason := amplitudeOK(acc, *priAct, cfg); reason != "" {
		t.Fatalf("amplitude rejected unbury: %s", reason)
	}

	p.applyDecisionActions(context.Background(), 1, time.Now(), cfg, d, []Account{*acc}, nil, nil, nil, nil)

	store.mu.Lock()
	defer store.mu.Unlock()
	var applied bool
	for _, a := range store.actions {
		if a.Op == AIOpSetPriority && a.State == AIActionRejected {
			t.Fatalf("set_priority rejected: %s (actions=%+v)", a.RejectReason, store.actions)
		}
		if a.Op == AIOpSetPriority && a.State == AIActionApplied {
			applied = true
			if a.After != wantPri {
				t.Fatalf("after=%q want %s; actions=%+v", a.After, wantPri, store.actions)
			}
		}
	}
	if !applied {
		t.Fatalf("expected applied set_priority, actions=%+v", store.actions)
	}
	got, _ := repo.GetByID(context.Background(), 77)
	if got.Priority != AIObservationPriority {
		t.Fatalf("account priority=%d want %d", got.Priority, AIObservationPriority)
	}
}

func TestApplyAIOp_EnableUnburiesPriority(t *testing.T) {
	t.Parallel()
	acc := &Account{
		ID: 88, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true,
		AIManaged: true, AIDisabled: true, Priority: 9000, ScheduleWeight: 1,
	}
	repo := newMemoryAccountRepo(acc)
	p := &AIPilotService{Accounts: repo}
	_, after, err := p.ApplyAIOp(context.Background(), 88, AIOpEnable, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetByID(context.Background(), 88)
	if got.AIDisabled {
		t.Fatal("still disabled")
	}
	if got.Priority != AIObservationPriority {
		t.Fatalf("priority=%d after=%s", got.Priority, after)
	}
}

func TestResolveAccountMoney_PersistsDepletedForGate(t *testing.T) {
	t.Parallel()
	// When balance is already in Extra (as after a refresh), gate must see it
	// on a GetByID reload path — simulate persist then load.
	acc := &Account{
		ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, AIManaged: true,
		Extra: map[string]any{},
	}
	repo := newMemoryAccountRepo(acc)
	_ = repo.UpdateExtra(context.Background(), 9, map[string]any{
		ExtraAIBalanceStatus:    "depleted",
		ExtraAIBalanceUSD:       0.0,
		ExtraAIBalanceCheckedAt: time.Now().UTC().Format(time.RFC3339),
	})
	loaded, _ := repo.GetByID(context.Background(), 9)
	if reason := balanceGateReason(AIOpEnable, loaded); reason == "" {
		t.Fatal("persisted depleted must block enable on reloaded account")
	}
}
