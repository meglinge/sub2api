package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const openAICapacityShedAfterOutputKey = "openai_capacity_shed_after_output"

type openAIPreviousResponseIDCtxKey struct{}

// WithOpenAIPreviousResponseID records the inbound previous_response_id so
// sticky-abandon paths can drop the response_id→account binding together with
// the session_hash binding. Continue turns send the last successful
// response_id; leaving that mapping in place re-pins the dead supplier.
func WithOpenAIPreviousResponseID(ctx context.Context, previousResponseID string) context.Context {
	previousResponseID = strings.TrimSpace(previousResponseID)
	if ctx == nil || previousResponseID == "" {
		return ctx
	}
	return context.WithValue(ctx, openAIPreviousResponseIDCtxKey{}, previousResponseID)
}

func openAIPreviousResponseIDFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(openAIPreviousResponseIDCtxKey{}).(string)
	return strings.TrimSpace(id)
}

// ShouldClearStickyOnOpenAIFailover reports whether a failover error should
// drop the session→account sticky binding when the selected account is unknown.
// Prefer shouldClearOpenAIStickyForFailover when the account is available.
func ShouldClearStickyOnOpenAIFailover(failoverErr *UpstreamFailoverError) bool {
	return shouldClearOpenAIStickyForFailover(failoverErr, nil)
}

// shouldClearOpenAIStickyForFailover reports whether a failover error should
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
// Reseller API-key 502/503/504/524 are treated as a dead supplier: those
// credentials are independent resellers, and pinning a Codex session to one
// after "Upstream request failed" / stream disconnect makes every continue
// retry the same garbage account. Official OAuth 502/524 still keep sticky
// (shared pool, prompt cache) unless first-output hang or capacity-shed.
//
// Transient 429 still keep sticky. Rate-limit windows are short-lived, and
// clearing them splits one Codex session across suppliers.
//
// Intentionally does NOT temp-unschedule the account: first_output timeouts
// and brief overload bursts would continuously empty the pool. Account health
// remains the job of rate-limit / transport / ops rules.
func shouldClearOpenAIStickyForFailover(failoverErr *UpstreamFailoverError, account *Account) bool {
	if failoverErr == nil {
		return false
	}
	if failoverErr.SafeToFailoverAfterWrite {
		return true
	}
	if failoverErr.IsOpenAICapacityShed() {
		return true
	}
	if failoverErr.StatusCode == http.StatusTooManyRequests {
		return false
	}
	return openAIStickyResellerAccount(account) && openAIStickyDeadSupplierStatus(failoverErr.StatusCode)
}

func openAIStickyResellerAccount(account *Account) bool {
	return account != nil && account.IsOpenAI() && !account.IsOpenAIOAuthLike()
}

func openAIStickyDeadSupplierStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 524:
		return true
	default:
		return false
	}
}

// ShouldClearStickyOnOpenAIStreamFailure reports whether a non-failover stream
// error (typically after semantic output has started, so this request cannot
// switch accounts) should still drop sticky so the next continue can leave a
// dead reseller.
func ShouldClearStickyOnOpenAIStreamFailure(err error, account *Account) bool {
	if err == nil || !openAIStickyResellerAccount(account) {
		return false
	}
	var failoverErr *UpstreamFailoverError
	if errors.As(err, &failoverErr) {
		return shouldClearOpenAIStickyForFailover(failoverErr, account)
	}
	if isOpenAIStickyClientAbortError(err) {
		return false
	}
	return isOpenAIStickyStreamDeathError(err)
}

func isOpenAIStickyClientAbortError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "after disconnect") ||
		strings.Contains(msg, "client disconnected")
}

func isOpenAIStickyStreamDeathError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "stream disconnected") ||
		strings.Contains(msg, "missing terminal event") ||
		strings.Contains(msg, "stream read error") ||
		strings.Contains(msg, "stream ended before") ||
		strings.Contains(msg, "ended before completion") ||
		strings.Contains(msg, "produced no output") ||
		strings.Contains(msg, "produced no semantic output") ||
		strings.Contains(msg, "upstream request failed") ||
		strings.Contains(msg, "request could not be completed")
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
	// Request ctx is often already canceled (client gone / stream abort).
	// Cleanup must still reach Redis or the next continue re-pins the dead account.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := s.deleteStickySessionAccountID(cleanupCtx, groupID, sessionHash); err != nil {
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
// after a first-output hang, a recognized capacity-shed/overload, or a dead
// reseller (API-key 502/503/504/524). Transient 429 failovers keep the original
// binding so the next request retries the same supplier (prompt cache). Official
// OAuth 502/524 also keep sticky unless first-output hang or capacity-shed.
//
// Does not temp-unschedule the account (see shouldClearOpenAIStickyForFailover).
func (s *OpenAIGatewayService) HandleOpenAIFailoverStickyFailure(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	account *Account,
	failoverErr *UpstreamFailoverError,
) {
	if !shouldClearOpenAIStickyForFailover(failoverErr, account) {
		return
	}
	status := 0
	if failoverErr != nil {
		status = failoverErr.StatusCode
	}
	reason := "failover"
	if failoverErr != nil && failoverErr.IsOpenAICapacityShed() {
		reason = "capacity_shed"
	} else if openAIStickyResellerAccount(account) && openAIStickyDeadSupplierStatus(status) {
		reason = "dead_supplier"
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
	s.abandonOpenAISticky(ctx, groupID, sessionHash, openAIPreviousResponseIDFromCtx(ctx), reason)
}

// HandleOpenAIPostOutputStickyFailure drops sticky when the stream already
// wrote semantic output (this request cannot failover) but the next continue
// must not stay pinned to a dead reseller or an overloaded account.
func (s *OpenAIGatewayService) HandleOpenAIPostOutputStickyFailure(
	ctx context.Context,
	c *gin.Context,
	groupID *int64,
	sessionHash string,
	account *Account,
	err error,
) {
	previousResponseID := openAIPreviousResponseIDFromCtx(ctx)
	if OpenAICapacityShedAfterOutput(c) {
		s.abandonOpenAISticky(ctx, groupID, sessionHash, previousResponseID, "capacity_shed_after_output")
		return
	}
	if ShouldClearStickyOnOpenAIStreamFailure(err, account) {
		s.abandonOpenAISticky(ctx, groupID, sessionHash, previousResponseID, "stream_failure")
	}
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
	s.HandleOpenAIPostOutputStickyFailure(ctx, c, groupID, sessionHash, nil, nil)
}

func (s *OpenAIGatewayService) abandonOpenAISticky(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	previousResponseID string,
	reason string,
) {
	s.ClearStickySessionOnFailure(ctx, groupID, sessionHash, reason)
	s.clearPreviousResponseSticky(ctx, groupID, previousResponseID)
}

func (s *OpenAIGatewayService) clearPreviousResponseSticky(ctx context.Context, groupID *int64, previousResponseID string) {
	previousResponseID = strings.TrimSpace(previousResponseID)
	if s == nil || previousResponseID == "" {
		return
	}
	store := s.getOpenAIWSStateStore()
	if store == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := store.DeleteResponseAccount(cleanupCtx, derefGroupID(groupID), previousResponseID); err != nil {
		logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
			"openai.previous_response_sticky_clear_failed",
			zap.Int64("group_id", derefGroupID(groupID)),
			zap.Error(err),
		)
		return
	}
	logger.L().With(zap.String("component", "service.openai_gateway")).Info(
		"openai.previous_response_sticky_cleared",
		zap.Int64("group_id", derefGroupID(groupID)),
	)
}
