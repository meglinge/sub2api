//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type openAI403ProbeStub struct {
	results    []*ScheduledTestResult
	errs       []error
	calls      int
	accountIDs []int64
	models     []string
}

func (s *openAI403ProbeStub) RunTestBackground(ctx context.Context, accountID int64, modelID string) (*ScheduledTestResult, error) {
	_ = ctx
	s.calls++
	s.accountIDs = append(s.accountIDs, accountID)
	s.models = append(s.models, modelID)

	idx := s.calls - 1
	var result *ScheduledTestResult
	if idx < len(s.results) {
		result = s.results[idx]
	}
	var err error
	if idx < len(s.errs) {
		err = s.errs[idx]
	}
	return result, err
}

func TestRateLimitService_HandleUpstreamError_OpenAI403ProbeSuccessSkipsMarking(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	probe := &openAI403ProbeStub{
		results: []*ScheduledTestResult{{Status: "success", ResponseText: "ok", LatencyMs: 123}},
	}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetOpenAI403Probe(probe)
	account := &Account{ID: 201, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, nil)

	require.False(t, shouldDisable)
	require.Equal(t, 0, repo.setErrorCalls)
	require.Equal(t, 1, probe.calls)
	require.Equal(t, []int64{account.ID}, probe.accountIDs)
	require.Equal(t, []string{openAI403ProbeModel}, probe.models)
}

func TestRateLimitService_HandleUpstreamError_OpenAI403ProbeFailuresMarkAccount(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	probe := &openAI403ProbeStub{
		results: []*ScheduledTestResult{{Status: "failed", ErrorMessage: "first 403"}},
		errs:    []error{nil, errors.New("second 403")},
	}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetOpenAI403Probe(probe)
	account := &Account{ID: 202, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, nil)

	require.True(t, shouldDisable)
	require.Equal(t, 1, repo.setErrorCalls)
	require.Equal(t, openAI403ProbeAttempts, probe.calls)
	require.Equal(t, openAI403GenericErrorMessage, repo.lastErrorMsg)
	for _, modelID := range probe.models {
		require.Equal(t, openAI403ProbeModel, modelID)
	}
	for _, accountID := range probe.accountIDs {
		require.Equal(t, account.ID, accountID)
	}
}

func TestRateLimitService_HandleUpstreamError_OpenAI403SpecificMessageSkipsProbe(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	probe := &openAI403ProbeStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetOpenAI403Probe(probe)
	account := &Account{ID: 203, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	body := []byte(`{"error":{"message":"workspace access denied"}}`)

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, body)

	require.True(t, shouldDisable)
	require.Equal(t, 1, repo.setErrorCalls)
	require.Equal(t, 0, probe.calls)
	require.Equal(t, "Access forbidden (403): workspace access denied", repo.lastErrorMsg)
}
