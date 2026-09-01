import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountPerfCell from '../AccountPerfCell.vue'
import type { AccountPerfStats } from '@/api/admin/accounts'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${Object.values(params).join(',')}` : key
    })
  }
})

const stats: AccountPerfStats = {
  account_id: 6550,
  samples: 12,
  ttfb_p50_ms: 3200,
  ttfb_p99_ms: 14000,
  tps_p50: 42.5,
  tps_p1: 8.2
}

describe('AccountPerfCell', () => {
  it('renders TTFB p50/p99', () => {
    const wrapper = mount(AccountPerfCell, {
      props: { kind: 'ttfb', stats }
    })
    expect(wrapper.text()).toContain('3.20s')
    expect(wrapper.text()).toContain('14.0s')
    expect(wrapper.text()).toContain('p50')
    expect(wrapper.text()).toContain('p99')
  })

  it('renders TPS p50/p1', () => {
    const wrapper = mount(AccountPerfCell, {
      props: { kind: 'tps', stats }
    })
    expect(wrapper.text()).toContain('42.5')
    expect(wrapper.text()).toContain('8.20')
    expect(wrapper.text()).toContain('p1')
  })

  it('shows dash when there are no samples', () => {
    const wrapper = mount(AccountPerfCell, {
      props: { kind: 'ttfb', stats: { ...stats, samples: 0 } }
    })
    expect(wrapper.text()).toContain('—')
  })
})
