import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import CacheHitRateBadge from '../CacheHitRateBadge.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => (key.endsWith('.cache') ? '缓存' : key)
    })
  }
})

describe('CacheHitRateBadge', () => {
  it('hides when rate is missing', () => {
    const wrapper = mount(CacheHitRateBadge, {
      props: { rate: null }
    })
    expect(wrapper.text()).toBe('')
  })

  it('renders percentage with cache label', () => {
    const wrapper = mount(CacheHitRateBadge, {
      props: { rate: 42.4 }
    })
    expect(wrapper.text()).toContain('42%')
    expect(wrapper.text()).toContain('缓存')
  })
})
