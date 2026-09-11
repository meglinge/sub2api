package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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
	}), "without an account, 502 still keeps sticky")
	require.False(t, ShouldClearStickyOnOpenAIFailover(&UpstreamFailoverError{
		StatusCode:        524,
		NextAccountAction: NextAccountRetry,
	}))

	capacityBody := []byte(`{"error":{"type":"service_unavailable_error","code":"server_error","message":"Our servers are currently overloaded. Please try again later."}}`)
	capacityErr := newOpenAIUpstreamFailoverError(
		http.StatusServiceUnavailable,
		nil,
		capacityBody,
		"Our servers are currently overloaded. Please try again later.",
		true,
	)
	require.True(t, capacityErr.IsOpenAICapacityShed())
	require.True(t, ShouldClearStickyOnOpenAIFailover(capacityErr),
		"overloaded must drop sticky so the next turn does not pin a dead reseller")

	reseller := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	gatewayTimeout := &UpstreamFailoverError{StatusCode: http.StatusBadGateway, NextAccountAction: NextAccountRetry}
	require.True(t, shouldClearOpenAIStickyForFailover(gatewayTimeout, reseller),
		"reseller API-key 502 must drop sticky so continue can leave a dead supplier")
	require.False(t, shouldClearOpenAIStickyForFailover(gatewayTimeout, oauth),
		"official OAuth 502 still keeps sticky for prompt cache")
	require.True(t, shouldClearOpenAIStickyForFailover(&UpstreamFailoverError{StatusCode: 524}, reseller))
	require.False(t, shouldClearOpenAIStickyForFailover(&UpstreamFailoverError{StatusCode: http.StatusTooManyRequests}, reseller),
		"429 still keeps sticky")
}

func TestShouldClearStickyOnOpenAIStreamFailure(t *testing.T) {
	t.Parallel()

	reseller := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	require.False(t, ShouldClearStickyOnOpenAIStreamFailure(nil, reseller))
	require.False(t, ShouldClearStickyOnOpenAIStreamFailure(errors.New("stream read error: eof"), oauth),
		"OAuth stream death keeps sticky")
	require.False(t, ShouldClearStickyOnOpenAIStreamFailure(context.Canceled, reseller))
	require.False(t, ShouldClearStickyOnOpenAIStreamFailure(errors.New("stream usage incomplete after disconnect: eof"), reseller))

	require.True(t, ShouldClearStickyOnOpenAIStreamFailure(
		errors.New("OpenAI stream disconnected before completion: stream closed before response.completed"),
		reseller,
	))
	require.True(t, ShouldClearStickyOnOpenAIStreamFailure(errors.New("stream usage incomplete: missing terminal event"), reseller))
	require.True(t, ShouldClearStickyOnOpenAIStreamFailure(errors.New("stream read error: unexpected EOF"), reseller))
	require.True(t, ShouldClearStickyOnOpenAIStreamFailure(errors.New("Upstream produced no output before the deadline"), reseller))
	require.True(t, ShouldClearStickyOnOpenAIStreamFailure(errors.New("Upstream request failed"), reseller))
	require.True(t, ShouldClearStickyOnOpenAIStreamFailure(errors.New("Request could not be completed"), reseller))
	require.True(t, ShouldClearStickyOnOpenAIStreamFailure(
		errors.New("upstream response failed: Our servers are currently overloaded. Please try again later."),
		reseller,
	))
}

func TestOpenAIStickySkipExcludesAccountForSession(t *testing.T) {
	t.Parallel()
	resetOpenAIStickySkipForTest()
	t.Cleanup(resetOpenAIStickySkipForTest)

	rememberOpenAIStickySkip("sess-skip", 6509)
	got := openAIStickySkipAccountIDs("sess-skip")
	require.Contains(t, got, int64(6509))
	merged := mergeOpenAIStickySkipExclusions("sess-skip", map[int64]struct{}{1: {}})
	require.Contains(t, merged, int64(6509))
	require.Contains(t, merged, int64(1))
	require.Nil(t, openAIStickySkipAccountIDs("other"))
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

func TestHandleOpenAIFailoverStickyFailure_Reseller502ClearsStickyAndPreviousResponse(t *testing.T) {
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{
			"openai:sess-dead": 6520,
		},
	}
	svc := &OpenAIGatewayService{cache: cache}
	groupID := int64(2)
	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindResponseAccount(context.Background(), groupID, "resp_prev_dead", 6520, time.Hour))

	ctx := WithOpenAIPreviousResponseID(context.Background(), "resp_prev_dead")
	svc.HandleOpenAIFailoverStickyFailure(
		ctx,
		&groupID,
		"sess-dead",
		&Account{ID: 6520, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		&UpstreamFailoverError{StatusCode: http.StatusBadGateway, NextAccountAction: NextAccountRetry},
	)

	_, exists := cache.sessionBindings["openai:sess-dead"]
	require.False(t, exists, "reseller 502 must delete session sticky")
	got, err := store.GetResponseAccount(context.Background(), groupID, "resp_prev_dead")
	require.NoError(t, err)
	require.Equal(t, int64(0), got, "reseller 502 must delete previous_response_id sticky")
}

func TestHandleOpenAIPostOutputStickyFailure_StreamDisconnectClearsSticky(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{
			"openai:sess-stream": 6520,
		},
	}
	svc := &OpenAIGatewayService{cache: cache}
	groupID := int64(2)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	svc.HandleOpenAIPostOutputStickyFailure(
		context.Background(),
		c,
		&groupID,
		"sess-stream",
		&Account{ID: 6520, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		errors.New("OpenAI stream disconnected before completion: stream closed before response.completed"),
	)

	_, exists := cache.sessionBindings["openai:sess-stream"]
	require.False(t, exists, "stream disconnect after output must drop sticky so continue can switch")
}

func TestHandleOpenAIFailoverStickyFailure_OAuth502KeepsSticky(t *testing.T) {
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{
			"openai:sess-oauth": 99,
		},
	}
	svc := &OpenAIGatewayService{cache: cache}
	groupID := int64(2)

	svc.HandleOpenAIFailoverStickyFailure(
		context.Background(),
		&groupID,
		"sess-oauth",
		&Account{ID: 99, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		&UpstreamFailoverError{StatusCode: http.StatusBadGateway, NextAccountAction: NextAccountRetry},
	)

	require.Equal(t, int64(99), cache.sessionBindings["openai:sess-oauth"], "official OAuth 502 must keep sticky")
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

func TestHandleOpenAIFailoverStickyFailure_CapacityShedClearsSticky(t *testing.T) {
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{
			"openai:sess-overload": 6509,
		},
	}
	svc := &OpenAIGatewayService{cache: cache}
	groupID := int64(5)
	body := []byte(`{"error":{"type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}`)
	failoverErr := newOpenAIUpstreamFailoverError(
		http.StatusServiceUnavailable,
		nil,
		body,
		"Our servers are currently overloaded. Please try again later.",
		true,
	)

	svc.HandleOpenAIFailoverStickyFailure(
		context.Background(),
		&groupID,
		"sess-overload",
		&Account{ID: 6509, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		failoverErr,
	)

	_, exists := cache.sessionBindings["openai:sess-overload"]
	require.False(t, exists, "capacity shed must delete sticky so the next request can leave the overloaded account")
}

func TestClearStickyIfCapacityShedAfterOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{
			"openai:sess-midstream": 6509,
		},
	}
	svc := &OpenAIGatewayService{cache: cache}
	groupID := int64(5)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	svc.ClearStickyIfCapacityShedAfterOutput(context.Background(), c, &groupID, "sess-midstream")
	require.Equal(t, int64(6509), cache.sessionBindings["openai:sess-midstream"],
		"must not clear sticky unless the stream marked post-output overload")

	MarkOpenAICapacityShedAfterOutput(c)
	svc.ClearStickyIfCapacityShedAfterOutput(context.Background(), c, &groupID, "sess-midstream")
	_, exists := cache.sessionBindings["openai:sess-midstream"]
	require.False(t, exists)
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
