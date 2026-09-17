import { apiClient } from '../client'

export type CodexTurnStateCellStatus =
  | 'healthy'
  | 'stale'
  | 'degraded'
  | 'cooling'
  | 'missing'
  | 'unparsed'

export interface CodexTurnStateCacheConfig {
  enabled: boolean
  ipv6_proxy_url: string
  models: string[]
  ttl_minutes: number
  countries: string[]
  max_ping_tries: number
  refresh_mode: 'blocking' | 'async'
  failure_cooldown_seconds: number
}

export interface CodexTurnStateHealth {
  cipher_len: number
  expected_cipher_len: number
  degraded: boolean
  error?: string
}

export interface CodexTurnStateCell {
  model: string
  auto_cached: boolean
  has_value: boolean
  value_length: number
  health?: CodexTurnStateHealth
  captured_at?: string
  expires_at?: string
  remaining_seconds: number
  expired: boolean
  cooldown_reason?: string
  cooldown_reset_at?: string
  cooldown_remaining_seconds: number
  cooldown_backoff_level: number
  refresh_consecutive_fails: number
  refresh_total_attempts: number
  refresh_total_successes: number
  refresh_last_ping_count: number
  refresh_last_duration_ms: number
  refresh_last_attempt_at?: string
  refresh_last_success_at?: string
  refresh_failure_kind?: string
  refresh_failure_detail?: string
  status: CodexTurnStateCellStatus
}

export interface CodexTurnStateAccountOverview {
  account_id: number
  name: string
  plan_type: string
  cells: CodexTurnStateCell[]
}

export interface CodexTurnStateOverview {
  generated_at: string
  config: CodexTurnStateCacheConfig
  summary: {
    accounts: number
    cells: number
    healthy: number
    degraded: number
    expired: number
    missing: number
    cooling_down: number
    chronic_failures: number
  }
  accounts: CodexTurnStateAccountOverview[]
}

export interface CodexTurnStateRefreshEvent {
  seq: number
  at: string
  account_id: number
  model: string
  ok: boolean
  ping_count: number
  duration_ms: number
  failure_kind?: string
  detail?: string
  cipher_len: number
  expected_cipher_len: number
}

const base = '/admin/codex-turn-states'

export const codexTurnStateAPI = {
  getConfig: async () => {
    const { data } = await apiClient.get<CodexTurnStateCacheConfig>(`${base}/config`)
    return data
  },
  updateConfig: async (payload: Partial<CodexTurnStateCacheConfig>) => {
    const { data } = await apiClient.put<CodexTurnStateCacheConfig>(`${base}/config`, payload)
    return data
  },
  getOverview: async () => {
    const { data } = await apiClient.get<CodexTurnStateOverview>(`${base}/overview`)
    return data
  },
  getEvents: async () => {
    const { data } = await apiClient.get<{ events: CodexTurnStateRefreshEvent[] }>(`${base}/events`)
    return data.events ?? []
  },
  refresh: async (account_id: number, model: string) => {
    const { data } = await apiClient.post(`${base}/refresh`, { account_id, model })
    return data
  },
  clearCooldown: async (account_id: number, model: string) => {
    const { data } = await apiClient.post(`${base}/clear-cooldown`, { account_id, model })
    return data
  },
  invalidate: async (account_id: number, model: string) => {
    const { data } = await apiClient.post(`${base}/invalidate`, { account_id, model })
    return data
  },
}

export default codexTurnStateAPI
