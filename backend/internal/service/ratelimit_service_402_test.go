//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestHandleUpstreamError_APIKey402TempUnschedulesInsteadOfSetError(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       6550,
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Status:   StatusActive,
		Credentials: map[string]any{
			"base_url": "https://api.codekey.ai",
		},
	}

	shouldDisable := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusPaymentRequired,
		http.Header{},
		[]byte(`{"error":{"message":"Insufficient account balance"}}`),
	)

	require.True(t, shouldDisable)
	require.Zero(t, repo.setErrorCalls)
	require.Equal(t, 1, repo.tempCalls)
	require.Equal(t, int64(6550), repo.lastTempID)
	require.Contains(t, repo.lastTempReason, paymentRequiredReasonPrefix)
	require.Contains(t, repo.lastTempReason, "Insufficient account balance")
}

func TestHandleUpstreamError_PaymentRequiredBodyTempUnschedules(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       6509,
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Status:   StatusActive,
		Credentials: map[string]any{
			"pool_mode": true,
			"base_url":  "https://api.lukyface.com",
		},
	}

	shouldDisable := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusPaymentRequired,
		http.Header{},
		[]byte(`{"error":{"message":"Payment Required"}}`),
	)

	require.True(t, shouldDisable)
	require.Zero(t, repo.setErrorCalls)
	require.Equal(t, 1, repo.tempCalls)
	require.Contains(t, repo.lastTempReason, "Payment Required")
}

func TestHandleUpstreamError_OAuthDeactivatedWorkspace402StillSetError(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := newTeamLinkedAccount(1, "team-A")

	shouldDisable := svc.HandleUpstreamError(
		context.Background(),
		&account,
		http.StatusPaymentRequired,
		http.Header{},
		[]byte(teamLinkedDeactivatedBody),
	)

	require.True(t, shouldDisable)
	require.Equal(t, 1, repo.setErrorCalls)
	require.Zero(t, repo.tempCalls)
	require.Contains(t, repo.lastErrorMsg, "Workspace deactivated (402)")
}

func TestHandleUpstreamError_APIKeyWrappedDeactivatedWorkspace402IsNotPermanent(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       42,
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Status:   StatusActive,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}

	shouldDisable := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusPaymentRequired,
		http.Header{},
		[]byte(teamLinkedDeactivatedBody),
	)

	require.True(t, shouldDisable)
	require.Zero(t, repo.setErrorCalls)
	require.Equal(t, 1, repo.tempCalls)
}

func TestPaymentRequiredTempUnschedReason(t *testing.T) {
	require.Equal(t, "payment_required: Payment Required", paymentRequiredTempUnschedReason("Payment Required"))
	require.Equal(t, "payment_required: Payment required (402)", paymentRequiredTempUnschedReason("  "))
}

func TestOpenAI402CooldownIsThirtyMinutes(t *testing.T) {
	require.Equal(t, 30, openAI402CooldownMinutesDefault)
	require.Equal(t, 30*time.Minute, time.Duration(openAI402CooldownMinutesDefault)*time.Minute)
}
