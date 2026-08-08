-- AI Autopilot: account control surface + audit tables
-- Account-level soft disable is orthogonal to manual schedulable/status.

ALTER TABLE accounts
  ADD COLUMN IF NOT EXISTS ai_disabled BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS ai_managed BOOLEAN NOT NULL DEFAULT TRUE,
  ADD COLUMN IF NOT EXISTS ai_watched BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS schedule_weight INTEGER NOT NULL DEFAULT 10,
  ADD COLUMN IF NOT EXISTS manual_touched_at TIMESTAMPTZ NULL;

COMMENT ON COLUMN accounts.ai_disabled IS 'AI autopilot soft-disable; orthogonal to status/schedulable';
COMMENT ON COLUMN accounts.ai_managed IS 'When false, autopilot cannot mutate this account';
COMMENT ON COLUMN accounts.ai_watched IS 'Watch flag for high-signal autopilot notifications';
COMMENT ON COLUMN accounts.schedule_weight IS 'OpenAI Top-K / same-priority traffic share weight (>=0)';
COMMENT ON COLUMN accounts.manual_touched_at IS 'Last human edit time; autopilot respects immunity window';

CREATE INDEX IF NOT EXISTS idx_accounts_ai_disabled_active
  ON accounts (id)
  WHERE deleted_at IS NULL AND ai_disabled = TRUE;

CREATE TABLE IF NOT EXISTS ai_runs (
  id              BIGSERIAL PRIMARY KEY,
  ts              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  trigger         TEXT NOT NULL DEFAULT '',
  model           TEXT NOT NULL DEFAULT '',
  window_from     TIMESTAMPTZ NULL,
  window_to       TIMESTAMPTZ NULL,
  status          TEXT NOT NULL DEFAULT 'ok',
  error           TEXT NOT NULL DEFAULT '',
  summary         TEXT NOT NULL DEFAULT '',
  observations    JSONB NOT NULL DEFAULT '[]'::jsonb,
  input_tokens    INTEGER NOT NULL DEFAULT 0,
  output_tokens   INTEGER NOT NULL DEFAULT 0,
  cost_micros     BIGINT NOT NULL DEFAULT 0,
  latency_ms      INTEGER NOT NULL DEFAULT 0,
  turns           INTEGER NOT NULL DEFAULT 1,
  raw_request     TEXT NOT NULL DEFAULT '',
  raw_response    TEXT NOT NULL DEFAULT '',
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ai_runs_ts ON ai_runs (ts DESC);

CREATE TABLE IF NOT EXISTS ai_actions (
  id              BIGSERIAL PRIMARY KEY,
  run_id          BIGINT NOT NULL REFERENCES ai_runs(id) ON DELETE CASCADE,
  ts              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  account_id      BIGINT NOT NULL,
  account_name    TEXT NOT NULL DEFAULT '',
  op              TEXT NOT NULL,
  before_val      TEXT NOT NULL DEFAULT '',
  after_val       TEXT NOT NULL DEFAULT '',
  reason          TEXT NOT NULL DEFAULT '',
  confidence      DOUBLE PRECISION NOT NULL DEFAULT 0,
  state           TEXT NOT NULL,
  reject_reason   TEXT NOT NULL DEFAULT '',
  outcome         TEXT NOT NULL DEFAULT '',
  rolled_back_at  TIMESTAMPTZ NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ai_actions_run ON ai_actions (run_id);
CREATE INDEX IF NOT EXISTS idx_ai_actions_account_ts ON ai_actions (account_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_ai_actions_state_ts ON ai_actions (state, ts DESC);

CREATE TABLE IF NOT EXISTS ai_account_scores (
  id              BIGSERIAL PRIMARY KEY,
  ts              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  run_id          BIGINT NOT NULL DEFAULT 0,
  account_id      BIGINT NOT NULL,
  account_name    TEXT NOT NULL DEFAULT '',
  stability       DOUBLE PRECISION NOT NULL DEFAULT 0,
  latency         DOUBLE PRECISION NOT NULL DEFAULT 0,
  throughput      DOUBLE PRECISION NOT NULL DEFAULT 0,
  cost            DOUBLE PRECISION NOT NULL DEFAULT 0,
  overall         DOUBLE PRECISION NOT NULL DEFAULT 0,
  confidence      DOUBLE PRECISION NOT NULL DEFAULT 0,
  note            TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_ai_account_scores_account_ts ON ai_account_scores (account_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_ai_account_scores_ts ON ai_account_scores (ts DESC);
