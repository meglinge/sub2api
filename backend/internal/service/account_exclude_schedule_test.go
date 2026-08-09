package service

import (
	"context"
	"testing"
)

func TestIsExcludedFromScheduleAndIsSchedulable(t *testing.T) {
	t.Parallel()
	acc := &Account{
		Status: StatusActive, Schedulable: true, Extra: map[string]any{
			ExtraExcludeFromSchedule: true,
		},
	}
	if !acc.IsExcludedFromSchedule() {
		t.Fatal("expected excluded")
	}
	if acc.IsSchedulable() {
		t.Fatal("excluded account must not be schedulable for normal traffic")
	}
	if !acc.IsSchedulableForRequest(true) {
		t.Fatal("control-plane request should allow excluded account when healthy")
	}
	if acc.IsSchedulableForRequest(false) {
		t.Fatal("normal request must not allow excluded account")
	}
	acc.Extra[ExtraExcludeFromSchedule] = false
	if !acc.IsSchedulable() {
		t.Fatal("expected schedulable when flag cleared")
	}
}

func TestAllowControlPlaneScheduleFromHeader(t *testing.T) {
	t.Parallel()
	ctx := WithSub2APIClient(context.Background(), Sub2APIClientAIAutopilot)
	if !AllowControlPlaneSchedule(ctx) {
		t.Fatal("expected allow for ai-autopilot client")
	}
	if AllowControlPlaneSchedule(context.Background()) {
		t.Fatal("expected deny without client tag")
	}
}
