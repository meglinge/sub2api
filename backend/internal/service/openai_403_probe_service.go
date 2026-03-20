package service

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type openAI403ProbeRunner interface {
	RunTestBackground(ctx context.Context, accountID int64, modelID string) (*ScheduledTestResult, error)
}

// OpenAI403ProbeService reuses the account test pipeline for ambiguous OpenAI 403 verification.
type OpenAI403ProbeService struct {
	accountTester *AccountTestService
}

func NewOpenAI403ProbeService(accountRepo AccountRepository, httpUpstream HTTPUpstream, cfg *config.Config) *OpenAI403ProbeService {
	if accountRepo == nil || httpUpstream == nil || cfg == nil {
		return &OpenAI403ProbeService{}
	}
	return &OpenAI403ProbeService{
		accountTester: NewAccountTestService(accountRepo, nil, nil, httpUpstream, cfg),
	}
}

func (s *OpenAI403ProbeService) RunTestBackground(ctx context.Context, accountID int64, modelID string) (*ScheduledTestResult, error) {
	if s == nil || s.accountTester == nil {
		return nil, errors.New("openai 403 probe service is not configured")
	}
	return s.accountTester.RunTestBackground(ctx, accountID, modelID)
}
