<template>
  <AppLayout>
    <div class="space-y-6">
      <!-- Status Card -->
      <div class="card">
        <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
          <div class="flex items-center justify-between">
            <div>
              <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
                {{ t('admin.pricing.status.title') }}
              </h2>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
                {{ t('admin.pricing.status.description') }}
              </p>
            </div>
            <div class="flex gap-2">
              <button
                @click="forceUpdate"
                :disabled="updating"
                class="btn btn-primary btn-sm"
              >
                <svg v-if="updating" class="mr-1 h-4 w-4 animate-spin" fill="none" viewBox="0 0 24 24">
                  <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
                  <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
                </svg>
                {{ updating ? t('admin.pricing.updating') : t('admin.pricing.forceUpdate') }}
              </button>
              <label
                class="btn btn-sm cursor-pointer"
                :class="uploading ? 'opacity-50 pointer-events-none' : ''"
              >
                <svg v-if="uploading" class="mr-1 h-4 w-4 animate-spin" fill="none" viewBox="0 0 24 24">
                  <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
                  <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
                </svg>
                <svg v-else class="mr-1 h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.5">
                  <path stroke-linecap="round" stroke-linejoin="round" d="M3 16.5v2.25A2.25 2.25 0 005.25 21h13.5A2.25 2.25 0 0021 18.75V16.5m-13.5-9L12 3m0 0l4.5 4.5M12 3v13.5" />
                </svg>
                {{ uploading ? t('admin.pricing.uploading') : t('admin.pricing.uploadFile') }}
                <input type="file" accept=".json" class="hidden" @change="handleFileUpload" :disabled="uploading" />
              </label>
            </div>
          </div>
        </div>
        <div v-if="uploadMessage" class="mx-6 mb-4 rounded-lg border p-3 text-sm" :class="uploadError ? 'border-red-200 bg-red-50 text-red-600 dark:border-red-800 dark:bg-red-900/20 dark:text-red-400' : 'border-green-200 bg-green-50 text-green-600 dark:border-green-800 dark:bg-green-900/20 dark:text-green-400'">
          {{ uploadMessage }}
        </div>
        <div class="grid grid-cols-2 gap-4 p-6 md:grid-cols-4" v-if="status">
          <div>
            <div class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.pricing.status.modelCount') }}</div>
            <div class="mt-1 text-lg font-semibold text-gray-900 dark:text-white">{{ status.status.model_count }}</div>
          </div>
          <div>
            <div class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.pricing.status.lastUpdated') }}</div>
            <div class="mt-1 text-sm font-medium text-gray-900 dark:text-white">{{ formatDate(status.status.last_updated) }}</div>
          </div>
          <div>
            <div class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.pricing.status.updateInterval') }}</div>
            <div class="mt-1 text-sm font-medium text-gray-900 dark:text-white">{{ status.config.update_interval_hours }}h / {{ status.config.hash_check_interval_minutes }}min</div>
          </div>
          <div>
            <div class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.pricing.status.hash') }}</div>
            <div class="mt-1 truncate text-sm font-mono text-gray-900 dark:text-white" :title="status.status.local_hash">{{ status.status.local_hash }}</div>
          </div>
        </div>
      </div>

      <!-- Model Lookup -->
      <div class="card">
        <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.pricing.lookup.title') }}</h2>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.pricing.lookup.description') }}</p>
        </div>
        <div class="p-6">
          <div class="flex gap-3">
            <input
              v-model="lookupModelName"
              @keyup.enter="doLookup"
              type="text"
              :placeholder="t('admin.pricing.lookup.placeholder')"
              class="input flex-1"
            />
            <button @click="doLookup" :disabled="lookingUp || !lookupModelName" class="btn btn-primary btn-sm">
              {{ lookingUp ? t('common.loading') : t('admin.pricing.lookup.button') }}
            </button>
          </div>
          <div v-if="lookupResult" class="mt-4 rounded-lg border border-gray-200 bg-gray-50 p-4 dark:border-dark-600 dark:bg-dark-800">
            <div class="text-sm font-medium text-gray-900 dark:text-white">{{ lookupResult.model }}</div>
            <div class="mt-2 grid grid-cols-2 gap-3 text-sm md:grid-cols-4">
              <div>
                <span class="text-gray-500 dark:text-gray-400">Input:</span>
                <span class="ml-1 font-mono text-gray-900 dark:text-white">${{ lookupResult.pricing.input_cost_per_mtok.toFixed(4) }}/MTok</span>
              </div>
              <div>
                <span class="text-gray-500 dark:text-gray-400">Output:</span>
                <span class="ml-1 font-mono text-gray-900 dark:text-white">${{ lookupResult.pricing.output_cost_per_mtok.toFixed(4) }}/MTok</span>
              </div>
              <div v-if="lookupResult.pricing.cache_creation_input_token_cost">
                <span class="text-gray-500 dark:text-gray-400">Cache Create:</span>
                <span class="ml-1 font-mono text-gray-900 dark:text-white">${{ (lookupResult.pricing.cache_creation_input_token_cost * 1_000_000).toFixed(4) }}/MTok</span>
              </div>
              <div v-if="lookupResult.pricing.cache_read_input_token_cost">
                <span class="text-gray-500 dark:text-gray-400">Cache Read:</span>
                <span class="ml-1 font-mono text-gray-900 dark:text-white">${{ (lookupResult.pricing.cache_read_input_token_cost * 1_000_000).toFixed(4) }}/MTok</span>
              </div>
            </div>
          </div>
          <div v-if="lookupError" class="mt-4 rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-600 dark:border-red-800 dark:bg-red-900/20 dark:text-red-400">
            {{ lookupError }}
          </div>
        </div>
      </div>

      <!-- Price List -->
      <div class="card">
        <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.pricing.list.title') }}</h2>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.pricing.list.description', { count: filteredItems.length }) }}</p>
        </div>
        <!-- Filters -->
        <div class="flex flex-wrap items-center gap-3 border-b border-gray-100 px-6 py-3 dark:border-dark-700">
          <input
            v-model="searchQuery"
            type="text"
            :placeholder="t('admin.pricing.list.searchPlaceholder')"
            class="input w-64"
          />
          <select v-model="selectedProvider" class="input w-48">
            <option value="">{{ t('admin.pricing.list.allProviders') }}</option>
            <option v-for="p in providers" :key="p" :value="p">{{ p }}</option>
          </select>
          <div class="ml-auto text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.pricing.list.showing', { count: paginatedItems.length, total: filteredItems.length }) }}
          </div>
        </div>
        <!-- Loading -->
        <div v-if="loading" class="flex items-center justify-center py-12">
          <div class="h-8 w-8 animate-spin rounded-full border-b-2 border-primary-600"></div>
        </div>
        <!-- Table -->
        <div v-else class="overflow-x-auto">
          <table class="w-full text-left text-sm">
            <thead class="border-b border-gray-200 bg-gray-50 dark:border-dark-600 dark:bg-dark-800">
              <tr>
                <th class="px-4 py-3 font-medium text-gray-600 dark:text-gray-300">{{ t('admin.pricing.list.model') }}</th>
                <th class="px-4 py-3 font-medium text-gray-600 dark:text-gray-300">{{ t('admin.pricing.list.provider') }}</th>
                <th class="px-4 py-3 font-medium text-gray-600 dark:text-gray-300">{{ t('admin.pricing.list.mode') }}</th>
                <th class="px-4 py-3 text-right font-medium text-gray-600 dark:text-gray-300">{{ t('admin.pricing.list.inputCost') }}</th>
                <th class="px-4 py-3 text-right font-medium text-gray-600 dark:text-gray-300">{{ t('admin.pricing.list.outputCost') }}</th>
                <th class="px-4 py-3 text-right font-medium text-gray-600 dark:text-gray-300">{{ t('admin.pricing.list.cacheCreate') }}</th>
                <th class="px-4 py-3 text-right font-medium text-gray-600 dark:text-gray-300">{{ t('admin.pricing.list.cacheRead') }}</th>
                <th class="px-4 py-3 text-center font-medium text-gray-600 dark:text-gray-300">{{ t('admin.pricing.list.caching') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="item in paginatedItems" :key="item.model" class="hover:bg-gray-50 dark:hover:bg-dark-800/50">
                <td class="max-w-xs truncate px-4 py-3 font-mono text-xs text-gray-900 dark:text-white" :title="item.model">{{ item.model }}</td>
                <td class="px-4 py-3 text-gray-600 dark:text-gray-300">
                  <span class="inline-flex items-center rounded-full bg-blue-50 px-2 py-0.5 text-xs font-medium text-blue-700 dark:bg-blue-900/30 dark:text-blue-300">{{ item.provider || '-' }}</span>
                </td>
                <td class="px-4 py-3 text-gray-600 dark:text-gray-300">{{ item.mode || '-' }}</td>
                <td class="px-4 py-3 text-right font-mono text-xs text-gray-900 dark:text-white">${{ item.input_cost_per_mtok.toFixed(4) }}</td>
                <td class="px-4 py-3 text-right font-mono text-xs text-gray-900 dark:text-white">${{ item.output_cost_per_mtok.toFixed(4) }}</td>
                <td class="px-4 py-3 text-right font-mono text-xs text-gray-500 dark:text-gray-400">{{ item.cache_creation_input_token_cost ? '$' + (item.cache_creation_input_token_cost * 1_000_000).toFixed(4) : '-' }}</td>
                <td class="px-4 py-3 text-right font-mono text-xs text-gray-500 dark:text-gray-400">{{ item.cache_read_input_token_cost ? '$' + (item.cache_read_input_token_cost * 1_000_000).toFixed(4) : '-' }}</td>
                <td class="px-4 py-3 text-center">
                  <span v-if="item.supports_prompt_caching" class="inline-flex h-5 w-5 items-center justify-center rounded-full bg-green-100 text-green-600 dark:bg-green-900/30 dark:text-green-400">✓</span>
                  <span v-else class="text-gray-300 dark:text-gray-600">-</span>
                </td>
              </tr>
              <tr v-if="paginatedItems.length === 0">
                <td colspan="8" class="px-4 py-8 text-center text-gray-500 dark:text-gray-400">
                  {{ t('admin.pricing.list.noData') }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <!-- Pagination -->
        <div v-if="totalPages > 1" class="flex items-center justify-between border-t border-gray-100 px-6 py-3 dark:border-dark-700">
          <button @click="currentPage = Math.max(1, currentPage - 1)" :disabled="currentPage <= 1" class="btn btn-sm">{{ t('common.previous') }}</button>
          <span class="text-sm text-gray-500 dark:text-gray-400">{{ currentPage }} / {{ totalPages }}</span>
          <button @click="currentPage = Math.min(totalPages, currentPage + 1)" :disabled="currentPage >= totalPages" class="btn btn-sm">{{ t('common.next') }}</button>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import { pricingAPI } from '@/api/admin/pricing'
import type { ModelPricingItem, PricingStatusResponse, ModelLookupResponse } from '@/api/admin/pricing'

const { t } = useI18n()

const loading = ref(false)
const updating = ref(false)
const uploading = ref(false)
const uploadMessage = ref('')
const uploadError = ref(false)
const lookingUp = ref(false)
const items = ref<ModelPricingItem[]>([])
const providers = ref<string[]>([])
const status = ref<PricingStatusResponse | null>(null)
const searchQuery = ref('')
const selectedProvider = ref('')
const lookupModelName = ref('')
const lookupResult = ref<ModelLookupResponse | null>(null)
const lookupError = ref('')
const currentPage = ref(1)
const pageSize = 50

const filteredItems = computed(() => {
  let result = items.value
  const q = searchQuery.value.toLowerCase().trim()
  if (q) {
    result = result.filter(
      (item) => item.model.toLowerCase().includes(q) || (item.provider && item.provider.toLowerCase().includes(q))
    )
  }
  if (selectedProvider.value) {
    result = result.filter((item) => item.provider === selectedProvider.value)
  }
  return result
})

const totalPages = computed(() => Math.ceil(filteredItems.value.length / pageSize))

const paginatedItems = computed(() => {
  const start = (currentPage.value - 1) * pageSize
  return filteredItems.value.slice(start, start + pageSize)
})

watch([searchQuery, selectedProvider], () => {
  currentPage.value = 1
})

const formatDate = (dateStr: string) => {
  if (!dateStr || dateStr === 'using fallback') return dateStr
  try {
    return new Date(dateStr).toLocaleString()
  } catch {
    return dateStr
  }
}

const loadData = async () => {
  loading.value = true
  try {
    const [pricingData, statusData] = await Promise.all([
      pricingAPI.list(),
      pricingAPI.getStatus()
    ])
    items.value = pricingData.items
    providers.value = pricingData.providers
    status.value = statusData
  } catch (error: any) {
    console.error('Failed to load pricing data:', error)
  } finally {
    loading.value = false
  }
}

const forceUpdate = async () => {
  updating.value = true
  try {
    await pricingAPI.forceUpdate()
    await loadData()
  } catch (error: any) {
    console.error('Failed to update pricing:', error)
  } finally {
    updating.value = false
  }
}

const handleFileUpload = async (event: Event) => {
  const target = event.target as HTMLInputElement
  const file = target.files?.[0]
  if (!file) return

  uploading.value = true
  uploadMessage.value = ''
  uploadError.value = false
  try {
    const result = await pricingAPI.upload(file)
    uploadMessage.value = `${result.message} (${result.model_count} models)`
    uploadError.value = false
    await loadData()
  } catch (error: any) {
    uploadMessage.value = error.message || 'Upload failed'
    uploadError.value = true
  } finally {
    uploading.value = false
    target.value = ''
  }
}

const doLookup = async () => {
  if (!lookupModelName.value) return
  lookingUp.value = true
  lookupResult.value = null
  lookupError.value = ''
  try {
    lookupResult.value = await pricingAPI.lookupModel(lookupModelName.value)
  } catch (error: any) {
    lookupError.value = error.message || 'Model not found'
  } finally {
    lookingUp.value = false
  }
}

onMounted(loadData)
</script>
