<template>
  <AppLayout>
    <div class="mx-auto max-w-7xl space-y-6">
      <div class="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">
            {{ t('admin.codexTurnState.title') }}
          </h1>
          <p class="mt-1 max-w-3xl text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.codexTurnState.description') }}
          </p>
        </div>
        <button class="btn btn-secondary" :disabled="loading" @click="reload">
          <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
          {{ t('common.refresh') }}
        </button>
      </div>

      <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <div class="card p-4">
          <div class="text-sm text-gray-500">{{ t('admin.codexTurnState.healthy') }}</div>
          <div class="mt-1 text-2xl font-semibold text-emerald-600">{{ overview?.summary.healthy ?? 0 }}</div>
        </div>
        <div class="card p-4">
          <div class="text-sm text-gray-500">{{ t('admin.codexTurnState.degraded') }}</div>
          <div class="mt-1 text-2xl font-semibold text-red-600">{{ overview?.summary.degraded ?? 0 }}</div>
        </div>
        <div class="card p-4">
          <div class="text-sm text-gray-500">{{ t('admin.codexTurnState.cooling') }}</div>
          <div class="mt-1 text-2xl font-semibold text-orange-600">{{ overview?.summary.cooling_down ?? 0 }}</div>
        </div>
        <div class="card p-4">
          <div class="text-sm text-gray-500">{{ t('admin.codexTurnState.chronic') }}</div>
          <div class="mt-1 text-2xl font-semibold text-amber-600">{{ overview?.summary.chronic_failures ?? 0 }}</div>
        </div>
      </div>

      <div class="card space-y-4 p-6">
        <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.codexTurnState.settings') }}</h2>
        <p class="text-sm text-amber-700 dark:text-amber-300">{{ t('admin.codexTurnState.lastHopWarning') }}</p>
        <label class="flex items-center gap-2 text-sm">
          <input v-model="config.enabled" type="checkbox" @change="saveConfig" />
          {{ t('admin.codexTurnState.enabled') }}
        </label>
        <label class="block text-sm">
          {{ t('admin.codexTurnState.models') }}
          <input v-model="modelsText" class="input mt-1" @blur="saveModels" />
        </label>
        <div class="grid gap-4 md:grid-cols-3">
          <label class="block text-sm">
            {{ t('admin.codexTurnState.ttl') }}
            <input v-model.number="config.ttl_minutes" type="number" min="1" class="input mt-1" @change="saveConfig" />
          </label>
          <label class="block text-sm">
            {{ t('admin.codexTurnState.maxTries') }}
            <input v-model.number="config.max_ping_tries" type="number" min="1" class="input mt-1" @change="saveConfig" />
          </label>
          <label class="block text-sm">
            {{ t('admin.codexTurnState.cooldown') }}
            <input v-model.number="config.failure_cooldown_seconds" type="number" min="5" class="input mt-1" @change="saveConfig" />
          </label>
        </div>
        <label class="block text-sm">
          {{ t('admin.codexTurnState.ipv6') }}
          <input v-model="config.ipv6_proxy_url" class="input mt-1" @blur="saveConfig" />
        </label>
        <label class="block text-sm">
          {{ t('admin.codexTurnState.countries') }}
          <input v-model="countriesText" class="input mt-1" @blur="saveCountries" />
        </label>
        <label class="block text-sm">
          {{ t('admin.codexTurnState.refreshMode') }}
          <select v-model="config.refresh_mode" class="input mt-1" @change="saveConfig">
            <option value="blocking">blocking</option>
            <option value="async">async</option>
          </select>
        </label>
      </div>

      <div v-if="errorMessage" class="rounded-lg bg-red-50 p-4 text-sm text-red-600 dark:bg-red-900/20 dark:text-red-400">
        {{ errorMessage }}
      </div>

      <div class="card overflow-x-auto">
        <table class="min-w-full text-sm">
          <thead class="bg-gray-50 text-left dark:bg-dark-800">
            <tr>
              <th class="px-4 py-3">{{ t('admin.codexTurnState.account') }}</th>
              <th class="px-4 py-3">{{ t('admin.codexTurnState.model') }}</th>
              <th class="px-4 py-3">{{ t('admin.codexTurnState.status') }}</th>
              <th class="px-4 py-3">{{ t('admin.codexTurnState.length') }}</th>
              <th class="px-4 py-3">{{ t('admin.codexTurnState.ttlLeft') }}</th>
              <th class="px-4 py-3"></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in rows" :key="`${row.accountId}:${row.model}`" class="border-t border-gray-100 dark:border-dark-700">
              <td class="px-4 py-3">
                <div class="font-medium">{{ row.name }}</div>
                <div class="text-xs text-gray-500">{{ row.planType || '-' }}</div>
              </td>
              <td class="px-4 py-3 font-mono">{{ row.model }}</td>
              <td class="px-4 py-3">
                <span class="rounded-full px-2 py-0.5 text-xs" :class="statusClass(row.status)">{{ row.status }}</span>
              </td>
              <td class="px-4 py-3">
                {{ row.value_length || '-' }}
                <span v-if="row.health" class="text-xs text-gray-500">
                  ({{ row.health.cipher_len }}/{{ row.health.expected_cipher_len }})
                </span>
              </td>
              <td class="px-4 py-3">{{ row.remaining_seconds }}s</td>
              <td class="px-4 py-3 space-x-2 whitespace-nowrap">
                <button class="btn btn-secondary btn-sm" :disabled="pending === row.key" @click="refreshCell(row)">
                  {{ t('admin.codexTurnState.refresh') }}
                </button>
                <button class="btn btn-secondary btn-sm" :disabled="pending === row.key" @click="clearCell(row)">
                  {{ t('admin.codexTurnState.clearCooldown') }}
                </button>
                <button class="btn btn-secondary btn-sm" :disabled="pending === row.key" @click="invalidateCell(row)">
                  {{ t('admin.codexTurnState.invalidate') }}
                </button>
              </td>
            </tr>
            <tr v-if="!rows.length">
              <td colspan="6" class="px-4 py-8 text-center text-gray-500">
                {{ t('admin.codexTurnState.empty') }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import { codexTurnStateAPI, type CodexTurnStateCacheConfig, type CodexTurnStateCell, type CodexTurnStateCellStatus, type CodexTurnStateOverview } from '@/api/admin/codexTurnState'

const { t } = useI18n()
const loading = ref(false)
const errorMessage = ref('')
const overview = ref<CodexTurnStateOverview | null>(null)
const pending = ref('')
const config = reactive<CodexTurnStateCacheConfig>({
  enabled: false,
  ipv6_proxy_url: '',
  models: [],
  ttl_minutes: 43,
  countries: [],
  max_ping_tries: 8,
  refresh_mode: 'blocking',
  failure_cooldown_seconds: 60,
})
const modelsText = ref('')
const countriesText = ref('')
let timer: number | undefined

type Row = CodexTurnStateCell & { accountId: number; name: string; planType: string; key: string }

const rows = computed<Row[]>(() => {
  const out: Row[] = []
  for (const account of overview.value?.accounts ?? []) {
    for (const cell of account.cells ?? []) {
      out.push({
        ...cell,
        accountId: account.account_id,
        name: account.name,
        planType: account.plan_type,
        key: `${account.account_id}:${cell.model}`,
      })
    }
  }
  return out
})

function statusClass(status: CodexTurnStateCellStatus) {
  switch (status) {
    case 'healthy':
      return 'bg-emerald-500/10 text-emerald-600'
    case 'stale':
      return 'bg-amber-500/10 text-amber-600'
    case 'degraded':
    case 'unparsed':
      return 'bg-red-500/10 text-red-600'
    case 'cooling':
      return 'bg-orange-500/10 text-orange-600'
    default:
      return 'bg-gray-100 text-gray-500'
  }
}

function applyConfig(next: CodexTurnStateCacheConfig) {
  Object.assign(config, next)
  modelsText.value = (next.models ?? []).join(', ')
  countriesText.value = (next.countries ?? []).join(', ')
}

async function reload() {
  loading.value = true
  errorMessage.value = ''
  try {
    const [cfg, data] = await Promise.all([codexTurnStateAPI.getConfig(), codexTurnStateAPI.getOverview()])
    applyConfig(cfg)
    overview.value = data
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : String(err)
  } finally {
    loading.value = false
  }
}

async function saveConfig() {
  try {
    const saved = await codexTurnStateAPI.updateConfig({ ...config })
    applyConfig(saved)
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : String(err)
  }
}

async function saveModels() {
  config.models = modelsText.value.split(/[,\s]+/).map((item) => item.trim()).filter(Boolean)
  await saveConfig()
}

async function saveCountries() {
  config.countries = countriesText.value.split(/[,\s]+/).map((item) => item.trim()).filter(Boolean)
  await saveConfig()
}

async function refreshCell(row: Row) {
  pending.value = row.key
  try {
    await codexTurnStateAPI.refresh(row.accountId, row.model)
    await reload()
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : String(err)
  } finally {
    pending.value = ''
  }
}

async function clearCell(row: Row) {
  pending.value = row.key
  try {
    await codexTurnStateAPI.clearCooldown(row.accountId, row.model)
    await reload()
  } finally {
    pending.value = ''
  }
}

async function invalidateCell(row: Row) {
  pending.value = row.key
  try {
    await codexTurnStateAPI.invalidate(row.accountId, row.model)
    await reload()
  } finally {
    pending.value = ''
  }
}

onMounted(() => {
  void reload()
  timer = window.setInterval(() => {
    void reload()
  }, 20000)
})

onUnmounted(() => {
  if (timer) window.clearInterval(timer)
})
</script>
