package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const openAICapacityShedAfterOutputKey = "openai_capacity_shed_after_output"

// ShouldClearStickyOnOpenAIFailover reports whether a failover error should
// drop the session→account sticky binding.
//
// First-output hangs (SafeToFailoverAfterWrite) abandon the binding: the
// current account stopped producing tokens after the client already received
// bytes, so the next request must not pile back onto it.
//
// Capacity shed / overloaded (RequestScopedTransient) also abandon the
// binding. Reseller API-key accounts do not share one upstream pool; pinning
// the session to the overloaded supplier turns the whole Codex window into a
// 3-minute fail loop. Official OAuth still retries the same credential on
// THIS request; sticky is cleared so the NEXT turn can pick a healthy account.
//
// Transient 429/502/524 still keep sticky. Those are common, short-lived, and
// clearing them splits one Codex session across suppliers and drops prompt
// cache to the shared ~3840-token prefix.
//
// Intentionally does NOT temp-unschedule the account: first_output timeouts
// and brief overload bursts would continuously empty the pool. Account health
// remains the job of rate-limit / transport / ops rules.
func ShouldClearStickyOnOpenAIFailover(failoverErr *UpstreamFailoverError) bool {
	if failoverErr == nil {
		return false
	}
	if failoverErr.SafeToFailoverAfterWrite {
		return true
	}
	return failoverErr.IsOpenAICapacityShed()
}

// MarkOpenAICapacityShedAfterOutput records that this request saw an upstream
// overload after semantic output had already been written. The stream cannot
// be replayed, but the session→account sticky binding must still be dropped.
func MarkOpenAICapacityShedAfterOutput(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(openAICapacityShedAfterOutputKey, true)
}

// OpenAICapacityShedAfterOutput reports whether MarkOpenAICapacityShedAfterOutput
// ran for this gin request.
func OpenAICapacityShedAfterOutput(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, ok := c.Get(openAICapacityShedAfterOutputKey)
	flagged, _ := value.(bool)
	return ok && flagged
}

// OpenAIPoolModeSameAccountRetryLimit returns how many same-account retries
// are allowed for a pool-mode failover error. 429s switch immediately: same-
// account retries only burn client first-byte budget while the account stays
// rate-limited.
func OpenAIPoolModeSameAccountRetryLimit(account *Account, failoverErr *UpstreamFailoverError) int {
	if account == nil || failoverErr == nil || !failoverErr.RetryableOnSameAccount {
		return 0
	}
	if failoverErr.StatusCode == http.StatusTooManyRequests {
		return 0
	}
	return account.GetPoolModeRetryCount()
}

// ClearStickySessionOnFailure deletes the sticky session→account binding.
// Safe no-op when sessionHash is empty or cache is unavailable.
func (s *OpenAIGatewayService) ClearStickySessionOnFailure(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	reason string,
) {
	if s == nil || strings.TrimSpace(sessionHash) == "" {
		return
	}
	if err := s.deleteStickySessionAccountID(ctx, groupID, sessionHash); err != nil {
		logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
			"openai.sticky_session_clear_failed",
			zap.Int64("group_id", derefGroupID(groupID)),
			zap.String("reason", reason),
			zap.Error(err),
		)
		return
	}
	logger.L().With(zap.String("component", "service.openai_gateway")).Info(
		"openai.sticky_session_cleared",
		zap.Int64("group_id", derefGroupID(groupID)),
		zap.String("reason", reason),
	)
}

// HandleOpenAIFailoverStickyFailure clears the session→account sticky binding
// after a first-output hang or a recognized capacity-shed/overload. Transient
// 429/502/524 failovers keep the original binding so the next request retries
// the same supplier (prompt cache).
//
// Does not temp-unschedule the account (see ShouldClearStickyOnOpenAIFailover).
func (s *OpenAIGatewayService) HandleOpenAIFailoverStickyFailure(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	account *Account,
	failoverErr *UpstreamFailoverError,
) {
	if !ShouldClearStickyOnOpenAIFailover(failoverErr) {
		return
	}
	status := 0
	if failoverErr != nil {
		status = failoverErr.StatusCode
	}
	reason := "failover"
	if failoverErr != nil && failoverErr.IsOpenAICapacityShed() {
		reason = "capacity_shed"
	} else if status > 0 {
		reason = "failover_" + http.StatusText(status)
		if reason == "failover_" {
			reason = "failover_status"
		}
		// StatusText is empty for 524; keep numeric fallback.
		if status == 524 {
			reason = "failover_524"
		} else if status == http.StatusGatewayTimeout && failoverErr.SafeToFailoverAfterWrite {
			reason = "first_output_timeout"
		}
	}
	_ = account // retained for call-site symmetry / future metrics
	s.ClearStickySessionOnFailure(ctx, groupID, sessionHash, reason)
}

// ClearStickyIfCapacityShedAfterOutput drops sticky when overload arrived after
// semantic output (this request cannot be replayed, but the next one must not
// stay pinned to the overloaded account).
func (s *OpenAIGatewayService) ClearStickyIfCapacityShedAfterOutput(
	ctx context.Context,
	c *gin.Context,
	groupID *int64,
	sessionHash string,
) {
	if !OpenAICapacityShedAfterOutput(c) {
		return
	}
	s.ClearStickySessionOnFailure(ctx, groupID, sessionHash, "capacity_shed_after_output")
}
