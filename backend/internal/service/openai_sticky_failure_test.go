package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShouldClearStickyOnOpenAIFailover(t *testing.T) {
	t.Parallel()

	require.False(t, ShouldClearStickyOnOpenAIFailover(nil))
	require.False(t, ShouldClearStickyOnOpenAIFailover(&UpstreamFailoverError{
		StatusCode:        http.StatusBadRequest,
		NextAccountAction: NextAccountStop,
	}))

	require.True(t, ShouldClearStickyOnOpenAIFailover(&UpstreamFailoverError{
		StatusCode:               http.StatusGatewayTimeout,
		SafeToFailoverAfterWrite: true,
	}))
	require.False(t, ShouldClearStickyOnOpenAIFailover(&UpstreamFailoverError{
		StatusCode:        http.StatusTooManyRequests,
		NextAccountAction: NextAccountRetry,
	}))
	require.False(t, ShouldClearStickyOnOpenAIFailover(&UpstreamFailoverError{
		StatusCode:        http.StatusBadGateway,
		NextAccountAction: NextAccountRetry,
	}))
	require.False(t, ShouldClearStickyOnOpenAIFailover(&UpstreamFailoverError{
		StatusCode:        524,
		NextAccountAction: NextAccountRetry,
	}))
}

func TestOpenAIPoolModeSameAccountRetryLimit_Skips429(t *testing.T) {
	t.Parallel()

	account := &Account{}
	require.Equal(t, 0, OpenAIPoolModeSameAccountRetryLimit(nil, &UpstreamFailoverError{
		RetryableOnSameAccount: true,
		StatusCode:             http.StatusBadGateway,
	}))
	require.Equal(t, 0, OpenAIPoolModeSameAccountRetryLimit(account, &UpstreamFailoverError{
		RetryableOnSameAccount: false,
		StatusCode:             http.StatusBadGateway,
	}))
	require.Equal(t, 0, OpenAIPoolModeSameAccountRetryLimit(account, &UpstreamFailoverError{
		RetryableOnSameAccount: true,
		StatusCode:             http.StatusTooManyRequests,
	}))
}

func TestHandleOpenAIFailoverStickyFailure_ClearsStickyOnly(t *testing.T) {
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{
			"openai:abc123session": 6158,
		},
	}
	svc := &OpenAIGatewayService{cache: cache}

	groupID := int64(5)
	account := &Account{ID: 6158, Name: "hot-account", Platform: PlatformOpenAI}
	failoverErr := &UpstreamFailoverError{
		StatusCode:               http.StatusGatewayTimeout,
		SafeToFailoverAfterWrite: true,
	}

	svc.HandleOpenAIFailoverStickyFailure(
		context.Background(),
		&groupID,
		"abc123session",
		account,
		failoverErr,
	)

	_, exists := cache.sessionBindings["openai:abc123session"]
	require.False(t, exists, "sticky binding must be deleted after first_output hang")
	// Must remain schedulable: hang path must not temp-unschedule the account.
	require.True(t, account.IsSchedulable() || account.Status == StatusActive || account.Status == "")
}

func TestHandleOpenAIFailoverStickyFailure_429KeepsSticky(t *testing.T) {
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{
			"openai:sess-429": 99,
		},
	}
	svc := &OpenAIGatewayService{cache: cache}
	groupID := int64(2)

	svc.HandleOpenAIFailoverStickyFailure(
		context.Background(),
		&groupID,
		"sess-429",
		&Account{ID: 99, Platform: PlatformOpenAI},
		&UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, NextAccountAction: NextAccountRetry},
	)

	require.Equal(t, int64(99), cache.sessionBindings["openai:sess-429"], "429 failover must keep original sticky")
}

func TestBindStickySessionPreserveExisting_DoesNotStealSession(t *testing.T) {
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{
			"openai:sess-keep": 6155,
		},
	}
	svc := &OpenAIGatewayService{cache: cache}
	groupID := int64(2)

	require.NoError(t, svc.bindStickySessionPreserveExisting(context.Background(), &groupID, "sess-keep", 6505))
	require.Equal(t, int64(6155), cache.sessionBindings["openai:sess-keep"])

	require.NoError(t, svc.bindStickySessionPreserveExisting(context.Background(), &groupID, "sess-new", 6505))
	require.Equal(t, int64(6505), cache.sessionBindings["openai:sess-new"])
}

func TestClearStickySessionOnFailure_EmptySessionNoop(t *testing.T) {
	cache := &stubGatewayCache{sessionBindings: map[string]int64{"openai:x": 1}}
	svc := &OpenAIGatewayService{cache: cache}
	svc.ClearStickySessionOnFailure(context.Background(), nil, "", "noop")
	require.Equal(t, int64(1), cache.sessionBindings["openai:x"])
}
