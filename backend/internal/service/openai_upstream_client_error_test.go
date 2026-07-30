package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMapOpenAIUpstreamClientError_InvalidRequest400(t *testing.T) {
	body := []byte(`{"error":{"message":"Invalid 'input[18].id': 'item_abc'. Expected an ID that begins with 'rs'.","type":"invalid_request_error","param":"input[18].id","code":"invalid_value"}}`)

	status, errType, msg := MapOpenAIUpstreamClientError(http.StatusBadRequest, body, "")
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "invalid_request_error", errType)
	require.Contains(t, msg, "Expected an ID that begins with 'rs'")
	require.NotEqual(t, "Upstream request failed", msg)
}

func TestMapOpenAIUpstreamClientError_Auth401StaysSanitized(t *testing.T) {
	body := []byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`)
	status, errType, msg := MapOpenAIUpstreamClientError(http.StatusUnauthorized, body, "Incorrect API key provided")
	require.Equal(t, http.StatusBadGateway, status)
	require.Equal(t, "upstream_error", errType)
	require.Contains(t, msg, "authentication failed")
}

func TestMapOpenAIUpstreamClientError_429KeepsMessage(t *testing.T) {
	body := []byte(`{"error":{"message":"Rate limit reached for gpt-5","type":"rate_limit_error"}}`)
	status, errType, msg := MapOpenAIUpstreamClientError(http.StatusTooManyRequests, body, "")
	require.Equal(t, http.StatusTooManyRequests, status)
	require.Equal(t, "rate_limit_error", errType)
	require.Contains(t, msg, "Rate limit reached")
}

func TestMapOpenAIUpstreamClientError_503KeepsStatusAndMessage(t *testing.T) {
	body := []byte(`{"error":{"message":"Service temporarily unavailable.","type":"upstream_error"}}`)
	status, errType, msg := MapOpenAIUpstreamClientError(http.StatusServiceUnavailable, body, "")
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "upstream_error", errType)
	require.Contains(t, msg, "temporarily unavailable")
}

func TestMapOpenAIUpstreamClientError_422Passthrough(t *testing.T) {
	body := []byte(`{"error":{"message":"Invalid schema for field messages","type":"invalid_request_error"}}`)
	status, errType, msg := MapOpenAIUpstreamClientError(http.StatusUnprocessableEntity, body, "")
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_request_error", errType)
	require.Equal(t, "Invalid schema for field messages", msg)
}

func TestIsSafeOpenAIClientFacingError(t *testing.T) {
	safeBody := []byte(`{"error":{"message":"Invalid 'input[18].id': 'item_abc'. Expected an ID that begins with 'rs'.","type":"invalid_request_error","code":"invalid_value"}}`)
	require.True(t, isSafeOpenAIClientFacingError(http.StatusBadRequest, safeBody, ""))

	// Hostname / secret leakage must stay generic under oauth/passthrough sanitization.
	leakyBody := []byte(`{"error":{"message":"secret-upstream.example invalid parameter","type":"invalid_request_error"}}`)
	require.False(t, isSafeOpenAIClientFacingError(http.StatusBadRequest, leakyBody, ""))
	require.False(t, isSafeOpenAIClientFacingError(http.StatusBadRequest, []byte(`{"error":{"message":"key sk-test leaked"}}`), ""))
}

func TestOpenAIHandleErrorResponse_InvalidItemIDReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &OpenAIGatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid 'input[18].id': 'item_4fc82177e65442a982c988f3'. Expected an ID that begins with 'rs'.","type":"invalid_request_error","param":"input[18].id","code":"invalid_value"}}`)
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 99, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "invalid_request_error", errField["type"])
	require.Contains(t, errField["message"], "Expected an ID that begins with 'rs'")
	require.NotContains(t, rec.Body.String(), "Upstream request failed")
}
