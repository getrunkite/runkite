package connector

import (
	"context"
	"net/http"
)

type actionCorrelationKey struct{}
type actionCorrelation struct{ action, lease string }

// WithMCPActionCorrelation is a trusted executor hook. An HTTP client cannot
// provide these fields; enabling headers is also an operator target setting.
func WithMCPActionCorrelation(ctx context.Context, action, lease string) context.Context {
	if !correlationID(action) || (lease != "" && !correlationID(lease)) {
		return ctx
	}
	return context.WithValue(ctx, actionCorrelationKey{}, actionCorrelation{action, lease})
}

func correlationID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

func applyActionCorrelation(ctx context.Context, headers http.Header) {
	target, ok := ctx.Value(mcpRequestTargetKey{}).(*mcpRequestTarget)
	if !ok || target.config.MCP == nil || !target.config.MCP.CorrelationHeaders {
		return
	}
	value, ok := ctx.Value(actionCorrelationKey{}).(actionCorrelation)
	if !ok {
		return
	}
	headers.Set("X-Runkite-Action-Id", value.action)
	if value.lease != "" {
		headers.Set("X-Runkite-Lease-Ref", value.lease)
	}
}
