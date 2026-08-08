import { describe, expect, it } from 'vitest'
import { buildStepSeries } from './autopilotTrends'
import type { AIHistory } from '@/api/admin/ai'

describe('buildStepSeries', () => {
  it('forwards fills applied weight changes and appends current now', () => {
    const hist: AIHistory = {
      runs: [
        { id: 1, ts: '2026-01-01T00:00:00Z', trigger: 'manual', status: 'ok' },
        { id: 2, ts: '2026-01-01T01:00:00Z', trigger: 'manual', status: 'ok' },
      ],
      actions: [
        {
          id: 10,
          run_id: 1,
          ts: '2026-01-01T00:00:00Z',
          account_id: 5,
          account_name: 'a',
          op: 'set_weight',
          before: '10',
          after: '30',
          reason: 'x',
          confidence: 0.9,
          state: 'applied',
          reject_reason: '',
          outcome: '',
        },
      ],
      accounts: [
        {
          id: 5,
          name: 'a',
          group_ids: [1],
          weight: 40,
          priority: 1,
          schedulable: true,
          ai_disabled: false,
          ai_managed: true,
          status: 'active',
        },
      ],
    }
    const s = buildStepSeries(hist, 'set_weight', [5], (a) => a.weight)
    expect(s.labels).toEqual(['#1', '#2', 'now'])
    // run1 applied 30, run2 no change stays 30, now uses current 40
    expect(s.datasets[0].data).toEqual([30, 30, 40])
  })

  it('ignores non-applied actions', () => {
    const hist: AIHistory = {
      runs: [{ id: 1, ts: 't', trigger: 'm', status: 'ok' }],
      actions: [
        {
          id: 1,
          run_id: 1,
          ts: 't',
          account_id: 1,
          account_name: 'x',
          op: 'set_weight',
          before: '1',
          after: '99',
          reason: '',
          confidence: 1,
          state: 'suggested',
          reject_reason: '',
          outcome: '',
        },
      ],
      accounts: [
        {
          id: 1,
          name: 'x',
          group_ids: [],
          weight: 7,
          priority: 2,
          schedulable: true,
          ai_disabled: false,
          ai_managed: true,
          status: 'active',
        },
      ],
    }
    const s = buildStepSeries(hist, 'set_weight', [1], (a) => a.weight)
    expect(s.datasets[0].data).toEqual([7, 7]) // start current + now
  })
})
