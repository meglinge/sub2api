package service

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// openAIHangCoolDuration is a short cool-down after hang-style failures
// (first-output timeout / gateway timeout) so concurrent sticky sessions
// stop piling onto the same overloaded account. Keep this shorter than
// transport-error cooling: hangs are often transient under load.
const openAIHangCoolDuration = 2 * time.Minute

// ShouldClearStickyOnOpenAIFailover reports whether a failover error should
// drop the session→account sticky binding so the next request (or the next
// selection in this request) does not re-stick to a known-bad account.
//
// Covers hang/timeout (first_output → 504 SafeToFailoverAfterWrite), upstream
// 5xx/524, and 429. Does not clear for credential/request-scoped stop actions
// that cannot be fixed by another account.
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

// ShouldCoolAccountOnOpenAIFailover reports whether the failed account should
// receive a short temp-unschedulable cool-down in addition to sticky clear.
// Limited to hang/timeout paths so a burst of 429s does not empty the pool.
func ShouldCoolAccountOnOpenAIFailover(failoverErr *UpstreamFailoverError) bool {
	if failoverErr == nil {
		return false
	}
	if failoverErr.SafeToFailoverAfterWrite {
		return true
	}
	return failoverErr.StatusCode == http.StatusGatewayTimeout || failoverErr.StatusCode == 524
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

// CoolAccountAfterHang marks an account temporarily unschedulable after a
// hang/timeout so concurrent sticky hits stop selecting it for a short window.
func (s *OpenAIGatewayService) CoolAccountAfterHang(
	ctx context.Context,
	account *Account,
	reason string,
) {
	if s == nil || account == nil {
		return
	}
	until := time.Now().Add(openAIHangCoolDuration)
	coolReason := strings.TrimSpace(reason)
	if coolReason == "" {
		coolReason = "hang_timeout"
	}
	fullReason := "openai hang cool-down: " + coolReason

	// Immediate in-memory block (scheduler selection honours this before DB
	// cache refresh), matching transport-error cool-down behaviour.
	s.BlockAccountScheduling(account, until, coolReason)

	if s.accountRepo == nil {
		logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
			"openai.account_temp_unscheduled_hang_memory_only",
			zap.Int64("account_id", account.ID),
			zap.String("account_name", account.Name),
			zap.Time("until", until),
			zap.String("reason", fullReason),
		)
		return
	}

	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIAccountStateUpdateTimeout)
	defer cancel()
	if err := s.accountRepo.SetTempUnschedulable(bgCtx, account.ID, until, fullReason); err != nil {
		logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
			"openai.account_temp_unscheduled_hang_failed",
			zap.Int64("account_id", account.ID),
			zap.Error(err),
		)
		return
	}

	logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
		"openai.account_temp_unscheduled_hang",
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.Time("until", until),
		zap.String("reason", fullReason),
	)
}

// HandleOpenAIFailoverStickyFailure clears sticky (and optionally cools the
// account) after a hang/5xx/429 failover. Call on both switch-away and
// failover-exhausted paths so the next request does not re-stick to a bad account.
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
	s.ClearStickySessionOnFailure(ctx, groupID, sessionHash, reason)
	if ShouldCoolAccountOnOpenAIFailover(failoverErr) && account != nil {
		s.CoolAccountAfterHang(ctx, account, reason)
	}
}
