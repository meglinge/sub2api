-- Outcome backfill + demotion TTL decay (UpstreamRouter parity).
ALTER TABLE ai_actions
  ADD COLUMN IF NOT EXISTS baseline_json TEXT NOT NULL DEFAULT '{}',
  ADD COLUMN IF NOT EXISTS outcome_at TIMESTAMPTZ NULL,
  ADD COLUMN IF NOT EXISTS decayed_at TIMESTAMPTZ NULL;

CREATE INDEX IF NOT EXISTS idx_ai_actions_pending_outcome
  ON ai_actions (ts ASC)
  WHERE state = 'applied' AND (outcome = '' OR outcome IS NULL);

CREATE INDEX IF NOT EXISTS idx_ai_actions_pending_decay
  ON ai_actions (ts ASC)
  WHERE state = 'applied' AND decayed_at IS NULL AND op IN ('set_priority', 'set_weight', 'disable');
