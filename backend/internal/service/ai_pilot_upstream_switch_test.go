package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKeyMatchesSK(t *testing.T) {
	t.Parallel()
	if !keyMatchesSK("sk-abcdefghijklmnop", "sk-abcdefghijklmnop") {
		t.Fatal("exact match")
	}
	if !keyMatchesSK("sk-12345678abcdef", "sk-****abcdef") {
		t.Fatal("masked suffix match")
	}
	if keyMatchesSK("sk-aaa", "sk-bbb") {
		t.Fatal("different keys")
	}
}

func TestPlatformMatchesAccount(t *testing.T) {
	t.Parallel()
	if !platformMatchesAccount(PlatformOpenAI, "openai") {
		t.Fatal("openai")
	}
	if platformMatchesAccount(PlatformOpenAI, PlatformAnthropic) {
		t.Fatal("claude must not match openai account")
	}
	if !platformMatchesAccount(PlatformAnthropic, "claude") {
		t.Fatal("claude alias")
	}
}

func TestParseSub2APIAuthTokens(t *testing.T) {
	t.Parallel()
	body := []byte(`{"code":0,"data":{"access_token":"tok-a","refresh_token":"tok-r","expires_in":3600}}`)
	a, r, exp := parseSub2APIAuthTokens(body)
	if a != "tok-a" || r != "tok-r" || exp == "" {
		t.Fatalf("got a=%q r=%q exp=%q", a, r, exp)
	}
}

func TestParseSub2APIAvailableGroups_FiltersShape(t *testing.T) {
	t.Parallel()
	body := []byte(`{"data":[
		{"id":59,"name":"plus-cheap","rate_multiplier":0.04,"platform":"openai"},
		{"id":41,"name":"claude","rate_multiplier":0.04,"platform":"anthropic"}
	]}`)
	gs := parseSub2APIAvailableGroups(body)
	if len(gs) != 2 {
		t.Fatalf("parse all raw groups, got %d", len(gs))
	}
}

func TestGateSwitchUpstreamGroupReason(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	acc := &Account{ID: 1, Extra: map[string]any{ExtraAIUpstreamGroupSwitch: true}}
	if reason := gateSwitchUpstreamGroupReason(acc, AccountTrafficStats{}, cfg); reason == "" {
		t.Fatal("expected op disabled by default")
	}
	cfg.OpSwitchUpstreamGroup = boolPtr(true)
	if reason := gateSwitchUpstreamGroupReason(acc, AccountTrafficStats{}, cfg); reason != "" {
		t.Fatalf("should pass, got %q", reason)
	}
	bad := AccountTrafficStats{Requests: 20, Successes: 5, Errors: 20}
	if reason := gateSwitchUpstreamGroupReason(acc, bad, cfg); reason == "" {
		t.Fatal("expected hard fail block")
	}
}

func TestSafeSwitchSub2API_TestKeyFirst(t *testing.T) {
	t.Parallel()
	var createHits, deleteHits, putHits, probeHits int
	var prodGroup int64 = 44
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"access_token": "jwt-test", "refresh_token": "ref", "expires_in": 7200},
		})
	})
	mux.HandleFunc("/api/v1/groups/available", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": 44, "name": "plus-mid", "rate_multiplier": 0.06, "platform": "openai"},
			{"id": 59, "name": "plus-cheap", "rate_multiplier": 0.04, "platform": "openai"},
			{"id": 41, "name": "claude", "rate_multiplier": 0.04, "platform": "anthropic"},
		}})
	})
	mux.HandleFunc("/api/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			createHits++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			gid := int64(anyToFloat64(body["group_id"]))
			if gid != 59 {
				t.Errorf("test key must be created on target group 59, got %v", body["group_id"])
			}
			// create returns full key
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"id": 999, "key": "sk-testkey99999999", "group_id": 59, "name": body["name"]},
			})
			return
		}
		// list
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"items": []map[string]any{
				{"id": 1347, "key": "sk-prodkey12345678", "name": "plus", "group_id": prodGroup},
			}},
		})
	})
	mux.HandleFunc("/api/v1/keys/999", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteHits++
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "ok"})
			return
		}
		w.WriteHeader(405)
	})
	mux.HandleFunc("/api/v1/keys/1347", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putHits++
			// production switch only after test key existed
			if createHits == 0 {
				t.Error("must create test key before switching production")
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			prodGroup = int64(anyToFloat64(body["group_id"]))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"id": 1347, "group_id": prodGroup},
			})
			return
		}
		w.WriteHeader(405)
	})
	// probe endpoints for test + prod keys
	mux.HandleFunc("/responses", func(w http.ResponseWriter, r *http.Request) {
		probeHits++
		auth := r.Header.Get("Authorization")
		// first probe should be test key, not prod — allow both pass
		if !strings.Contains(auth, "sk-") {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"resp_1","output":[]}`))
	})
	// also accept /v1/responses style via joinOpenAIURL
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		probeHits++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"resp_1"}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	accRepo := newMemoryAccountRepo(&Account{
		ID: 6154, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Priority:    AIObservationPriority,
		Credentials: map[string]any{"api_key": "sk-prodkey12345678", "base_url": srv.URL},
		Extra: map[string]any{
			ExtraAIUpstreamGroupSwitch: true,
			ExtraUpstreamKind:          "sub2api",
			ExtraUpstreamPanelEmail:    "u@test.com",
			ExtraUpstreamPanelPassword: "secret",
			ExtraAIRateMultiplier:      0.06,
			ExtraUpstreamCurrentGroup:  "44",
		},
	})
	p := &AIPilotService{HTTP: srv.Client(), Accounts: accRepo, Repo: &aiPilotStoreMem{}}
	acc, _ := accRepo.GetByID(t.Context(), 6154)

	// Claude target must be rejected by candidate filter
	_, _, err := p.SwitchUpstreamGroup(t.Context(), acc, "41")
	if err == nil || !strings.Contains(err.Error(), "候选") {
		t.Fatalf("claude group should be rejected, err=%v", err)
	}

	acc, _ = accRepo.GetByID(t.Context(), 6154)
	before, after, err := p.SwitchUpstreamGroup(t.Context(), acc, "59")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if before != "44" || !strings.HasPrefix(after, "59") {
		t.Fatalf("before=%q after=%q", before, after)
	}
	if createHits < 1 {
		t.Fatal("expected test key create")
	}
	if putHits < 1 {
		t.Fatal("expected production put")
	}
	if deleteHits < 1 {
		t.Fatal("expected test key delete")
	}
	if probeHits < 1 {
		t.Fatal("expected probes")
	}
	if prodGroup != 59 {
		t.Fatalf("prod group=%d", prodGroup)
	}
}

func TestInjectUpstreamGroupSwitches_PicksCheaper(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	cfg.OpSwitchUpstreamGroup = boolPtr(true)
	// Pre-seed candidates cache so inject needs no network
	cands := []UpstreamGroupCandidate{
		{ID: "44", Name: "mid", Ratio: 0.06, Platform: PlatformOpenAI, Source: "sub2api", Eligible: true},
		{ID: "59", Name: "cheap", Ratio: 0.04, Platform: PlatformOpenAI, Source: "sub2api", Eligible: true},
	}
	raw, _ := json.Marshal(cands)
	accs := []Account{{
		ID: 1, Platform: PlatformOpenAI, AIManaged: true, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{
			ExtraAIUpstreamGroupSwitch:       true,
			ExtraUpstreamKind:                "sub2api",
			ExtraAIRateMultiplier:            0.06,
			ExtraUpstreamGroupCandidatesJSON: string(raw),
			ExtraUpstreamGroupCandidatesAt:   time.Now().UTC().Format(time.RFC3339),
		},
	}}
	d := decision{}
	p := &AIPilotService{}
	n := injectUpstreamGroupSwitches(p, t.Context(), &d, accs, nil, cfg)
	if n != 1 {
		t.Fatalf("n=%d acts=%+v", n, d.Actions)
	}
	if d.Actions[0].Op != AIOpSwitchUpstreamGroup || d.Actions[0].Value != "59" {
		t.Fatalf("act=%+v", d.Actions[0])
	}
}
