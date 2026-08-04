package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// ShouldClearStickyOnOpenAIFailover reports whether a failover error should
// drop the session→account sticky binding so the next request (or the next
// selection in this request) does not re-stick to a known-bad account.
//
// Covers hang/timeout (first_output → 504 SafeToFailoverAfterWrite), upstream
// 5xx/524, and 429. Does not clear for credential/request-scoped stop actions
// that cannot be fixed by another account.
//
// Intentionally does NOT temp-unschedule the account: first_output timeouts are
// frequent under load and cooling would continuously empty the pool. Sticky
// clear alone is enough to stop session pile-on; account health remains the
// job of rate-limit / transport / ops rules.
func ShouldClearStickyOnOpenAIFailover(failoverErr *UpstreamFailoverError) bool {
	if failoverErr == nil {
		return false
	}
	if !failoverErr.ShouldRetryNextAccount() && !failoverErr.SafeToFailoverAfterWrite {
		return false
	}
	if failoverErr.SafeToFailoverAfterWrite {
		return true
	}
	switch failoverErr.StatusCode {
	case http.StatusTooManyRequests, // 429
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout,     // 504
		524:                           // Cloudflare/origin timeout
		return true
	default:
		return false
	}
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
// after a hang/5xx/429 failover. Call on both switch-away and failover-exhausted
// paths so the next request does not re-stick to a bad account.
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
	if status > 0 {
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
