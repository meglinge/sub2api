package service

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fakeCodexTurnStateFernet(cipherLen int) string {
	raw := make([]byte, 1+8+16+cipherLen+32)
	raw[0] = 0x80
	return base64.URLEncoding.EncodeToString(raw)
}

func TestInspectCodexTurnStateHealthPersonal(t *testing.T) {
	healthy := InspectCodexTurnStateHealth(fakeCodexTurnStateFernet(160), "plus")
	require.NotNil(t, healthy)
	require.False(t, healthy.Degraded)
	require.Equal(t, 160, healthy.CipherLen)

	degraded := InspectCodexTurnStateHealth(fakeCodexTurnStateFernet(176), "plus")
	require.NotNil(t, degraded)
	require.True(t, degraded.Degraded)
	require.Equal(t, 176, degraded.CipherLen)
	require.Equal(t, 160, degraded.ExpectedCipherLen)
}

func TestInspectCodexTurnStateHealthTeam(t *testing.T) {
	healthy := InspectCodexTurnStateHealth(fakeCodexTurnStateFernet(192), "team")
	require.NotNil(t, healthy)
	require.False(t, healthy.Degraded)
	require.Equal(t, 192, healthy.ExpectedCipherLen)

	wrong := InspectCodexTurnStateHealth(fakeCodexTurnStateFernet(160), "team")
	require.NotNil(t, wrong)
	require.True(t, wrong.Degraded)
}

func TestVerifyCodexTurnStatePingIntelligence(t *testing.T) {
	require.NoError(t, verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(160), "plus"))
	require.Error(t, verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(176), "plus"))
	require.NoError(t, verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(192), "self_serve_business_prolite"))
}

func TestCodexTurnStateCacheConfigCoversOnlyEnabledModels(t *testing.T) {
	cfg := CodexTurnStateCacheConfig{Models: []string{"gpt-5.4"}, TTLMinutes: 43}.Normalized()
	require.False(t, cfg.EnabledForTraffic())
	require.False(t, cfg.CoversModel("gpt-5.4"))

	cfg.Enabled = true
	cfg = cfg.Normalized()
	require.True(t, cfg.EnabledForTraffic())
	require.True(t, cfg.CoversModel("GPT-5.4"))
	require.False(t, cfg.CoversModel("other"))
}

func TestPrepareCodexTurnStateSkipsAPIKeyAccounts(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	body := []byte(`{"model":"gpt-5.4"}`)
	out, err := svc.prepareCodexTurnState(nil, nil, account, body)
	require.NoError(t, err)
	require.Equal(t, body, out)
	require.False(t, account.UsesOpenAICodexProtocol())
}

func TestInjectStoredCodexTurnStateBody(t *testing.T) {
	account := &Account{ID: 9, Extra: map[string]any{}}
	account.SetCodexTurnState("gpt-5.4", fakeCodexTurnStateFernet(160), time.Now())
	body := []byte(`{"model":"gpt-5.4","client_metadata":{"session_id":"abc"}}`)
	out := injectStoredCodexTurnStateBody(nil, account, body)
	require.Contains(t, string(out), `"x-codex-turn-state"`)
}
