<template>
  <div v-if="loading && !stats" class="space-y-0.5">
    <div class="h-3 w-14 animate-pulse rounded bg-gray-200 dark:bg-gray-700" />
    <div class="h-3 w-12 animate-pulse rounded bg-gray-200 dark:bg-gray-700" />
  </div>
  <div v-else-if="stats && stats.samples > 0" class="space-y-0.5 text-xs tabular-nums" :title="hint">
    <template v-if="kind === 'ttfb'">
      <div class="flex items-baseline justify-end gap-1">
        <span :class="ttfbClass(stats.ttfb_p50_ms)">{{ formatMs(stats.ttfb_p50_ms) }}</span>
        <span class="text-[10px] text-gray-400 dark:text-gray-500">p50</span>
      </div>
      <div class="flex items-baseline justify-end gap-1">
        <span :class="ttfbClass(stats.ttfb_p99_ms)">{{ formatMs(stats.ttfb_p99_ms) }}</span>
        <span class="text-[10px] text-gray-400 dark:text-gray-500">p99</span>
      </div>
    </template>
    <template v-else>
      <div class="flex items-baseline justify-end gap-1">
        <span :class="tpsClass(stats.tps_p50)">{{ formatTps(stats.tps_p50) }}</span>
        <span class="text-[10px] text-gray-400 dark:text-gray-500">p50</span>
      </div>
      <div class="flex items-baseline justify-end gap-1">
        <span :class="tpsClass(stats.tps_p1)">{{ formatTps(stats.tps_p1) }}</span>
        <span class="text-[10px] text-gray-400 dark:text-gray-500">p1</span>
      </div>
    </template>
  </div>
  <div v-else class="text-right text-xs text-gray-400">—</div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AccountPerfStats } from '@/api/admin/accounts'

const props = withDefaults(
  defineProps<{
    stats?: AccountPerfStats | null
    loading?: boolean
    kind: 'ttfb' | 'tps'
  }>(),
  { stats: null, loading: false }
)

const { t } = useI18n()

const hint = computed(() => {
  if (!props.stats || props.stats.samples <= 0) return t('admin.accounts.perf.noSamples')
  return t('admin.accounts.perf.sampleHint', { n: props.stats.samples })
})

function formatMs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—'
  if (ms < 1000) return `${Math.round(ms)}ms`
  return `${(ms / 1000).toFixed(ms >= 10000 ? 1 : 2)}s`
}

function formatTps(tps: number): string {
  if (!Number.isFinite(tps) || tps <= 0) return '—'
  if (tps >= 100) return tps.toFixed(0)
  if (tps >= 10) return tps.toFixed(1)
  return tps.toFixed(2)
}

function ttfbClass(ms: number): string {
  if (!ms || ms <= 0) return 'text-gray-400'
  if (ms <= 4000) return 'text-emerald-600 dark:text-emerald-400'
  if (ms <= 8000) return 'text-blue-600 dark:text-blue-400'
  if (ms <= 12000) return 'text-yellow-600 dark:text-yellow-400'
  if (ms <= 20000) return 'text-orange-600 dark:text-orange-400'
  return 'text-red-600 dark:text-red-400'
}

function tpsClass(tps: number): string {
  if (!tps || tps <= 0) return 'text-gray-400'
  if (tps <= 10) return 'text-red-600 dark:text-red-400'
  if (tps <= 20) return 'text-orange-600 dark:text-orange-400'
  if (tps <= 30) return 'text-yellow-600 dark:text-yellow-400'
  if (tps <= 45) return 'text-blue-600 dark:text-blue-400'
  return 'text-emerald-600 dark:text-emerald-400'
}
</script>
