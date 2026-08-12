package service

import (
	"context"
	"log/slog"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// withGatewayProfitControlGate installs the gate only for explicitly marked
// token requests. This keeps media, metadata, and models-list paths outside
// the profit-control surface by construction.
func (s *GatewayService) withGatewayProfitControlGate(ctx context.Context, groupID *int64) context.Context {
	if _, ok := gatewayTokenRequestPricingAtFromContext(ctx); !ok || groupID == nil || *groupID <= 0 {
		return ctx
	}
	if existing, ok := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate); ok && existing != nil && existing.groupID == *groupID {
		return ctx
	}

	group, err := s.resolveProfitControlGroup(ctx, *groupID)
	if err != nil {
		slog.Warn("profit_control_group_load_failed", "group_id", *groupID, "error", err)
		return s.clearForeignProfitControlGate(ctx, groupID)
	}
	if group == nil || !group.ProfitControlEnabled || !profitControlPlatformSupported(group.Platform) {
		return s.clearForeignProfitControlGate(ctx, groupID)
	}

	pricingAt, _ := gatewayTokenRequestPricingAtFromContext(ctx)
	billingGroup := gatewayTokenRequestBillingGroupFromContext(ctx)
	if billingGroup == nil {
		if ctxGroup, ok := ctx.Value(ctxkey.Group).(*Group); ok && IsGroupContextValid(ctxGroup) {
			billingGroup = ctxGroup
		} else {
			billingGroup = group
		}
	}

	downstream := billingGroup.RateMultiplier
	if userID, _ := ctx.Value(ctxkey.UserID).(int64); userID > 0 {
		downstream = s.ResolveUserGroupRateMultiplier(ctx, userID, billingGroup.ID, billingGroup.RateMultiplier)
	}
	downstream *= billingGroup.PeakMultiplierAt(pricingAt)
	threshold := clampProfitControlThreshold(downstream * (1 - group.ProfitMinMargin - group.ProfitSafetyBuffer))

	gate := &openAIProfitControlGate{
		groupID:   group.ID,
		platform:  group.Platform,
		threshold: threshold,
		pricingAt: pricingAt,
	}
	openAIProfitControlObserverInstance.recordInstall(gate.groupID, gate.platform, gate.threshold)
	return context.WithValue(ctx, openAIProfitControlGateCtxKey{}, gate)
}

func (s *GatewayService) clearForeignProfitControlGate(ctx context.Context, groupID *int64) context.Context {
	existing, ok := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	if !ok || existing == nil || groupID == nil || existing.groupID == *groupID {
		return ctx
	}
	return context.WithValue(ctx, openAIProfitControlGateCtxKey{}, (*openAIProfitControlGate)(nil))
}

func (s *GatewayService) resolveProfitControlGroup(ctx context.Context, groupID int64) (*Group, error) {
	if group, ok := ctx.Value(ctxkey.Group).(*Group); ok && IsGroupContextValid(group) && group.ID == groupID {
		return group, nil
	}
	if s.schedulerSnapshot != nil {
		// Lite 读取：门只用平台/倍率/利润/高峰字段，不需要账号计数聚合。
		return s.schedulerSnapshot.GetGroupByIDLite(ctx, groupID)
	}
	return s.resolveGroupByID(ctx, groupID)
}

// GatewayProfitControlVetoLatest performs the terminal post-slot check against
// the latest scheduler snapshot. Snapshot read failures are deliberately
// fail-open to preserve availability, but are observable.
func (s *GatewayService) GatewayProfitControlVetoLatest(ctx context.Context, selected *Account) (*Account, bool, string) {
	return profitControlVetoLatest(ctx, selected, s.schedulerSnapshot)
}

func profitControlVetoLatest(ctx context.Context, selected *Account, snapshot *SchedulerSnapshotService) (*Account, bool, string) {
	if selected == nil {
		return nil, true, "nil_account"
	}
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	latest := selected
	if snapshot != nil {
		refreshed, err := snapshot.GetAccount(ctx, selected.ID)
		if err != nil || refreshed == nil {
			if gate != nil {
				slog.Warn("profit_control_account_refresh_failed", "group_id", gate.groupID, "platform", gate.platform, "account_id", selected.ID, "error", err)
				openAIProfitControlObserverInstance.recordRefreshFailure(gate.groupID, gate.platform, gate.threshold)
			}
		} else if !refreshed.UpdatedAt.Before(selected.UpdatedAt) {
			// 选号路径可能已做过 DB recheck，selected 比缓存快照更新鲜；只有
			// 快照不落后时才替换，避免终检把新鲜账号换回较旧的缓存对象。
			latest = refreshed
		}
	}
	// Always re-check schedulability after slot acquire (WaitPlan may outlive a
	// mid-wait ai_disabled / weight=0 soft quarantine). Profit gate may be absent.
	if reason := postSlotAdmissionVetoReason(ctx, latest); reason != "" {
		return latest, true, reason
	}
	if gate == nil {
		return latest, false, ""
	}
	vetoed, reason := openAIProfitControlVetoReason(ctx, latest)
	return latest, vetoed, reason
}

// postSlotAdmissionVetoReason rejects accounts that became unusable between
// selection and slot acquire (ai_disabled etc.). Soft-weight last-resort is
// decided at selection time (preferPrimaryAccounts), not vetoed here — otherwise
// ultimate-spare would never be able to serve when cheap peers are all dead.
func postSlotAdmissionVetoReason(ctx context.Context, account *Account) string {
	if account == nil {
		return "nil_account"
	}
	if !account.IsSchedulableForRequest(AllowControlPlaneSchedule(ctx)) {
		if account.AIDisabled {
			return "ai_disabled"
		}
		return "not_schedulable"
	}
	return ""
}

func (s *GatewayService) isGatewayAccountProfitEligible(ctx context.Context, account *Account) bool {
	vetoed, _ := openAIProfitControlVetoReason(ctx, account)
	return !vetoed
}

func gatewayProfitControlGateActive(ctx context.Context) bool {
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	return gate != nil
}
