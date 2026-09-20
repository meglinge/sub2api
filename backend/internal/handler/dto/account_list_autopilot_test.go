package dto

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountListItemPreservesAutopilotControls(t *testing.T) {
	touched := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	for _, weight := range []int{0, 25} {
		account := &service.Account{
			ID: 42, AIDisabled: true, AIManaged: false, AIWatched: true,
			ScheduleWeight: weight, ManualTouchedAt: &touched,
		}
		full := AccountFromServiceShallow(account)
		lite := AccountListItemFromAccount(full)
		fullJSON, err := json.Marshal(full)
		require.NoError(t, err)
		liteJSON, err := json.Marshal(lite)
		require.NoError(t, err)
		var fullFields, liteFields map[string]any
		require.NoError(t, json.Unmarshal(fullJSON, &fullFields))
		require.NoError(t, json.Unmarshal(liteJSON, &liteFields))
		for _, key := range []string{"ai_disabled", "ai_managed", "ai_watched", "schedule_weight", "manual_touched_at"} {
			require.Contains(t, liteFields, key)
			require.Equal(t, fullFields[key], liteFields[key], key)
		}
	}
}
