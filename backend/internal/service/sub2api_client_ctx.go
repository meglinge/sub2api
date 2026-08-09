package service

import (
	"context"
	"strings"
)

type sub2APIClientCtxKey struct{}

// Sub2APIClientAIAutopilot is the value of X-Sub2API-Client set by the AI pilot LLM path.
const Sub2APIClientAIAutopilot = "ai-autopilot"

// WithSub2APIClient stores the inbound X-Sub2API-Client header for scheduling decisions.
func WithSub2APIClient(ctx context.Context, client string) context.Context {
	client = strings.TrimSpace(strings.ToLower(client))
	if client == "" {
		return ctx
	}
	return context.WithValue(ctx, sub2APIClientCtxKey{}, client)
}

// Sub2APIClientFromContext returns the client tag if present.
func Sub2APIClientFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(sub2APIClientCtxKey{}).(string)
	return v
}

// AllowControlPlaneSchedule is true when the request is allowed to select
// exclude_from_schedule accounts (AI autopilot LLM only).
func AllowControlPlaneSchedule(ctx context.Context) bool {
	return Sub2APIClientFromContext(ctx) == Sub2APIClientAIAutopilot
}
