package service

import (
	"net/http"
	"strings"
)

// upstreamModelNotFoundKeywords are matched against the normalized body for
// HTTP 404 model capability rejections. The broad trailing "not found" token is
// intentional for 404 only (many gateways return plain "not found" when the
// path/model is missing); 400 uses the stricter list below.
var upstreamModelNotFoundKeywords = []string{"model not found", "unknown model", "not found"}

// upstreamModelNotFoundStrongKeywords are high-confidence model-capability
// rejection phrases. Used for HTTP 400 because NewAPI / OpenAI-compatible
// relays often return 400 + code model_not_found (or "unknown provider for
// model X") instead of 404. Bare "not found" is deliberately omitted so
// generic invalid_request 400s are not misclassified as model-not-found.
//
// Note: after normalizeModelNotFoundBody, error.code "model_not_found" becomes
// "model not found" and matches the first phrase.
var upstreamModelNotFoundStrongKeywords = []string{
	"model not found",
	"unknown model",
	"unknown provider for model",
	"model does not exist",
	"no such model",
	"unsupported model",
	"model is not supported",
}

func isUpstreamModelNotFoundError(statusCode int, body []byte) bool {
	normalized := normalizeModelNotFoundBody(body)
	if normalized == "" || !strings.Contains(normalized, "model") {
		return false
	}
	switch statusCode {
	case http.StatusNotFound:
		return containsAnyKeyword(normalized, upstreamModelNotFoundKeywords)
	case http.StatusBadRequest:
		// Deterministic "this account/upstream cannot serve this model"
		// rejections. Callers cool the (account, model) pair and fail over.
		// Codex plan-gated OAuth 400s are handled by a dedicated branch (and
		// must not be classified here for API-key accounts that should ignore
		// that phrase entirely).
		if strings.Contains(normalized, openAICodexPlanGatedModelPhrase) {
			return false
		}
		return containsAnyKeyword(normalized, upstreamModelNotFoundStrongKeywords)
	default:
		return false
	}
}

func isModelNotFoundError(statusCode int, body []byte) bool {
	return isUpstreamModelNotFoundError(statusCode, body) || statusCode == http.StatusNotFound
}

// openAICodexPlanGatedModelPhrase matches the deterministic Codex 400 returned
// when a ChatGPT OAuth account's plan cannot serve the requested model, e.g.
// {"detail":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}
// The phrase is compared against the normalized body (lowercased, "_"/"-"
// folded to spaces), so it also matches the same message embedded in
// error.message-style payloads.
const openAICodexPlanGatedModelPhrase = "model is not supported when using codex"

// isOpenAICodexPlanGatedModelError reports whether the upstream response is the
// deterministic Codex rejection of a plan-gated model on a ChatGPT account.
// Unlike transient failures, retrying the same account cannot succeed until the
// account's plan changes, so callers should treat it like model-not-found and
// cool the (account, model) pair down instead of re-selecting the account.
func isOpenAICodexPlanGatedModelError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	normalized := normalizeModelNotFoundBody(body)
	if normalized == "" {
		return false
	}
	return strings.Contains(normalized, openAICodexPlanGatedModelPhrase)
}

func containsModelNotFoundKeyword(normalizedBody string) bool {
	return containsAnyKeyword(normalizedBody, upstreamModelNotFoundKeywords)
}

func containsAnyKeyword(normalizedBody string, keywords []string) bool {
	if normalizedBody == "" {
		return false
	}
	for _, keyword := range keywords {
		if keyword != "" && strings.Contains(normalizedBody, keyword) {
			return true
		}
	}
	return false
}

func normalizeModelNotFoundBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	normalized := strings.ToLower(string(body))
	normalized = strings.NewReplacer("_", " ", "-", " ", "\n", " ", "\r", " ", "\t", " ").Replace(normalized)
	return strings.Join(strings.Fields(normalized), " ")
}
