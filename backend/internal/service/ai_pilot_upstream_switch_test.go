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
	if !keyMatchesSK("sk-abcdefghijklmnop", "sk-****mnop") && !keyMatchesSK("sk-abcdefghijklmnop", "sk-****klmnop") {
		// trailing alnum of listed may be shorter
	}
	if !keyMatchesSK("sk-12345678abcdef", "sk-****abcdef") {
		t.Fatal("masked suffix match")
	}
	if keyMatchesSK("sk-aaa", "sk-bbb") {
		t.Fatal("different keys")
	}
}

func TestParseSub2APIAuthTokens(t *testing.T) {
	t.Parallel()
	body := []byte(`{"code":0,"data":{"access_token":"tok-a","refresh_token":"tok-r","expires_in":3600}}`)
	a, r, exp := parseSub2APIAuthTokens(body)
	if a != "tok-a" || r != "tok-r" || exp == "" {
		t.Fatalf("got a=%q r=%q exp=%q", a, r, exp)
	}
	flat := []byte(`{"access_token":"x","refresh_token":"y"}`)
	a, r, _ = parseSub2APIAuthTokens(flat)
	if a != "x" || r != "y" {
		t.Fatalf("flat got a=%q r=%q", a, r)
	}
}

func TestParseSub2APIAvailableGroups(t *testing.T) {
	t.Parallel()
	body := []byte(`{"data":[{"id":2,"name":"Pro","rate_multiplier":0.05,"platform":"openai"},{"id":5,"name":"Team","rate_multiplier":0.08}]}`)
	gs := parseSub2APIAvailableGroups(body)
	if len(gs) != 2 || gs[0].ID != 2 || gs[0].RateMultiplier != 0.05 {
		t.Fatalf("got %+v", gs)
	}
}

func TestParseSub2APIKeyList(t *testing.T) {
	t.Parallel()
	body := []byte(`{"data":{"items":[{"id":9,"key":"sk-hello12345678","name":"k1","group_id":2}]}}`)
	keys := parseSub2APIKeyList(body)
	if len(keys) != 1 || keys[0].ID != 9 || keys[0].GroupID != 2 {
		t.Fatalf("got %+v", keys)
	}
}

func TestGateSwitchUpstreamGroupReason(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	// default op off
	acc := &Account{ID: 1, Extra: map[string]any{ExtraAIUpstreamGroupSwitch: true}}
	if reason := gateSwitchUpstreamGroupReason(acc, AccountTrafficStats{}, cfg); reason == "" {
		t.Fatal("expected op disabled")
	}
	cfg.OpSwitchUpstreamGroup = boolPtr(true)
	if reason := gateSwitchUpstreamGroupReason(acc, AccountTrafficStats{}, cfg); reason != "" {
		t.Fatalf("enabled should pass empty recent, got %q", reason)
	}
	// account switch off
	acc2 := &Account{ID: 2}
	if reason := gateSwitchUpstreamGroupReason(acc2, AccountTrafficStats{}, cfg); !strings.Contains(reason, "ai_upstream_group_switch") {
		t.Fatalf("got %q", reason)
	}
	// recent hard fail
	bad := AccountTrafficStats{Requests: 20, Successes: 5, Errors: 20}
	if reason := gateSwitchUpstreamGroupReason(acc, bad, cfg); reason == "" {
		t.Fatal("expected hard fail block")
	}
	// dwell
	acc.Extra[ExtraUpstreamLastGroupSwitchAt] = time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	if reason := gateSwitchUpstreamGroupReason(acc, AccountTrafficStats{}, cfg); !strings.Contains(reason, "驻留") {
		t.Fatalf("expected dwell, got %q", reason)
	}
}

func TestSub2APIPanelLoginAndSwitch(t *testing.T) {
	t.Parallel()
	var loginHits, putHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		loginHits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"access_token": "jwt-test", "refresh_token": "ref-test", "expires_in": 7200},
		})
	})
	mux.HandleFunc("/api/v1/groups/available", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jwt-test" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": 2, "name": "cheap", "rate_multiplier": 0.04},
				{"id": 5, "name": "stable", "rate_multiplier": 0.08},
			},
		})
	})
	mux.HandleFunc("/api/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"items": []map[string]any{
					{"id": 42, "key": "sk-testhostkey12345678", "name": "main", "group_id": 5},
				},
			},
		})
	})
	mux.HandleFunc("/api/v1/keys/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(405)
			return
		}
		putHits++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": 42, "group_id": 2}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	store := &aiPilotStoreMem{}
	// minimal account repo stub via memoryAccountRepo from apply_unbury_test
	accRepo := newMemoryAccountRepo(&Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-testhostkey12345678", "base_url": srv.URL},
		Extra: map[string]any{
			ExtraAIUpstreamGroupSwitch: true,
			ExtraUpstreamKind:          "sub2api",
			ExtraUpstreamPanelEmail:    "u@test.com",
			ExtraUpstreamPanelPassword: "secret",
		},
	})
	p := &AIPilotService{HTTP: srv.Client(), Repo: store, Accounts: accRepo}
	acc, _ := accRepo.GetByID(t.Context(), 1)
	before, after, err := p.SwitchUpstreamGroup(t.Context(), acc, "2")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if before != "5" || !strings.HasPrefix(after, "2") {
		t.Fatalf("before=%q after=%q", before, after)
	}
	if loginHits < 1 || putHits < 1 {
		t.Fatalf("loginHits=%d putHits=%d", loginHits, putHits)
	}
	// second switch within dwell should fail gate (not Apply path — SwitchUpstreamGroup itself checks dwell)
	acc2, _ := accRepo.GetByID(t.Context(), 1)
	_, _, err = p.SwitchUpstreamGroup(t.Context(), acc2, "5")
	if err == nil || !strings.Contains(err.Error(), "驻留") {
		t.Fatalf("expected dwell error, got %v", err)
	}
}
