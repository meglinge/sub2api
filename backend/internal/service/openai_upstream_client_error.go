package service

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// MapOpenAIUpstreamClientError maps an upstream OpenAI HTTP error into the
// status / type / message returned to the client.
//
// Design goals:
//   - Client request errors (400/404/413/422/...) keep the real status code and
//     sanitized upstream message so gateways (e.g. claude-code-hub) stop seeing
//     everything as opaque 502 "Upstream request failed".
//   - Account-credential failures (401, and generic 403) stay sanitized as 502
//     so pool-account auth issues are not presented as the caller's own key
//     problem. Content-policy style 403s still surface the real message.
//   - 429/503 preserve status; message prefers upstream when non-empty.
//   - Other 5xx collapse to 502 but keep the upstream message when present.
func MapOpenAIUpstreamClientError(statusCode int, body []byte, upstreamMsg string) (status int, errType, message string) {
	msg := strings.TrimSpace(upstreamMsg)
	if msg == "" {
		msg = strings.TrimSpace(extractUpstreamErrorMessage(body))
	}
	msg = sanitizeUpstreamErrorMessage(msg)

	bodyType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.type").String()))

	switch statusCode {
	case http.StatusUnauthorized: // 401 — pool account auth, do not leak as client 401
		return http.StatusBadGateway, "upstream_error", "Upstream authentication failed, please contact administrator"

	case http.StatusForbidden: // 403
		// Content policy / client-facing rejections should still pass the real message.
		if bodyType == "invalid_request_error" || isClientFacingForbiddenMessage(msg) {
			if msg == "" {
				msg = "Upstream access forbidden"
			}
			errType = bodyType
			if errType == "" {
				errType = "invalid_request_error"
			}
			return http.StatusForbidden, errType, msg
		}
		return http.StatusBadGateway, "upstream_error", "Upstream access forbidden, please contact administrator"

	case http.StatusPaymentRequired: // 402
		if msg == "" {
			msg = "Upstream payment required: insufficient balance or billing issue"
		}
		return http.StatusPaymentRequired, "upstream_error", msg

	case http.StatusTooManyRequests: // 429
		if msg == "" {
			msg = "Upstream rate limit exceeded, please retry later"
		}
		return http.StatusTooManyRequests, "rate_limit_error", msg

	case 529:
		if msg == "" {
			msg = "Upstream service overloaded, please retry later"
		}
		return http.StatusServiceUnavailable, "upstream_error", msg

	case http.StatusBadRequest: // 400
		if msg == "" {
			msg = "Upstream request failed"
		}
		errType = bodyType
		if errType == "" {
			errType = "invalid_request_error"
		}
		return http.StatusBadRequest, errType, msg

	case http.StatusNotFound: // 404
		if msg == "" {
			msg = "Upstream resource not found"
		}
		errType = bodyType
		if errType == "" {
			errType = "not_found_error"
		}
		return http.StatusNotFound, errType, msg

	case http.StatusRequestEntityTooLarge: // 413
		if msg == "" {
			msg = OpenAIRequestBodyTooLargeClientMessage
		}
		errType = bodyType
		if errType == "" {
			errType = "invalid_request_error"
		}
		return http.StatusRequestEntityTooLarge, errType, msg

	case http.StatusUnprocessableEntity: // 422
		if msg == "" {
			msg = "Upstream request failed"
		}
		errType = bodyType
		if errType == "" {
			errType = "invalid_request_error"
		}
		return http.StatusUnprocessableEntity, errType, msg

	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if msg == "" {
			msg = "Upstream service temporarily unavailable"
		}
		// Preserve 503 so clients/load balancers can treat overload distinctly.
		if statusCode == http.StatusServiceUnavailable {
			return http.StatusServiceUnavailable, "upstream_error", msg
		}
		return http.StatusBadGateway, "upstream_error", msg

	default:
		if statusCode >= 400 && statusCode < 500 {
			if msg == "" {
				msg = "Upstream request failed"
			}
			errType = bodyType
			if errType == "" {
				errType = "upstream_error"
			}
			return statusCode, errType, msg
		}
		if msg == "" {
			msg = "Upstream request failed"
		}
		return http.StatusBadGateway, "upstream_error", msg
	}
}

func isClientFacingForbiddenMessage(msg string) bool {
	lower := strings.ToLower(strings.TrimSpace(msg))
	if lower == "" {
		return false
	}
	// Policy / safety / content rejections are useful to the end user.
	for _, needle := range []string{
		"content policy",
		"content_policy",
		"safety",
		"moderation",
		"flagged",
		"violat",
		"not allowed",
		"blocked",
		"cyber",
		"usage policy",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// isSafeOpenAIClientFacingError reports whether an upstream error is safe to
// surface to clients even under OAuth/passthrough sanitization.
//
// Allow-list only: OpenAI-shaped invalid_request / rate_limit messages without
// hostnames, URLs, or key-like material. Everything else stays generic.
func isSafeOpenAIClientFacingError(statusCode int, body []byte, upstreamMsg string) bool {
	msg := strings.TrimSpace(upstreamMsg)
	if msg == "" {
		msg = strings.TrimSpace(extractUpstreamErrorMessage(body))
	}
	msg = sanitizeUpstreamErrorMessage(msg)
	if msg == "" {
		return false
	}

	// Never pass through messages that look like they contain infrastructure leakage.
	lower := strings.ToLower(msg)
	for _, needle := range []string{
		"http://", "https://", "www.", ".example", ".internal",
		"sk-", "rk-", "pk-", "bearer ", "api_key", "api-key",
		"refresh_token", "access_token", "secret",
	} {
		if strings.Contains(lower, needle) {
			return false
		}
	}
	// Host-like tokens (foo.bar.baz) — conservative: require a common TLD-ish suffix.
	if strings.Contains(msg, ".") {
		for _, tld := range []string{".com", ".net", ".org", ".io", ".ai", ".dev", ".cloud", ".cn"} {
			if strings.Contains(lower, tld) {
				return false
			}
		}
	}

	bodyType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.type").String()))
	bodyCode := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))

	switch statusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusNotFound, http.StatusRequestEntityTooLarge:
		if bodyType == "invalid_request_error" || bodyType == "invalid_request" {
			return true
		}
		if bodyCode == "invalid_value" || bodyCode == "invalid_id" || bodyCode == "invalid_id_prefix" {
			return true
		}
		// Common OpenAI Responses client errors even when type is missing.
		for _, needle := range []string{
			"expected an id that begins with",
			"invalid '",
			"missing required parameter",
			"extra inputs are not permitted",
			"context window",
			"context length",
			"context_too_large",
		} {
			if strings.Contains(lower, needle) {
				return true
			}
		}
		return false
	case http.StatusTooManyRequests:
		return true
	case http.StatusServiceUnavailable, 529:
		return true
	default:
		return false
	}
}
