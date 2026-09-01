package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchNewAPIRate_TokenVipRatio(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/user/self/groups", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("New-Api-User") != "42" {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"data":{"vip":{"ratio":0.5,"desc":"VIP"},"default":{"ratio":1}}}`)
	})
	mux.HandleFunc("/api/token/", func(w http.ResponseWriter, r *http.Request) {
		// Accept only raw Authorization (no Bearer) — some new-api builds do this.
		auth := r.Header.Get("Authorization")
		if auth == "Bearer mgmt-tok" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"success":false,"message":"invalid"}`)
			return
		}
		if auth != "mgmt-tok" {
			w.WriteHeader(401)
			return
		}
		// Only p=0 returns data (p=1 empty) — pagination variance.
		if r.URL.Query().Get("p") == "1" {
			_, _ = io.WriteString(w, `{"success":true,"data":{"items":[]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"data":{"items":[{"id":1,"key":"ZNC0**********bmx4","group":"vip","status":1,"remain_quota":1000},{"id":2,"key":"sk-vip-key-001","group":"vip","status":1,"remain_quota":1000}]}}`)
	})
	mux.HandleFunc("/api/user/self", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "Bearer mgmt-tok" {
			w.WriteHeader(401)
			return
		}
		if auth != "mgmt-tok" {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"data":{"quota":1000000,"used_quota":0}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &AIPilotService{HTTP: srv.Client()}
	// Masked token without sk- prefix must still match full sk- key.
	rate, src, remain, ok := p.fetchNewAPIRate(context.Background(), srv.URL, "mgmt-tok", "42", "sk-ZNC0U3Qr6Z5WgBv3eszAtlCv2hw0kcTkYRkEAXFUVMtUbmx4")
	if !ok || rate != 0.5 || src != "newapi" {
		t.Fatalf("rate=%v src=%s ok=%v", rate, src, ok)
	}
	if remain != 1000 {
		t.Fatalf("remain=%d want 1000", remain)
	}

	// missing mgmt → resolveAccountRate falls to default
	acc := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{}}
	r, source := resolveAccountRate(acc)
	if r != 1 || source != "default_one" {
		t.Fatalf("no mgmt: rate=%v source=%s", r, source)
	}

	// ResolveAccountMoney with mgmt fills rate + balance (Bearer-fail → raw auth).
	acc2 := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-vip-key-001", "base_url": srv.URL},
		Extra: map[string]any{
			ExtraUpstreamKind:       "newapi",
			ExtraUpstreamMgmtToken:  "mgmt-tok",
			ExtraUpstreamMgmtUserID: "42",
		},
	}
	rate2, src2, balSt, balUSD := p.ResolveAccountMoney(context.Background(), acc2)
	if rate2 != 0.5 || !strings.Contains(src2, "newapi") {
		t.Fatalf("money rate=%v src=%s", rate2, src2)
	}
	if balSt != "ok" || balUSD != 2 {
		t.Fatalf("bal st=%s usd=%v", balSt, balUSD)
	}
}

func TestMoneyView_MissingMgmtLowConfidence(t *testing.T) {
	t.Parallel()
	acc := &Account{Extra: map[string]any{ExtraRechargeMultiplier: 1.0}}
	m := moneyView(acc)
	rc := m["rateConfidence"].(map[string]any)
	if rc["level"].(int) != 3 {
		t.Fatalf("expected low confidence, got %+v", rc)
	}
	if m["rateMultiplier"].(float64) != 1 {
		t.Fatalf("rate=%v", m["rateMultiplier"])
	}
}

func TestFetchNewAPIRate_NoFirstGroupFallback(t *testing.T) {
	t.Parallel()
	// multi-group + no matching token → must NOT invent first group's ratio (was Niko 0.3 bug)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/user/self/groups", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"data":{"gemini":{"ratio":0.3},"codex-混池":{"ratio":0.06}}}`)
	})
	mux.HandleFunc("/api/token/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"data":{"items":[{"id":1,"key":"sk-other-zzzz","group":"gemini","status":1,"remain_quota":1}]}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := &AIPilotService{HTTP: srv.Client()}
	_, _, _, ok := p.fetchNewAPIRate(context.Background(), srv.URL, "mgmt", "1", "sk-real-codex-key")
	if ok {
		t.Fatal("multi-group without token match must fail, not fall back to groups[0]")
	}

	// single group still ok without match
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/api/user/self/groups", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"data":{"only":{"ratio":0.12}}}`)
	})
	mux2.HandleFunc("/api/token/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"data":{"items":[]}}`)
	})
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()
	p2 := &AIPilotService{HTTP: srv2.Client()}
	rate, src, _, ok := p2.fetchNewAPIRate(context.Background(), srv2.URL, "mgmt", "1", "sk-x")
	if !ok || rate != 0.12 || src != "newapi" {
		t.Fatalf("single group: rate=%v src=%s ok=%v", rate, src, ok)
	}
}

func TestTokenKeyMatch_MaskedWithoutSkPrefix(t *testing.T) {
	t.Parallel()
	// Niko-class: listed drops "sk-", full key keeps it
	full := "sk-8M3m87GRQxxxxERo3IdjNwlh3"
	listed := "8M3m**********wlh3"
	if !tokenKeyMatch(full, listed) {
		t.Fatalf("should match masked key without sk- prefix")
	}
	if tokenKeyMatch("sk-other**********zzzz", listed) {
		t.Fatal("should not match different key")
	}
	if !tokenKeyMatch(full, full) {
		t.Fatal("exact match")
	}
	// also works when both have sk-
	if !tokenKeyMatch(full, "sk-8M3m**********wlh3") {
		t.Fatal("masked with sk- should match")
	}
}

func TestResolveAccountMoney_APIKeyUsageTokenTrailingSlash(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	// bare path 301s; trailing slash works (new-api)
	mux.HandleFunc("/api/usage/token", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/usage/token/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/api/usage/token/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":true,"data":{"object":"token_usage","total_available":-1,"unlimited_quota":true},"message":"ok"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := &AIPilotService{HTTP: srv.Client()}
	acc := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-x", "base_url": srv.URL},
		Extra:       map[string]any{ExtraUpstreamKind: "newapi"},
	}
	_, _, balSt, _ := p.ForceResolveAccountMoney(context.Background(), acc)
	if balSt != "unlimited" {
		t.Fatalf("expected unlimited from trailing-slash usage, got %s", balSt)
	}
}

func TestResolveAccountMoney_APIKeyUsageBalance(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	// Original UpstreamRouter path for sub2api pool vendors
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer sk-") {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"balance":24.74,"daily_usage":[{"date":"2026-08-05","cost":1}]}`)
	})
	mux.HandleFunc("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","resolved_rate_multiplier":0.07,"group_rate_multiplier":0.07,"effective_rate_multiplier":0.07}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	acc := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test-usage-001", "base_url": srv.URL},
		Extra:       map[string]any{},
	}
	p := &AIPilotService{HTTP: srv.Client()}
	rate, src, balSt, balUSD := p.ResolveAccountMoney(context.Background(), acc)
	if rate != 0.07 || src != "sub2api" {
		t.Fatalf("rate=%v src=%s want 0.07/sub2api", rate, src)
	}
	if balSt != "ok" || balUSD < 24 || balUSD > 25 {
		t.Fatalf("bal st=%s usd=%v want ~24.74 from /v1/usage", balSt, balUSD)
	}
	if extraFloat(acc.Extra, ExtraAIRateMultiplier) != 0.07 {
		t.Fatalf("extra rate not persisted in memory: %+v", acc.Extra)
	}
	if extraString(acc.Extra, ExtraAIBalanceStatus) != "ok" {
		t.Fatalf("extra bal not set: %+v", acc.Extra)
	}
}

func TestShouldRefreshSub2APIBilling_ByKindNotSource(t *testing.T) {
	t.Parallel()
	if !shouldRefreshSub2APIBilling("sub2api", false) {
		t.Fatal("sub2api kind must refresh")
	}
	if !shouldRefreshSub2APIBilling("", false) {
		t.Fatal("empty kind must refresh")
	}
	if !shouldRefreshSub2APIBilling("manual", false) {
		t.Fatal("manual kind must refresh")
	}
	if shouldRefreshSub2APIBilling("newapi", false) {
		t.Fatal("newapi kind must not use sub2api billing")
	}
	if shouldRefreshSub2APIBilling("sub2api", true) {
		t.Fatal("already-live must not double probe")
	}
}

func TestResolveAccountMoney_RefreshesStaleSub2APIRate(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"sub2api.key_billing","resolved_rate_multiplier":0.1,"group_rate_multiplier":0.1,"effective_rate_multiplier":0.1}`)
	})
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"balance":10}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	acc := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-hajimi-pro", "base_url": srv.URL},
		Extra: map[string]any{
			ExtraAIRateMultiplier: 0.06,
			ExtraAIRateSource:     "sub2api",
			ExtraAIRateCheckedAt:  "2026-08-10T00:50:43Z",
		},
	}
	p := &AIPilotService{HTTP: srv.Client()}
	rate, src, _, _ := p.ResolveAccountMoney(context.Background(), acc)
	if rate != 0.1 || src != "sub2api" {
		t.Fatalf("stale sub2api cache must refresh, rate=%v src=%s", rate, src)
	}
	if extraFloat(acc.Extra, ExtraAIRateMultiplier) != 0.1 {
		t.Fatalf("extra not updated: %+v", acc.Extra)
	}

	// Fresh cache of a different family must not be overwritten by sub2api probe.
	acc2 := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-x", "base_url": srv.URL},
		Extra: map[string]any{
			ExtraAIRateMultiplier: 0.045,
			ExtraAIRateSource:     "newapi",
			ExtraAIRateCheckedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}
	rate2, src2, _, _ := p.ResolveAccountMoney(context.Background(), acc2)
	if rate2 != 0.045 || src2 != "newapi" {
		t.Fatalf("fresh newapi must stick, rate=%v src=%s", rate2, src2)
	}
}

func TestResolveAccountMoney_RefreshesManualRollbackAndGroupSwitch(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"sub2api.key_billing","resolved_rate_multiplier":0.1,"group_rate_multiplier":0.1,"effective_rate_multiplier":0.1}`)
	})
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"balance":10}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := &AIPilotService{HTTP: srv.Client()}

	for _, src := range []string{"manual_rollback", "sub2api_group_switch", "imported"} {
		acc := &Account{
			Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "sk-madou-pro", "base_url": srv.URL},
			Extra: map[string]any{
				ExtraUpstreamKind:     "sub2api",
				ExtraAIRateMultiplier: 0.06,
				ExtraAIRateSource:     src,
				ExtraAIRateCheckedAt:  "2026-08-10T03:13:01Z",
			},
		}
		rate, gotSrc, _, _ := p.ResolveAccountMoney(context.Background(), acc)
		if rate != 0.1 || gotSrc != "sub2api" {
			t.Fatalf("source=%s must refresh, rate=%v src=%s", src, rate, gotSrc)
		}
	}

	// newapi-kind must not be overwritten by the sub2api billing endpoint.
	acc := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-x", "base_url": srv.URL},
		Extra: map[string]any{
			ExtraUpstreamKind:     "newapi",
			ExtraAIRateMultiplier: 0.045,
			ExtraAIRateSource:     "newapi",
			ExtraAIRateCheckedAt:  "2026-08-10T03:13:01Z",
		},
	}
	rate, src, _, _ := p.ResolveAccountMoney(context.Background(), acc)
	if rate != 0.045 || src != "newapi" {
		t.Fatalf("newapi kind must keep stale newapi when mgmt missing, rate=%v src=%s", rate, src)
	}
}

func TestResolveAccountMoney_AdoptsNewerProbeOverStaleSub2APICache(t *testing.T) {
	t.Parallel()
	// 2chat (1193): extra.ai_rate=0.03 written yesterday, official probe 0.05 this morning.
	acc := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-2chat", "base_url": "http://127.0.0.1:1"},
		Extra: map[string]any{
			ExtraAIRateMultiplier: 0.03,
			ExtraAIRateSource:     "sub2api",
			ExtraAIRateCheckedAt:  "2026-08-30T14:25:56Z",
			UpstreamBillingProbeExtraKey: map[string]any{
				"status":      "ok",
				"received_at": "2026-08-31T06:09:50.999822363Z",
				"data": map[string]any{
					"resolved_rate_multiplier":  0.05,
					"effective_rate_multiplier": 0.05,
					"group_rate_multiplier":     0.05,
				},
			},
		},
	}
	p := &AIPilotService{HTTP: &http.Client{Timeout: time.Second}}
	rate, src, _, _ := p.ResolveAccountMoney(context.Background(), acc)
	if rate != 0.05 || src != "billing_probe" {
		t.Fatalf("newer probe must win over stale 0.03 extra, rate=%v src=%s", rate, src)
	}
	if extraFloat(acc.Extra, ExtraAIRateMultiplier) != 0.05 {
		t.Fatalf("extra must be corrected to probe: %+v", acc.Extra)
	}
}

func TestResolveAccountRate_PrefersNewerBillingProbe(t *testing.T) {
	t.Parallel()
	acc := &Account{
		Extra: map[string]any{
			ExtraAIRateMultiplier: 0.06,
			ExtraAIRateSource:     "manual_rollback",
			ExtraAIRateCheckedAt:  "2026-08-10T03:13:01Z",
			UpstreamBillingProbeExtraKey: map[string]any{
				"status":      "ok",
				"received_at": "2026-08-24T00:49:25.67123348Z",
				"data": map[string]any{
					"resolved_rate_multiplier":  0.065,
					"effective_rate_multiplier": 0.065,
					"group_rate_multiplier":     0.065,
				},
			},
		},
	}
	rate, src := resolveAccountRate(acc)
	if rate != 0.065 || src != "billing_probe" {
		t.Fatalf("newer probe should win, rate=%v src=%s", rate, src)
	}

	// Adopt into extra when resolving money even if HTTP 403s.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	acc.Platform = PlatformOpenAI
	acc.Type = AccountTypeAPIKey
	acc.Credentials = map[string]any{"api_key": "sk-madou", "base_url": srv.URL}
	acc.Extra[ExtraUpstreamKind] = "sub2api"
	p := &AIPilotService{HTTP: srv.Client()}
	rate2, src2, _, _ := p.ResolveAccountMoney(context.Background(), acc)
	if rate2 != 0.065 || src2 != "billing_probe" {
		t.Fatalf("adopt probe after HTTP fail, rate=%v src=%s", rate2, src2)
	}
	if extraFloat(acc.Extra, ExtraAIRateMultiplier) != 0.065 {
		t.Fatalf("extra not adopted: %+v", acc.Extra)
	}
}

func TestParseSub2APIUsageBalance_PoolShape(t *testing.T) {
	t.Parallel()
	st, usd, ok := ParseSub2APIUsageBalance([]byte(`{"balance":333.71,"daily_usage":[]}`))
	if !ok || st != "ok" || usd < 333 || usd > 334 {
		t.Fatalf("st=%s usd=%v ok=%v", st, usd, ok)
	}
	st, usd, ok = ParseSub2APIUsageBalance([]byte(`{"mode":"quota_limited","isValid":true,"remaining":12.5}`))
	if !ok || st != "ok" || usd != 12.5 {
		t.Fatalf("remaining st=%s usd=%v ok=%v", st, usd, ok)
	}
	st, _, ok = ParseSub2APIUsageBalance([]byte(`{"mode":"unrestricted","isValid":true}`))
	if !ok || st != "unlimited" {
		t.Fatalf("unlimited st=%s ok=%v", st, ok)
	}
	_, _, ok = ParseSub2APIUsageBalance([]byte(`{"hello":"world"}`))
	if ok {
		t.Fatal("should reject unknown shape")
	}
}

func TestParseSub2APIUsageRate_CodekeyShape(t *testing.T) {
	t.Parallel()
	// Live codekey /v1/usage: list cost vs wallet actual_cost → 0.08.
	body := []byte(`{
		"balance":198.60988694,
		"daily_usage":[{"date":"2026-09-01","cost":17.3787773,"actual_cost":1.390113064}],
		"isValid":true,
		"mode":"unrestricted",
		"model_stats":[
			{"model":"gpt-5.6-sol","cost":16.4165955,"actual_cost":1.31313852},
			{"model":"gpt-5.4","cost":0.000465,"actual_cost":0.0000372}
		],
		"planName":"钱包余额",
		"remaining":198.60988694,
		"unit":"USD",
		"usage":{"today":{"actual_cost":1.390113064,"cost":17.3787773}}
	}`)
	r, ok := ParseSub2APIUsageRate(body)
	if !ok || r != 0.08 {
		t.Fatalf("rate=%v ok=%v want 0.08", r, ok)
	}

	// daily_usage only (no usage.today)
	r, ok = ParseSub2APIUsageRate([]byte(`{"balance":10,"daily_usage":[{"date":"2026-09-01","cost":1,"actual_cost":0.05}]}`))
	if !ok || r != 0.05 {
		t.Fatalf("daily_usage rate=%v ok=%v want 0.05", r, ok)
	}

	// cost without actual_cost cannot invent a rate
	if _, ok = ParseSub2APIUsageRate([]byte(`{"balance":24.74,"daily_usage":[{"date":"2026-08-05","cost":1}]}`)); ok {
		t.Fatal("must not infer rate without actual_cost")
	}
	if _, ok = ParseSub2APIUsageRate([]byte(`{"hello":"world"}`)); ok {
		t.Fatal("must reject unknown shape")
	}
}

func TestResolveAccountMoney_UsageRateFallbackWhenBillingMissing(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	})
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer sk-") {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"balance":198.61,"isValid":true,"mode":"unrestricted","usage":{"today":{"cost":17.3787773,"actual_cost":1.390113064}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	acc := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-codekey-test", "base_url": srv.URL},
		Extra:       map[string]any{},
	}
	p := &AIPilotService{HTTP: srv.Client()}
	rate, src, balSt, balUSD := p.ResolveAccountMoney(context.Background(), acc)
	if rate != 0.08 || src != "sub2api_usage" {
		t.Fatalf("rate=%v src=%s want 0.08/sub2api_usage", rate, src)
	}
	if balSt != "ok" || balUSD < 198 || balUSD > 199 {
		t.Fatalf("bal st=%s usd=%v want ~198.61", balSt, balUSD)
	}
	if extraFloat(acc.Extra, ExtraAIRateMultiplier) != 0.08 {
		t.Fatalf("extra rate not persisted: %+v", acc.Extra)
	}

	// Declared /v1/sub2api/billing still wins over usage inference.
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","resolved_rate_multiplier":0.05,"group_rate_multiplier":0.05,"effective_rate_multiplier":0.05}`)
	})
	mux2.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"balance":10,"usage":{"today":{"cost":10,"actual_cost":0.8}}}`)
	})
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()
	acc2 := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-has-billing", "base_url": srv2.URL},
		Extra:       map[string]any{},
	}
	p2 := &AIPilotService{HTTP: srv2.Client()}
	rate2, src2, _, _ := p2.ResolveAccountMoney(context.Background(), acc2)
	if rate2 != 0.05 || src2 != "sub2api" {
		t.Fatalf("billing must win, rate=%v src=%s", rate2, src2)
	}
}

func TestFetchOneAPIDashboardBalance(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/dashboard/billing/subscription", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"billing_subscription","hard_limit_usd":100}`)
	})
	mux.HandleFunc("/v1/dashboard/billing/usage", func(w http.ResponseWriter, r *http.Request) {
		// 2550 cents = $25.50
		_, _ = io.WriteString(w, `{"object":"list","total_usage":2550}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := &AIPilotService{HTTP: srv.Client()}
	st, usd, ok := p.fetchOneAPIDashboardBalance(context.Background(), srv.URL, "sk-x")
	if !ok || st != "ok" || usd != 74.5 {
		t.Fatalf("st=%s usd=%v ok=%v want 74.5", st, usd, ok)
	}
}
