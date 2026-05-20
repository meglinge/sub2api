package httputil

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsCloudflareChallengeResponse(t *testing.T) {
	t.Run("cf mitigated header", func(t *testing.T) {
		headers := http.Header{"Cf-Mitigated": []string{"challenge"}}

		require.True(t, IsCloudflareChallengeResponse(http.StatusForbidden, headers, nil))
	})

	t.Run("html challenge body", func(t *testing.T) {
		headers := http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}
		body := []byte(`<html><head><title>Just a moment...</title></head><script>window._cf_chl_opt={}</script></html>`)

		require.True(t, IsCloudflareChallengeResponse(http.StatusForbidden, headers, body))
	})

	t.Run("429 challenge body", func(t *testing.T) {
		body := []byte(`Enable JavaScript and cookies to continue`)

		require.True(t, IsCloudflareChallengeResponse(http.StatusTooManyRequests, nil, body))
	})

	t.Run("plain json forbidden", func(t *testing.T) {
		headers := http.Header{"Content-Type": []string{"application/json"}}
		body := []byte(`{"error":{"message":"workspace access denied"}}`)

		require.False(t, IsCloudflareChallengeResponse(http.StatusForbidden, headers, body))
	})

	t.Run("unsupported status", func(t *testing.T) {
		headers := http.Header{"Cf-Mitigated": []string{"challenge"}}

		require.False(t, IsCloudflareChallengeResponse(http.StatusInternalServerError, headers, nil))
	})
}

func TestExtractCloudflareRayID(t *testing.T) {
	t.Run("header", func(t *testing.T) {
		headers := http.Header{"Cf-Ray": []string{"9fe90c66c85072a4-EWR"}}

		require.Equal(t, "9fe90c66c85072a4-EWR", ExtractCloudflareRayID(headers, nil))
	})

	t.Run("body", func(t *testing.T) {
		body := []byte(`<span>cf-ray: 9fe90c66c85072a4-EWR</span>`)

		require.Equal(t, "9fe90c66c85072a4-EWR", ExtractCloudflareRayID(nil, body))
	})
}
