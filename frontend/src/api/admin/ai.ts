import { apiClient } from '../client'

export interface AIPilotStatus {
  enabled: boolean
  scheduled: boolean
  running: boolean
  started_at: number
  trigger: string
  phase: string
  turn: number
  max_turns: number
  interval_minutes: number
  apply_mode: string
}

export interface AIRun {
  id: number
  ts: string
  trigger: string
  model: string
  status: string
  error: string
  summary: string
  observations: string
  input_tokens: number
  output_tokens: number
  latency_ms: number
  turns: number
  suggested_count: number
  actions?: AIAction[]
}

export interface AIAction {
  id: number
  run_id: number
  ts: string
  account_id: number
  account_name: string
  op: string
  before: string
  after: string
  reason: string
  confidence: number
  state: string
  reject_reason: string
  outcome: string
}

export interface AIHistoryRun {
  id: number
  ts: string
  trigger: string
  status: string
}

export interface AIHistoryAccount {
  id: number
  name: string
  group_ids: number[]
  weight: number
  priority: number
  schedulable: boolean
  ai_disabled: boolean
  ai_managed: boolean
  status: string
}

export interface AIHistory {
  runs: AIHistoryRun[]
  actions: AIAction[]
  accounts: AIHistoryAccount[]
}

export interface AIAccountScore {
  id: number
  ts: string
  run_id: number
  account_id: number
  account_name: string
  stability: number
  latency: number
  throughput: number
  cost: number
  overall: number
  confidence: number
  note: string
}

export interface AIScoreRow {
  account_id: number
  name: string
  group_ids: number[]
  weight: number
  priority: number
  schedulable: boolean
  ai_disabled: boolean
  ai_managed: boolean
  status: string
  rate_limited: boolean
  scored: boolean
  score: AIAccountScore
}

export interface BulkSuggestionResult {
  done: number
  failed: number
  errors?: string[]
}

/** Mirrors backend AIAutopilotSettings / UpstreamRouter AI knobs. */
export interface AIAutopilotSettings {
  enabled: boolean
  source: string
  base_url: string
  api_key: string
  model: string
  self_group?: string
  apply_mode: string
  interval_minutes: number
  timeout_seconds: number
  window_minutes: number
  recent_window_minutes: number
  recent_sample_limit: number
  trend_bucket_minutes: number
  memory_runs: number
  min_available_per_group: number
  max_actions_per_run: number
  channel_cooldown_minutes: number
  manual_immunity_hours: number
  confidence_threshold: number
  weight_max_delta_percent: number
  weight_max_delta_abs: number
  priority_max_delta: number
  /** Overall score dimension weights as percentages (default 40/30/20/10). Sum renormalized if ≠100. */
  score_weight_stability?: number
  score_weight_latency?: number
  score_weight_throughput?: number
  score_weight_cost?: number
  max_input_tokens?: number
  daily_run_budget?: number
  managed_group_ids: number[]
  activation_probe_enabled?: boolean
  activation_probe_timeout_seconds?: number
  activation_probe_max_ttfb_ms?: number
  activation_probe_max_per_run?: number
  activation_probe_fresh_minutes?: number
  activation_probe_prompt?: string
  activation_probe_juice?: boolean
  /** Comma-separated models tried first on enable probes. Default gpt-5.6-sol. */
  activation_probe_models?: string
  max_probe_turns?: number
  op_disable?: boolean
  op_enable?: boolean
  op_set_priority?: boolean
  op_set_weight?: boolean
  op_set_rpm_limit?: boolean
  op_set_max_concurrency?: boolean
  op_release?: boolean
  op_unlock?: boolean
  /** Switch new-api token group / sub2api panel key group (requires per-account credentials). */
  op_switch_upstream_group?: boolean
}

export const AI_OP_SWITCHES = [
  { key: 'op_set_weight' as const, op: 'set_weight', label: '调权重', hint: '改同优先级层内的分流比例。' },
  { key: 'op_set_priority' as const, op: 'set_priority', label: '调优先级', hint: '改选路层级，越小越优先。' },
  { key: 'op_disable' as const, op: 'disable', label: 'AI 停用账号', hint: '写 ai_disabled，受每组最少可用护栏约束。' },
  { key: 'op_enable' as const, op: 'enable', label: '解除 AI 停用', hint: '只清 AI 停用标记，不改人工 schedulable。' },
  { key: 'op_set_rpm_limit' as const, op: 'set_rpm_limit', label: '调 RPM 上限', hint: '0 = 不限。' },
  { key: 'op_set_max_concurrency' as const, op: 'set_max_concurrency', label: '调并发上限', hint: '0 = 不限。' },
  { key: 'op_release' as const, op: 'release', label: '解除限流/临时不可调度', hint: '恢复被系统临时摘下的账号。' },
  { key: 'op_unlock' as const, op: 'unlock', label: '解除锁死', hint: '解锁死状态（若有）。' },
  {
    key: 'op_switch_upstream_group' as const,
    op: 'switch_upstream_group',
    label: '切换上游分组',
    hint: '改 new-api token 组或 sub2api 面板 key 分组以调进货倍率。需账号勾选开关并填面板凭证；默认关。'
  },
]

export async function getAIStatus(): Promise<AIPilotStatus> {
  const { data } = await apiClient.get<AIPilotStatus>('/admin/ai/status')
  return data
}

export async function listAIRuns(params?: { limit?: number; before?: number }): Promise<AIRun[]> {
  const { data } = await apiClient.get<AIRun[]>('/admin/ai/runs', { params })
  return data
}

export async function getAIRun(id: number): Promise<AIRun> {
  const { data } = await apiClient.get<AIRun>(`/admin/ai/runs/${id}`)
  return data
}

export async function analyzeNow(): Promise<AIRun> {
  const { data } = await apiClient.post<AIRun>('/admin/ai/analyze', undefined, {
    timeout: 15 * 60 * 1000,
  })
  return data
}

export async function listAISuggestions(limit = 50): Promise<AIAction[]> {
  const { data } = await apiClient.get<AIAction[]>('/admin/ai/suggestions', { params: { limit } })
  return data
}

export async function getAIHistory(runs = 100): Promise<AIHistory> {
  const { data } = await apiClient.get<AIHistory>('/admin/ai/history', { params: { runs } })
  return data
}

export async function listAIScores(): Promise<AIScoreRow[]> {
  const { data } = await apiClient.get<{ items: AIScoreRow[] }>('/admin/ai/scores')
  return data?.items || []
}

export async function getAIScoreHistory(accountId: number, limit = 50): Promise<AIAccountScore[]> {
  const { data } = await apiClient.get<{ items: AIAccountScore[] }>(`/admin/ai/scores/${accountId}/history`, {
    params: { limit },
  })
  return data?.items || []
}

export async function approveRunSuggestions(runId: number): Promise<BulkSuggestionResult> {
  const { data } = await apiClient.post<BulkSuggestionResult>(`/admin/ai/runs/${runId}/approve-all`)
  return data
}

export async function dismissRunSuggestions(runId: number): Promise<BulkSuggestionResult> {
  const { data } = await apiClient.post<BulkSuggestionResult>(`/admin/ai/runs/${runId}/dismiss-all`)
  return data
}

export async function rollbackAIAction(id: number): Promise<void> {
  await apiClient.post(`/admin/ai/actions/${id}/rollback`)
}

export async function approveAIAction(id: number): Promise<void> {
  await apiClient.post(`/admin/ai/actions/${id}/approve`)
}

export async function dismissAIAction(id: number): Promise<void> {
  await apiClient.post(`/admin/ai/actions/${id}/dismiss`)
}

export async function getAISettings(): Promise<AIAutopilotSettings> {
  const { data } = await apiClient.get<AIAutopilotSettings>('/admin/ai/settings')
  return data
}

export async function updateAISettings(body: Partial<AIAutopilotSettings>): Promise<AIAutopilotSettings> {
  const { data } = await apiClient.put<AIAutopilotSettings>('/admin/ai/settings', body)
  return data
}
