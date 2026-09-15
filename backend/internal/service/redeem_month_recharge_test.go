package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

type monthSumRedeemRepo struct {
	redeemRejectRepo
	userID int64
	from   time.Time
	to     time.Time
	sum    float64
}

func (r *monthSumRedeemRepo) SumPositiveBalanceByUserInRange(_ context.Context, userID int64, from, to time.Time) (float64, error) {
	r.userID = userID
	r.from = from
	r.to = to
	return r.sum, nil
}

func TestRedeemServiceCurrentMonthRecharged(t *testing.T) {
	repo := &monthSumRedeemRepo{sum: 77.5}
	svc := NewRedeemService(repo, nil, nil, nil, nil, nil, nil, nil)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, timezone.Location())

	amount, period, err := svc.CurrentMonthRecharged(context.Background(), 9, now)
	require.NoError(t, err)
	require.Equal(t, 77.5, amount)
	require.Equal(t, "2026-09", period)
	require.Equal(t, int64(9), repo.userID)
	require.True(t, repo.from.Equal(timezone.StartOfMonth(now)))
	require.True(t, repo.to.Equal(timezone.StartOfMonth(now).AddDate(0, 1, 0)))
}

func TestRedeemServiceCurrentMonthRechargedUnavailable(t *testing.T) {
	svc := NewRedeemService(nil, nil, nil, nil, nil, nil, nil, nil)
	_, _, err := svc.CurrentMonthRecharged(context.Background(), 1, time.Time{})
	require.Error(t, err)
}
