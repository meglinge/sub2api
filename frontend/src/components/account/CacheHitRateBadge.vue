<template>
  <span
    v-if="formatted"
    :class="badgeClass"
    :title="hint"
  >
    {{ formatted }}
  </span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

const props = defineProps<{
  rate?: number | null
  compact?: boolean
}>()

const { t } = useI18n()

const formatted = computed(() => {
  if (props.rate == null || Number.isNaN(props.rate)) return ''
  const digits = props.rate >= 10 || props.rate === 0 ? 0 : 1
  const pct = `${props.rate.toFixed(digits)}%`
  return props.compact ? pct : `${pct} ${t('admin.accounts.stats.cache')}`
})

const hint = computed(() => t('admin.accounts.stats.cacheHitRateHint'))

const badgeClass = computed(() => {
  const rate = props.rate ?? 0
  const tone =
    rate >= 50
      ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
      : rate >= 20
        ? 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300'
        : 'bg-gray-100 text-gray-600 dark:bg-gray-800 dark:text-gray-400'
  return `rounded px-1.5 py-0.5 font-medium ${tone}`
})
</script>
