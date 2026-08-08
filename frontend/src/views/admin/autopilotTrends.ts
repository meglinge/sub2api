import type { AIAction, AIHistory, AIHistoryAccount } from '@/api/admin/ai'

function numeric(v: string): number | null {
  const n = Number(String(v).trim())
  return Number.isFinite(n) ? n : null
}

export interface StepSeries {
  labels: string[]
  datasets: Array<{
    label: string
    data: number[]
    borderColor: string
    backgroundColor: string
    tension: number
    stepped: boolean
    pointRadius: number
  }>
}

const COLORS = ['#3b82f6', '#10b981', '#f59e0b', '#ef4444', '#8b5cf6', '#06b6d4', '#f97316', '#14b8a6']

/** Build step series for weight/priority from applied actions + current values. */
export function buildStepSeries(
  hist: AIHistory,
  op: 'set_weight' | 'set_priority',
  selected: number[],
  currentOf: (a: AIHistoryAccount) => number,
): StepSeries {
  const byId = new Map(hist.accounts.map((a) => [a.id, a]))
  const ids = selected.filter((id) => byId.has(id))
  if (!ids.length || !hist.runs.length) return { labels: [], datasets: [] }

  const perRun = new Map<number, Map<number, number>>()
  const firstBefore = new Map<number, number>()
  for (const a of hist.actions as AIAction[]) {
    if (a.op !== op || a.state !== 'applied') continue
    const after = numeric(a.after)
    if (after === null) continue
    if (!firstBefore.has(a.account_id)) {
      const before = numeric(a.before)
      if (before !== null) firstBefore.set(a.account_id, before)
    }
    let row = perRun.get(a.run_id)
    if (!row) {
      row = new Map()
      perRun.set(a.run_id, row)
    }
    row.set(a.account_id, after)
  }

  const current = new Map<number, number>()
  for (const id of ids) {
    const acc = byId.get(id)!
    current.set(id, firstBefore.get(id) ?? currentOf(acc))
  }

  const labels: string[] = []
  const seriesData = new Map<number, number[]>()
  for (const id of ids) seriesData.set(id, [])

  for (const run of hist.runs) {
    labels.push(`#${run.id}`)
    const changes = perRun.get(run.id)
    for (const id of ids) {
      if (changes?.has(id)) current.set(id, changes.get(id)!)
      seriesData.get(id)!.push(current.get(id) ?? 0)
    }
  }
  labels.push('now')
  for (const id of ids) {
    const acc = byId.get(id)!
    seriesData.get(id)!.push(currentOf(acc))
  }

  const datasets = ids.map((id, i) => ({
    label: byId.get(id)?.name || `#${id}`,
    data: seriesData.get(id) || [],
    borderColor: COLORS[i % COLORS.length],
    backgroundColor: COLORS[i % COLORS.length],
    tension: 0,
    stepped: true as const,
    pointRadius: 2,
  }))
  return { labels, datasets }
}
