package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
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
// Official OAuth 429 still keeps sticky (short-lived, prompt cache). Reseller
// API-key 429 does not: newapi "Too many pending requests" is that supplier's
// concurrency ceiling, and keeping sticky turns Codex reconnect 5/5 into a
// fail loop on the same dead key.
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
		return openAIStickyResellerAccount(account)
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
		strings.Contains(msg, "request could not be completed") ||
		strings.Contains(msg, "currently overloaded") ||
		strings.Contains(msg, "overloaded") ||
		strings.Contains(msg, "too many pending") ||
		strings.Contains(msg, "temporarily unavailable")
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
// reseller (API-key 502/503/504/524/429). Official OAuth 429 and 502/524 keep
// sticky for prompt cache unless first-output hang or capacity-shed.
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
	} else if openAIStickyResellerAccount(account) && (openAIStickyDeadSupplierStatus(status) || status == http.StatusTooManyRequests) {
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
	s.abandonOpenAIStickyForAccount(ctx, groupID, sessionHash, openAIPreviousResponseIDFromCtx(ctx), account, reason)
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
		s.abandonOpenAIStickyForAccount(ctx, groupID, sessionHash, previousResponseID, account, "capacity_shed_after_output")
		return
	}
	if ShouldClearStickyOnOpenAIStreamFailure(err, account) {
		s.abandonOpenAIStickyForAccount(ctx, groupID, sessionHash, previousResponseID, account, "stream_failure")
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

func (s *OpenAIGatewayService) abandonOpenAIStickyForAccount(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	previousResponseID string,
	account *Account,
	reason string,
) {
	s.abandonOpenAISticky(ctx, groupID, sessionHash, previousResponseID, reason)
	if account != nil {
		rememberOpenAIStickySkip(sessionHash, account.ID)
	}
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

// Session-scoped skip: after a dead-supplier / overload abandon, the next
// continue must not immediately re-select the same still-schedulable account
// (legacy load-balance is priority+LRU; advanced scheduler is off in prod).
const openAIStickySkipTTL = 10 * time.Minute

type openAIStickySkipSet struct {
	mu      sync.Mutex
	expires map[int64]int64 // accountID -> unix nano
}

var openAIStickySkipBySession sync.Map // sessionHash -> *openAIStickySkipSet

func rememberOpenAIStickySkip(sessionHash string, accountID int64) {
	sessionHash = strings.TrimSpace(sessionHash)
	if sessionHash == "" || accountID <= 0 {
		return
	}
	now := time.Now()
	raw, _ := openAIStickySkipBySession.LoadOrStore(sessionHash, &openAIStickySkipSet{
		expires: make(map[int64]int64),
	})
	set, _ := raw.(*openAIStickySkipSet)
	if set == nil {
		return
	}
	set.mu.Lock()
	if set.expires == nil {
		set.expires = make(map[int64]int64)
	}
	set.expires[accountID] = now.Add(openAIStickySkipTTL).UnixNano()
	set.mu.Unlock()
}

func openAIStickySkipAccountIDs(sessionHash string) map[int64]struct{} {
	sessionHash = strings.TrimSpace(sessionHash)
	if sessionHash == "" {
		return nil
	}
	raw, ok := openAIStickySkipBySession.Load(sessionHash)
	if !ok {
		return nil
	}
	set, _ := raw.(*openAIStickySkipSet)
	if set == nil {
		return nil
	}
	now := time.Now().UnixNano()
	set.mu.Lock()
	defer set.mu.Unlock()
	out := make(map[int64]struct{}, len(set.expires))
	for id, exp := range set.expires {
		if exp <= now {
			delete(set.expires, id)
			continue
		}
		out[id] = struct{}{}
	}
	if len(set.expires) == 0 {
		openAIStickySkipBySession.Delete(sessionHash)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneInt64Set(in map[int64]struct{}) map[int64]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[int64]struct{}, len(in))
	for id := range in {
		out[id] = struct{}{}
	}
	return out
}

func mergeOpenAIStickySkipExclusions(sessionHash string, excludedIDs map[int64]struct{}) map[int64]struct{} {
	skip := openAIStickySkipAccountIDs(sessionHash)
	if len(skip) == 0 {
		return excludedIDs
	}
	if len(excludedIDs) == 0 {
		return skip
	}
	merged := cloneInt64Set(excludedIDs)
	if merged == nil {
		merged = make(map[int64]struct{}, len(skip))
	}
	for id := range skip {
		merged[id] = struct{}{}
	}
	return merged
}

func accountIDsFromOpenAIAccounts(accounts []Account) []int64 {
	if len(accounts) == 0 {
		return nil
	}
	ids := make([]int64, len(accounts))
	for i := range accounts {
		ids[i] = accounts[i].ID
	}
	return ids
}

// stickySkipExclusionsOrKeepPool applies session sticky-skip unless that would
// exclude every account in the pool. Skip exists so a multi-account group can
// leave a dead reseller; with pool=1 one Coco 502 otherwise becomes 10 minutes
// of 30ms "no available accounts" for that session.
func stickySkipExclusionsOrKeepPool(sessionHash string, requestExcludedIDs map[int64]struct{}, accountIDs []int64) map[int64]struct{} {
	skip := openAIStickySkipAccountIDs(sessionHash)
	if len(skip) == 0 {
		return requestExcludedIDs
	}
	merged := mergeOpenAIStickySkipExclusions(sessionHash, requestExcludedIDs)
	if len(accountIDs) == 0 {
		return merged
	}
	for _, id := range accountIDs {
		if _, excluded := merged[id]; !excluded {
			return merged
		}
	}
	return requestExcludedIDs
}

func resetOpenAIStickySkipForTest() {
	openAIStickySkipBySession = sync.Map{}
}
