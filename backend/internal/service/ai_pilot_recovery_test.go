package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompactPilotMemoryActions_PrefersDisableEnable(t *testing.T) {
	t.Parallel()
	list := []AIAction{
		{ID: 1, Op: AIOpSetWeight, AccountID: 1},
		{ID: 2, Op: AIOpDisable, AccountID: 2},
		{ID: 3, Op: AIOpSetWeight, AccountID: 3},
		{ID: 4, Op: AIOpEnable, AccountID: 4},
		{ID: 5, Op: AIOpSetPriority, AccountID: 5},
	}
	got := compactPilotMemoryActions(list, 3)
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	ops := []string{got[0].Op, got[1].Op, got[2].Op}
	joined := strings.Join(ops, ",")
	if !strings.Contains(joined, AIOpDisable) || !strings.Contains(joined, AIOpEnable) {
		t.Fatalf("should keep disable/enable, got %v", ops)
	}
}

func TestCollectRecoveryProbeRequests_PrioritizesAIDisabled(t *testing.T) {
	t.Parallel()
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, AIDisabled: false, ScheduleWeight: 10}, // idle
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, AIDisabled: true, ScheduleWeight: 10},  // disabled
		{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, AIDisabled: false, ScheduleWeight: 0},  // soft stop
		{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, AIDisabled: true, ScheduleWeight: 5},   // disabled
		{ID: 5, Platform: PlatformOpenAI, Type: "oauth", AIDisabled: true, ScheduleWeight: 10},            // oauth skipped
	}
	traffic := map[int64]AccountTrafficStats{
		1: {AccountID: 1}, // idle
		2: {AccountID: 2, Requests: 0},
		3: {AccountID: 3, Requests: 5},
		4: {AccountID: 4, Requests: 1},
	}
	reqs := collectRecoveryProbeRequests(accounts, traffic, 10)
	if len(reqs) < 3 {
		t.Fatalf("expected at least disabled+soft+idle, got %+v", reqs)
	}
	// First two must be the AI-disabled accounts (2 then 4).
	if reqs[0].AccountID != 2 || reqs[1].AccountID != 4 {
		t.Fatalf("disabled should come first: %+v", reqs)
	}
	// Cap works.
	capped := collectRecoveryProbeRequests(accounts, traffic, 1)
	if len(capped) != 1 || capped[0].AccountID != 2 {
		t.Fatalf("cap=1 should take first disabled: %+v", capped)
	}
}

func TestInjectRecoveryEnables_SlowProbeDoesNotReviveDeadAccount(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	accounts := []Account{
		{ID: 6520, Name: "CoCo", AIDisabled: true, Priority: 150, ScheduleWeight: 16,
			Status: StatusActive, Schedulable: true},
	}
	probes := map[int64]activationResult{
		6520: {AccountID: 6520, Verdict: "slow", Fresh: true, TTFBMs: 8000, Source: "upstream"},
	}
	long := map[int64]AccountTrafficStats{
		6520: {Requests: 8, Successes: 8, Errors: 33},
	}
	recent := map[int64]AccountTrafficStats{
		6520: {Requests: 1, Successes: 1, Errors: 22},
	}
	d := decision{}
	n := injectRecoveryEnables(&d, accounts, probes, long, recent, cfg, nil, time.Time{})
	for _, a := range d.Actions {
		if a.AccountID == 6520 && a.Op == AIOpEnable {
			t.Fatalf("dead CoCo must not be re-enabled on slow ping, n=%d acts=%+v", n, d.Actions)
		}
		if a.AccountID == 6520 && a.Op == AIOpSetPriority {
			t.Fatalf("dead CoCo must not be lifted to main, n=%d acts=%+v", n, d.Actions)
		}
	}
}

func TestInjectRecoveryEnables_FillsMissingEnable(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	cfg.ApplyMode = "auto"
	accounts := []Account{
		{ID: 10, Name: "bad-still", AIDisabled: true},
		{ID: 11, Name: "recovered", AIDisabled: true},
		{ID: 12, Name: "already-proposed", AIDisabled: true},
		{ID: 13, Name: "healthy", AIDisabled: false},
	}
	probes := map[int64]activationResult{
		10: {AccountID: 10, Verdict: "fail", Fresh: true, Error: "HTTP 403", Source: "upstream"},
		11: {AccountID: 11, Verdict: "pass", Fresh: true, TTFBMs: 200, Source: "upstream"},
		12: {AccountID: 12, Verdict: "pass", Fresh: true, Source: "upstream"},
	}
	d := decision{
		Actions: []decisionAction{
			{AccountID: 12, Op: AIOpEnable, Reason: "model already", Confidence: 0.9},
		},
	}
	n := injectRecoveryEnables(&d, accounts, probes, nil, nil, cfg, nil, time.Time{})
	if n < 1 {
		t.Fatalf("expected >=1 inject, got %d actions=%+v", n, d.Actions)
	}
	// Should not inject for fail or already-proposed or non-disabled.
	var found11 bool
	for _, a := range d.Actions {
		if a.AccountID == 10 && a.Op == AIOpEnable {
			t.Fatal("must not enable failed probe")
		}
		if a.AccountID == 11 && a.Op == AIOpEnable {
			found11 = true
			if a.Confidence < 0.8 {
				t.Fatalf("upstream pass conf too low: %v", a.Confidence)
			}
			if !strings.Contains(a.Reason, "自动恢复") {
				t.Fatalf("reason=%q", a.Reason)
			}
		}
	}
	if !found11 {
		t.Fatalf("missing enable for 11: %+v", d.Actions)
	}
	// Op enable off → no inject.
	off := false
	cfg.OpEnable = &off
	d2 := decision{}
	if n := injectRecoveryEnables(&d2, accounts, probes, nil, nil, cfg, nil, time.Time{}); n != 0 {
		t.Fatalf("op_enable off should inject 0, got %d", n)
	}
}

func TestInjectRecoveryEnables_ControlPlaneLowerConfidence(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	accounts := []Account{{ID: 7, AIDisabled: true}}
	probes := map[int64]activationResult{
		7: {AccountID: 7, Verdict: "pass", Fresh: true, Source: "control_plane"},
	}
	d := decision{}
	n := injectRecoveryEnables(&d, accounts, probes, nil, nil, cfg, nil, time.Time{})
	if n != 1 {
		t.Fatalf("n=%d", n)
	}
	if d.Actions[0].Confidence > 0.7 {
		t.Fatalf("control_plane conf should be capped at 0.7, got %v", d.Actions[0].Confidence)
	}
}

func TestActivationView_DisabledPass(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	acc := &Account{ID: 5, AIDisabled: true, ScheduleWeight: 10}
	res := activationResult{AccountID: 5, Verdict: "pass", Fresh: true, TTFBMs: 100, Source: "upstream"}
	v := activationView(acc, res, true, cfg)
	if v["verdict"] != "pass" {
		t.Fatalf("verdict=%v", v["verdict"])
	}
	note, _ := v["note"].(string)
	if !strings.Contains(note, "enable") {
		t.Fatalf("note should urge enable: %q", note)
	}
	if needed, _ := v["needed"].(bool); !needed {
		t.Fatal("needed should be true for aiDisabled")
	}
}

func TestProbeViaUpstream_PassAndFail(t *testing.T) {
	t.Parallel()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test-key" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":"bad key"}`)
			return
		}
		// Default path is /v1/responses for pool accounts.
		if !strings.Contains(r.URL.Path, "/responses") {
			w.WriteHeader(404)
			return
		}
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_x","output":[]}`)
	}))
	defer srv.Close()

	p := &AIPilotService{HTTP: srv.Client()}
	cfg := DefaultAIAutopilotSettings()
	cfg.ActivationProbeMaxTtfbMs = 0

	acc := &Account{
		ID: 99, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test-key",
			"base_url": srv.URL,
		},
		Extra: map[string]any{"probe_model": "gpt-5.6-sol", "openai_responses_supported": true},
	}
	res, ok := p.probeViaUpstream(context.Background(), acc, 5*time.Second, "test", cfg)
	if !ok {
		t.Fatal("expected upstream path")
	}
	if res.Verdict != "pass" {
		t.Fatalf("verdict=%s err=%s", res.Verdict, res.Error)
	}
	if res.Source != "upstream" {
		t.Fatalf("source=%s", res.Source)
	}
	if hits < 1 {
		t.Fatalf("hits=%d", hits)
	}
	if !strings.Contains(res.Reason, "via=responses") || !strings.Contains(res.Reason, "gpt-5.6-sol") {
		t.Fatalf("expected responses+probe_model, reason=%q", res.Reason)
	}

	// Fail path: wrong key
	acc.Credentials["api_key"] = "sk-wrong"
	res, ok = p.probeViaUpstream(context.Background(), acc, 5*time.Second, "test", cfg)
	if !ok {
		t.Fatal("expected upstream path even on fail")
	}
	if res.Verdict != "fail" {
		t.Fatalf("expected fail, got %s", res.Verdict)
	}
}

func TestResolveActivationProbeModels_SettingsFirst(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	cfg.ActivationProbeModels = "gpt-5.6-sol"
	acc := &Account{
		Extra: map[string]any{"probe_model": "gpt-5.6-terra"},
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5.6-terra": "gpt-5.6-terra"},
		},
	}
	got := resolveActivationProbeModels(cfg, acc)
	if len(got) != 1 || got[0] != "gpt-5.6-sol" {
		t.Fatalf("operator list must be exclusive (no mapping/gpt-5 fallbacks), got=%v", got)
	}
	empty := DefaultAIAutopilotSettings()
	empty.ActivationProbeModels = ""
	got = resolveActivationProbeModels(empty.Normalize(), &Account{})
	if len(got) != 1 || got[0] != "gpt-5.6-sol" {
		t.Fatalf("empty setting must default to gpt-5.6-sol only, got=%v", got)
	}
}

func TestParseActivationProbeModels(t *testing.T) {
	t.Parallel()
	got := parseActivationProbeModels("gpt-5.6-sol, gpt-5.4，gpt-5")
	if len(got) != 3 || got[0] != "gpt-5.6-sol" || got[1] != "gpt-5.4" || got[2] != "gpt-5" {
		t.Fatalf("got=%v", got)
	}
}

func TestProbeViaUpstream_RetriesModelNotFound(t *testing.T) {
	t.Parallel()
	var tried []string
	var paths []string
	var sawMessages bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/models") {
			t.Errorf("must not list /models during activation probe")
			w.WriteHeader(404)
			return
		}
		if strings.Contains(r.URL.Path, "/chat/completions") {
			t.Errorf("must not fall back to chat when responses supported")
			w.WriteHeader(500)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"messages"`) {
			sawMessages = true
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		tried = append(tried, body.Model)
		if body.Model != "gpt-5.6-sol" {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"message":"Model not supported","type":"model_not_found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer srv.Close()

	p := &AIPilotService{HTTP: srv.Client()}
	cfg := DefaultAIAutopilotSettings()
	cfg.ActivationProbeMaxTtfbMs = 0
	cfg.ActivationProbeModels = "nope-model, gpt-5.6-sol"
	acc := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-x", "base_url": srv.URL},
		Extra:       map[string]any{"probe_model": "nope-model", "default_model": "gpt-5.6-sol", "openai_responses_supported": true},
	}
	res, ok := p.probeViaUpstream(context.Background(), acc, 5*time.Second, "r", cfg)
	if !ok || res.Verdict != "pass" {
		t.Fatalf("ok=%v res=%+v tried=%v paths=%v", ok, res, tried, paths)
	}
	if len(tried) < 2 {
		t.Fatalf("expected model retry, tried=%v", tried)
	}
	if sawMessages {
		t.Fatal("probe body must not use messages for responses-supported accounts")
	}
	if !strings.Contains(res.Reason, "via=responses") {
		t.Fatalf("expected responses path, reason=%q", res.Reason)
	}
}

func TestProbeViaUpstream_PayloadOmitsPingAndMaxOutputTokens(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = io.WriteString(w, `{"id":"resp_ok"}`)
	}))
	defer srv.Close()
	p := &AIPilotService{HTTP: srv.Client()}
	cfg := DefaultAIAutopilotSettings()
	cfg.ActivationProbeMaxTtfbMs = 0
	acc := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-x", "base_url": srv.URL},
		Extra:       map[string]any{"openai_responses_supported": true},
	}
	res, ok := p.probeViaUpstream(context.Background(), acc, 5*time.Second, "r", cfg)
	if !ok || res.Verdict != "pass" {
		t.Fatalf("ok=%v res=%+v", ok, res)
	}
	if got["input"] != "ok" {
		t.Fatalf("input=%v want ok", got["input"])
	}
	if got["stream"] != true {
		t.Fatalf("stream=%v", got["stream"])
	}
	if _, exists := got["max_output_tokens"]; exists {
		t.Fatalf("max_output_tokens must be omitted: %+v", got)
	}
}

func TestProbeViaUpstream_TimeoutOnConfiguredModelIsSlowNotGPT5(t *testing.T) {
	t.Parallel()
	var tried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(raw, &body)
		tried = append(tried, body.Model)
		if !body.Stream {
			t.Errorf("responses probe must stream, model=%s", body.Model)
		}
		if body.Model == "gpt-5.6-sol" {
			time.Sleep(3 * time.Second)
			return
		}
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":{"code":"model_not_found","message":"Model \"gpt-5\" is not available for this group"}}`)
	}))
	defer srv.Close()
	p := &AIPilotService{HTTP: srv.Client()}
	cfg := DefaultAIAutopilotSettings()
	cfg.ActivationProbeMaxTtfbMs = 0
	cfg.ActivationProbeModels = "gpt-5.6-sol"
	acc := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-x", "base_url": srv.URL},
		Extra:       map[string]any{"openai_responses_supported": true},
	}
	res, ok := p.probeViaUpstream(context.Background(), acc, time.Second, "r", cfg)
	if !ok {
		t.Fatal("expected upstream path")
	}
	if res.Verdict != "slow" {
		t.Fatalf("timeout on configured model must be slow, got %s err=%s tried=%v", res.Verdict, res.Error, tried)
	}
	if strings.Contains(res.Error, "gpt-5:") || containsString(tried, "gpt-5") {
		t.Fatalf("must not fall through to gpt-5, tried=%v err=%s", tried, res.Error)
	}
	if !strings.Contains(res.Error, "gpt-5.6-sol") {
		t.Fatalf("error should name configured model, err=%s", res.Error)
	}
}

func containsString(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}

func TestProbeViaUpstream_503IsSlowNotFail(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":{"message":"Service temporarily unavailable","type":"api_error"}}`)
	}))
	defer srv.Close()
	p := &AIPilotService{HTTP: srv.Client()}
	cfg := DefaultAIAutopilotSettings()
	cfg.ActivationProbeMaxTtfbMs = 0
	acc := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-x", "base_url": srv.URL},
		Extra:       map[string]any{"probe_model": "gpt-5.6-sol", "openai_responses_supported": true},
	}
	res, ok := p.probeViaUpstream(context.Background(), acc, 5*time.Second, "r", cfg)
	if !ok {
		t.Fatal("expected upstream path")
	}
	if res.Verdict != "slow" {
		t.Fatalf("503 must be slow not %s err=%s", res.Verdict, res.Error)
	}
	if reason := activationGateReason(AIOpEnable, "", 10, map[int64]activationResult{1: res}, 1, cfg); reason != "" {
		t.Fatalf("slow 503 must not block enable: %s", reason)
	}
}

func TestProbeViaUpstream_NoChatFallbackWhenResponsesSupported(t *testing.T) {
	t.Parallel()
	var hitResponses, hitChat int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/responses") {
			hitResponses++
			// Fail all models on responses
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"message":"Model not supported","type":"model_not_found"}}`)
			return
		}
		if strings.Contains(r.URL.Path, "/chat/completions") {
			hitChat++
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	p := &AIPilotService{HTTP: srv.Client()}
	cfg := DefaultAIAutopilotSettings()
	cfg.ActivationProbeMaxTtfbMs = 0
	acc := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-x", "base_url": srv.URL},
		Extra:       map[string]any{"probe_model": "gpt-5.6-sol", "openai_responses_supported": true},
	}
	res, ok := p.probeViaUpstream(context.Background(), acc, 5*time.Second, "r", cfg)
	if !ok || res.Verdict != "fail" {
		t.Fatalf("expected fail without chat fallback, ok=%v res=%+v", ok, res)
	}
	if hitChat != 0 {
		t.Fatalf("chat fallback forbidden when responses supported, hitChat=%d hitResponses=%d", hitChat, hitResponses)
	}
	if hitResponses == 0 {
		t.Fatal("expected responses attempts")
	}
}

func TestRunActivationProbes_PrefersUpstream(t *testing.T) {
	t.Parallel()
	var hitChat, hitLLM int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			hitChat++
			w.WriteHeader(500)
			return
		}
		if strings.Contains(r.URL.Path, "/responses") {
			_, _ = io.WriteString(w, `{"id":"resp_ok","output":[]}`)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	// Separate "control plane" base would be cfg.BaseURL; leave empty so any LLM
	// attempt would fail — we must never call it for activation.
	p := &AIPilotService{HTTP: srv.Client()}
	cfg := DefaultAIAutopilotSettings()
	cfg.ActivationProbeMaxTtfbMs = 0
	cfg.BaseURL = srv.URL
	cfg.APIKey = "sk-cee-should-not-be-used"
	acc := &Account{
		ID: 42, Name: "relay", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-x", "base_url": srv.URL},
		Extra:       map[string]any{"probe_model": "gpt-5.6-sol", "openai_responses_supported": true},
	}
	byID := map[int64]*Account{42: acc}
	names := map[int64]string{42: "relay"}
	out := p.runActivationProbes(context.Background(), cfg,
		[]probeRequest{{AccountID: 42, Reason: "recovery"}}, byID, names)
	if len(out) != 1 {
		t.Fatalf("out=%+v", out)
	}
	if out[0].Source != "upstream" || out[0].Verdict != "pass" {
		t.Fatalf("got %+v", out[0])
	}
	if hitChat != 0 || hitLLM != 0 {
		t.Fatalf("must not hit chat/LLM, hitChat=%d", hitChat)
	}
}

func TestRunActivationProbes_NoControlPlaneFallback(t *testing.T) {
	t.Parallel()
	var chatHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			chatHits++
		}
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `fail`)
	}))
	defer srv.Close()
	p := &AIPilotService{HTTP: srv.Client()}
	cfg := DefaultAIAutopilotSettings()
	cfg.BaseURL = srv.URL
	cfg.APIKey = "sk-cee"
	// oauth — previously fell back to control-plane LLM
	acc := &Account{ID: 9, Platform: PlatformOpenAI, Type: "oauth"}
	out := p.runActivationProbes(context.Background(), cfg,
		[]probeRequest{{AccountID: 9, Reason: "x"}}, map[int64]*Account{9: acc}, map[int64]string{9: "o"})
	if len(out) != 1 || out[0].Verdict != "unknown" {
		t.Fatalf("expected skipped unknown, got %+v", out)
	}
	if chatHits != 0 {
		t.Fatalf("oauth must not hit CCH chat, hits=%d", chatHits)
	}
}

func TestSoftUnburyEligible(t *testing.T) {
	t.Parallel()
	longOK := AccountTrafficStats{Requests: 100, Successes: 95, Errors: 5}
	longTiny := AccountTrafficStats{Requests: 3, Successes: 3}
	recentBad := AccountTrafficStats{Requests: 20, Successes: 10, Errors: 12}
	if !softUnburyEligible(longOK, AccountTrafficStats{}) {
		t.Fatal("healthy long + empty recent should allow")
	}
	if softUnburyEligible(longOK, recentBad) {
		t.Fatal("recent hard fail must block soft unbury")
	}
	if softUnburyEligible(longTiny, AccountTrafficStats{}) {
		t.Fatal("tiny long sample must not soft-unbury")
	}
}

func TestSoftUnburyDwell_BlocksThrash(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	// Just demoted 1m ago → dwell active (dwell window is AISoftUnburyDwell, currently 5m)
	if reason := softUnburyDwellGateReason(150, 100, now.Add(-1*time.Minute), now); reason == "" {
		t.Fatal("expected dwell reject for recent spare demotion")
	}
	// Outside dwell → allow
	if reason := softUnburyDwellGateReason(150, 100, now.Add(-AISoftUnburyDwell-time.Minute), now); reason != "" {
		t.Fatalf("dwell expired should allow, got %q", reason)
	}
	// Deep exile unbury not gated
	if reason := softUnburyDwellGateReason(9000, 100, now.Add(-time.Minute), now); reason != "" {
		t.Fatalf("deep exile must not use soft dwell, got %q", reason)
	}
	// Demotion (not unbury) not gated
	if reason := softUnburyDwellGateReason(100, 150, now.Add(-time.Minute), now); reason != "" {
		t.Fatalf("demotion must not hit unbury dwell, got %q", reason)
	}
	// No demotion history
	if reason := softUnburyDwellGateReason(150, 100, time.Time{}, now); reason != "" {
		t.Fatalf("zero demotion time should allow, got %q", reason)
	}
}

func TestInjectRecovery_SoftUnburyDwellBlocks(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	// Healthy long window at p150 — classic soft-unbury would fire without dwell.
	accounts := []Account{
		{ID: 6395, Name: "Niko", AIDisabled: false, Priority: 150, ScheduleWeight: 100,
			Status: StatusActive, Schedulable: true},
	}
	long := map[int64]AccountTrafficStats{
		6395: {Requests: 200, Successes: 190, Errors: 10},
	}
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	// Without dwell map: inject unbury
	d1 := decision{}
	n1 := injectRecoveryEnables(&d1, accounts, nil, long, nil, cfg, nil, now)
	if n1 != 1 {
		t.Fatalf("expected soft unbury without dwell, n=%d acts=%+v", n1, d1.Actions)
	}
	// With recent demotion: blocked
	d2 := decision{}
	dem := map[int64]time.Time{6395: now.Add(-3 * time.Minute)}
	n2 := injectRecoveryEnables(&d2, accounts, nil, long, nil, cfg, dem, now)
	if n2 != 0 {
		t.Fatalf("dwell must block soft unbury, n=%d acts=%+v", n2, d2.Actions)
	}
	// After dwell: unbury again
	d3 := decision{}
	demOld := map[int64]time.Time{6395: now.Add(-AISoftUnburyDwell - time.Minute)}
	n3 := injectRecoveryEnables(&d3, accounts, nil, long, nil, cfg, demOld, now)
	if n3 != 1 {
		t.Fatalf("expired dwell should allow soft unbury, n=%d acts=%+v", n3, d3.Actions)
	}
}
