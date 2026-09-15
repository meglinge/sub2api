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

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

func TestParseSub2APIMonthRecharge(t *testing.T) {
	t.Parallel()
	amount, period, ok := ParseSub2APIMonthRecharge([]byte(`{
		"object":"sub2api.key_billing",
		"month_recharged_usd": 12.5,
		"month_recharged_period": "2026-09"
	}`))
	if !ok || amount != 12.5 || period != "2026-09" {
		t.Fatalf("got ok=%v amount=%v period=%q", ok, amount, period)
	}

	amount, period, ok = ParseSub2APIMonthRecharge([]byte(`{"code":0,"data":{"period":"2026-09","amount":8}}`))
	if !ok || amount != 8 || period != "2026-09" {
		t.Fatalf("wrapped stats got ok=%v amount=%v period=%q", ok, amount, period)
	}

	if _, _, ok := ParseSub2APIMonthRecharge([]byte(`{"object":"sub2api.key_billing","resolved_rate_multiplier":0.1}`)); ok {
		t.Fatal("missing month fields must not parse")
	}
	if _, _, ok := ParseSub2APIMonthRecharge([]byte(`{"month_recharged_usd":-1,"month_recharged_period":"2026-09"}`)); ok {
		t.Fatal("negative amount must not parse")
	}
}

func TestParseSub2APIRedeemHistoryMonth(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	body, _ := json.Marshal(map[string]any{
		"code": 0,
		"data": []map[string]any{
			{"type": "balance", "value": 10, "used_at": "2026-09-02T00:00:00Z"},
			{"type": "admin_balance", "value": 5, "used_at": "2026-09-10T00:00:00Z"},
			{"type": "balance", "value": 20, "used_at": "2026-08-31T00:00:00Z"},
			{"type": "concurrency", "value": 3, "used_at": "2026-09-03T00:00:00Z"},
			{"type": "admin_balance", "value": -2, "used_at": "2026-09-04T00:00:00Z"},
		},
	})
	amount, partial, ok := parseSub2APIRedeemHistoryMonth(body, start, end)
	if !ok || amount != 15 || partial {
		t.Fatalf("got ok=%v amount=%v partial=%v", ok, amount, partial)
	}
}

func TestResolveAccountMoney_Sub2APIMonthRechargeFromBilling(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"object":"sub2api.key_billing",
			"schema_version":1,
			"billing_scope":"token",
			"resolved_rate_multiplier":0.07,
			"group_rate_multiplier":0.07,
			"effective_rate_multiplier":0.07,
			"month_recharged_usd":42.5,
			"month_recharged_period":"2026-09"
		}`)
	})
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"balance":10}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	acc := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-month-1", "base_url": srv.URL},
		Extra:       map[string]any{ExtraUpstreamKind: "sub2api"},
	}
	p := &AIPilotService{HTTP: srv.Client()}
	_, _, _, _ = p.ResolveAccountMoney(context.Background(), acc)
	if extraFloat(acc.Extra, ExtraAIMonthRechargedUSD) != 42.5 {
		t.Fatalf("month usd=%v extra=%+v", extraFloat(acc.Extra, ExtraAIMonthRechargedUSD), acc.Extra)
	}
	if extraString(acc.Extra, ExtraAIMonthRechargedPeriod) != "2026-09" {
		t.Fatalf("period=%q", extraString(acc.Extra, ExtraAIMonthRechargedPeriod))
	}
	if extraString(acc.Extra, ExtraAIMonthRechargedSource) != "billing" {
		t.Fatalf("source=%q", extraString(acc.Extra, ExtraAIMonthRechargedSource))
	}
}

func TestResolveAccountMoney_Sub2APIMonthRechargeFromHistory(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"sub2api.key_billing","resolved_rate_multiplier":0.05,"group_rate_multiplier":0.05,"effective_rate_multiplier":0.05}`)
	})
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"balance":3}`)
	})
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"access_token": "jwt-month", "expires_in": 7200},
		})
	})
	mux.HandleFunc("/api/v1/redeem/month-stats", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/v1/redeem/history", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer jwt-month") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		now := timezone.Now()
		used := time.Date(now.Year(), now.Month(), 2, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": []map[string]any{
				{"type": "balance", "value": 30, "used_at": used},
				{"type": "admin_balance", "value": 5.5, "used_at": used},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	acc := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-month-hist", "base_url": srv.URL},
		Extra: map[string]any{
			ExtraUpstreamKind:          "sub2api",
			ExtraUpstreamPanelEmail:    "user@example.com",
			ExtraUpstreamPanelPassword: "secret",
		},
	}
	p := &AIPilotService{HTTP: srv.Client()}
	_, _, _, _ = p.ResolveAccountMoney(context.Background(), acc)
	if extraFloat(acc.Extra, ExtraAIMonthRechargedUSD) != 35.5 {
		t.Fatalf("month usd=%v extra=%+v", extraFloat(acc.Extra, ExtraAIMonthRechargedUSD), acc.Extra)
	}
	if extraString(acc.Extra, ExtraAIMonthRechargedSource) != "redeem_history" {
		t.Fatalf("source=%q extra=%+v", extraString(acc.Extra, ExtraAIMonthRechargedSource), acc.Extra)
	}
}

func TestResolveAccountMoney_NewAPISkipsMonthRecharge(t *testing.T) {
	t.Parallel()
	var historyHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/v1/redeem/history", func(w http.ResponseWriter, r *http.Request) {
		historyHits++
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	acc := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-newapi-skip", "base_url": srv.URL},
		Extra: map[string]any{
			ExtraUpstreamKind:          "newapi",
			ExtraUpstreamPanelEmail:    "user@example.com",
			ExtraUpstreamPanelPassword: "secret",
		},
	}
	p := &AIPilotService{HTTP: srv.Client()}
	_, _, _, _ = p.ResolveAccountMoney(context.Background(), acc)
	if historyHits != 0 {
		t.Fatalf("newapi must not hit redeem history, hits=%d", historyHits)
	}
	if extraString(acc.Extra, ExtraAIMonthRechargedPeriod) != "" {
		t.Fatalf("newapi must not store month recharge: %+v", acc.Extra)
	}
}
