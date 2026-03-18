<template>
    <div class="space-y-6">
      <!-- PostgreSQL 数据库备份/恢复 -->
      <div class="card p-6">
        <div class="mb-4">
          <h3 class="text-base font-semibold text-gray-900 dark:text-white">
            {{ t('admin.dataManagement.postgres.title') }}
          </h3>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.dataManagement.postgres.description') }}
          </p>
        </div>

        <!-- 连接信息 -->
        <div v-if="pgInfo" class="mb-4 rounded-lg bg-gray-50 p-4 dark:bg-dark-800">
          <div class="grid grid-cols-2 gap-3 text-sm md:grid-cols-4">
            <div>
              <span class="text-gray-500 dark:text-gray-400">Host:</span>
              <span class="ml-1 font-mono text-gray-900 dark:text-white">{{ pgInfo.host }}</span>
            </div>
            <div>
              <span class="text-gray-500 dark:text-gray-400">Port:</span>
              <span class="ml-1 font-mono text-gray-900 dark:text-white">{{ pgInfo.port }}</span>
            </div>
            <div>
              <span class="text-gray-500 dark:text-gray-400">Database:</span>
              <span class="ml-1 font-mono text-gray-900 dark:text-white">{{ pgInfo.dbname }}</span>
            </div>
            <div>
              <span class="text-gray-500 dark:text-gray-400">SSL:</span>
              <span class="ml-1 font-mono text-gray-900 dark:text-white">{{ pgInfo.sslmode }}</span>
            </div>
          </div>
          <div v-if="!pgInfo.tools_ok" class="mt-2 text-sm text-red-600 dark:text-red-400">
            ⚠ {{ pgInfo.tools_error }}
          </div>
        </div>

        <!-- 导出 -->
        <div class="mb-4 flex flex-wrap items-center gap-3">
          <button
            type="button"
            class="btn btn-primary btn-sm"
            :disabled="pgExporting || !pgInfo?.tools_ok"
            @click="exportPostgres"
          >
            {{ pgExporting ? t('common.loading') : t('admin.dataManagement.postgres.export') }}
          </button>
          <span class="text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.dataManagement.postgres.exportHint') }}
          </span>
        </div>

        <!-- 恢复 -->
        <div class="rounded-lg border border-red-200 bg-red-50 p-4 dark:border-red-900/50 dark:bg-red-950/20">
          <h4 class="mb-2 text-sm font-semibold text-red-700 dark:text-red-400">
            {{ t('admin.dataManagement.postgres.restore') }}
          </h4>
          <p class="mb-3 text-xs text-red-600 dark:text-red-400">
            {{ t('admin.dataManagement.postgres.restoreWarning') }}
          </p>
          <div class="flex flex-col gap-3 sm:flex-row sm:items-end">
            <div class="flex-1">
              <label class="mb-1 block text-xs text-gray-700 dark:text-gray-300">
                {{ t('admin.dataManagement.postgres.selectFile') }}
              </label>
              <input
                type="file"
                accept=".dump"
                class="input w-full text-sm"
                @change="onRestoreFileChange"
              />
            </div>
            <div class="flex-1">
              <label class="mb-1 block text-xs text-gray-700 dark:text-gray-300">
                {{ t('admin.dataManagement.postgres.confirmLabel', { dbname: pgInfo?.dbname || '...' }) }}
              </label>
              <input
                v-model="pgRestoreConfirm"
                class="input w-full font-mono text-sm"
                :placeholder="`RESTORE ${pgInfo?.dbname || '...'}`"
              />
            </div>
            <button
              type="button"
              class="btn btn-danger btn-sm whitespace-nowrap"
              :disabled="pgRestoring || !pgRestoreFile || !pgRestoreConfirmValid || !pgInfo?.tools_ok"
              @click="restorePostgres"
            >
              {{ pgRestoring ? t('common.loading') : t('admin.dataManagement.postgres.restoreBtn') }}
            </button>
          </div>
          <!-- 上传进度 -->
          <div v-if="pgRestoring" class="mt-3">
            <div class="mb-1 flex items-center justify-between text-xs text-gray-600 dark:text-gray-400">
              <span>{{ pgRestorePhase === 'uploading' ? t('admin.dataManagement.postgres.uploading', { current: pgRestoreProgress, total: pgRestoreTotal }) : t('admin.dataManagement.postgres.executingRestore') }}</span>
              <span v-if="pgRestorePhase === 'uploading' && pgRestoreTotal > 0">{{ Math.round(pgRestoreProgress / pgRestoreTotal * 100) }}%</span>
            </div>
            <div class="h-2 w-full overflow-hidden rounded-full bg-gray-200 dark:bg-dark-700">
              <div
                class="h-full rounded-full transition-all duration-300"
                :class="pgRestorePhase === 'restoring' ? 'animate-pulse bg-yellow-500' : 'bg-blue-500'"
                :style="{ width: pgRestorePhase === 'uploading' && pgRestoreTotal > 0 ? `${(pgRestoreProgress / pgRestoreTotal) * 100}%` : '100%' }"
              ></div>
            </div>
          </div>
        </div>
      </div>

      <div class="card p-6">
        <div class="mb-4 flex flex-wrap items-center justify-between gap-3">
          <div>
            <h3 class="text-base font-semibold text-gray-900 dark:text-white">
              {{ t('admin.settings.soraS3.title') }}
            </h3>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
              {{ t('admin.settings.soraS3.description') }}
            </p>
          </div>
          <div class="flex flex-wrap gap-2">
            <button type="button" class="btn btn-secondary btn-sm" @click="startCreateSoraProfile">
              {{ t('admin.settings.soraS3.newProfile') }}
            </button>
            <button type="button" class="btn btn-secondary btn-sm" :disabled="loadingSoraProfiles" @click="loadSoraS3Profiles">
              {{ loadingSoraProfiles ? t('common.loading') : t('admin.settings.soraS3.reloadProfiles') }}
            </button>
          </div>
        </div>

        <div class="overflow-x-auto">
          <table class="w-full min-w-[1000px] text-sm">
            <thead>
              <tr class="border-b border-gray-200 text-left text-xs uppercase tracking-wide text-gray-500 dark:border-dark-700 dark:text-gray-400">
                <th class="py-2 pr-4">{{ t('admin.settings.soraS3.columns.profile') }}</th>
                <th class="py-2 pr-4">{{ t('admin.settings.soraS3.columns.active') }}</th>
                <th class="py-2 pr-4">{{ t('admin.settings.soraS3.columns.endpoint') }}</th>
                <th class="py-2 pr-4">{{ t('admin.settings.soraS3.columns.bucket') }}</th>
                <th class="py-2 pr-4">{{ t('admin.settings.soraS3.columns.quota') }}</th>
                <th class="py-2 pr-4">{{ t('admin.settings.soraS3.columns.updatedAt') }}</th>
                <th class="py-2">{{ t('admin.settings.soraS3.columns.actions') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="profile in soraS3Profiles" :key="profile.profile_id" class="border-b border-gray-100 align-top dark:border-dark-800">
                <td class="py-3 pr-4">
                  <div class="font-mono text-xs">{{ profile.profile_id }}</div>
                  <div class="mt-1 text-xs text-gray-600 dark:text-gray-400">{{ profile.name }}</div>
                </td>
                <td class="py-3 pr-4">
                  <span
                    class="rounded px-2 py-0.5 text-xs"
                    :class="profile.is_active ? 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300' : 'bg-gray-100 text-gray-700 dark:bg-dark-800 dark:text-gray-300'"
                  >
                    {{ profile.is_active ? t('common.enabled') : t('common.disabled') }}
                  </span>
                </td>
                <td class="py-3 pr-4 text-xs">
                  <div>{{ profile.endpoint || '-' }}</div>
                  <div class="mt-1 text-gray-500 dark:text-gray-400">{{ profile.region || '-' }}</div>
                </td>
                <td class="py-3 pr-4 text-xs">{{ profile.bucket || '-' }}</td>
                <td class="py-3 pr-4 text-xs">{{ formatStorageQuotaGB(profile.default_storage_quota_bytes) }}</td>
                <td class="py-3 pr-4 text-xs">{{ formatDate(profile.updated_at) }}</td>
                <td class="py-3 text-xs">
                  <div class="flex flex-wrap gap-2">
                    <button type="button" class="btn btn-secondary btn-xs" @click="editSoraProfile(profile.profile_id)">
                      {{ t('common.edit') }}
                    </button>
                    <button
                      v-if="!profile.is_active"
                      type="button"
                      class="btn btn-secondary btn-xs"
                      :disabled="activatingSoraProfile"
                      @click="activateSoraProfile(profile.profile_id)"
                    >
                      {{ t('admin.settings.soraS3.activateProfile') }}
                    </button>
                    <button
                      type="button"
                      class="btn btn-danger btn-xs"
                      :disabled="deletingSoraProfile"
                      @click="removeSoraProfile(profile.profile_id)"
                    >
                      {{ t('common.delete') }}
                    </button>
                  </div>
                </td>
              </tr>
              <tr v-if="soraS3Profiles.length === 0">
                <td colspan="7" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">
                  {{ t('admin.settings.soraS3.empty') }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>

    <Teleport to="body">
      <Transition name="dm-drawer-mask">
        <div
          v-if="soraProfileDrawerOpen"
          class="fixed inset-0 z-[54] bg-black/40 backdrop-blur-sm"
          @click="closeSoraProfileDrawer"
        ></div>
      </Transition>

      <Transition name="dm-drawer-panel">
        <div
          v-if="soraProfileDrawerOpen"
          class="fixed inset-y-0 right-0 z-[55] flex h-full w-full max-w-2xl flex-col border-l border-gray-200 bg-white shadow-2xl dark:border-dark-700 dark:bg-dark-900"
        >
          <div class="flex items-center justify-between border-b border-gray-200 px-4 py-3 dark:border-dark-700">
            <h4 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ creatingSoraProfile ? t('admin.settings.soraS3.createTitle') : t('admin.settings.soraS3.editTitle') }}
            </h4>
            <button
              type="button"
              class="rounded p-1 text-gray-500 hover:bg-gray-100 hover:text-gray-700 dark:text-gray-400 dark:hover:bg-dark-800 dark:hover:text-gray-200"
              @click="closeSoraProfileDrawer"
            >
              ✕
            </button>
          </div>

          <div class="flex-1 overflow-y-auto p-4">
            <div class="grid grid-cols-1 gap-3 md:grid-cols-2">
              <input
                v-model="soraProfileForm.profile_id"
                class="input w-full"
                :placeholder="t('admin.settings.soraS3.profileID')"
                :disabled="!creatingSoraProfile"
              />
              <input
                v-model="soraProfileForm.name"
                class="input w-full"
                :placeholder="t('admin.settings.soraS3.profileName')"
              />
              <label class="inline-flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300 md:col-span-2">
                <input v-model="soraProfileForm.enabled" type="checkbox" />
                <span>{{ t('admin.settings.soraS3.enabled') }}</span>
              </label>
              <input v-model="soraProfileForm.endpoint" class="input w-full" :placeholder="t('admin.settings.soraS3.endpoint')" />
              <input v-model="soraProfileForm.region" class="input w-full" :placeholder="t('admin.settings.soraS3.region')" />
              <input v-model="soraProfileForm.bucket" class="input w-full" :placeholder="t('admin.settings.soraS3.bucket')" />
              <input v-model="soraProfileForm.prefix" class="input w-full" :placeholder="t('admin.settings.soraS3.prefix')" />
              <input v-model="soraProfileForm.access_key_id" class="input w-full" :placeholder="t('admin.settings.soraS3.accessKeyId')" />
              <input
                v-model="soraProfileForm.secret_access_key"
                type="password"
                class="input w-full"
                :placeholder="soraProfileForm.secret_access_key_configured ? t('admin.settings.soraS3.secretConfigured') : t('admin.settings.soraS3.secretAccessKey')"
              />
              <input v-model="soraProfileForm.cdn_url" class="input w-full" :placeholder="t('admin.settings.soraS3.cdnUrl')" />
              <div>
                <input
                  v-model.number="soraProfileForm.default_storage_quota_gb"
                  type="number"
                  min="0"
                  step="0.1"
                  class="input w-full"
                  :placeholder="t('admin.settings.soraS3.defaultQuota')"
                />
                <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.settings.soraS3.defaultQuotaHint') }}</p>
              </div>
              <label class="inline-flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
                <input v-model="soraProfileForm.force_path_style" type="checkbox" />
                <span>{{ t('admin.settings.soraS3.forcePathStyle') }}</span>
              </label>
              <label v-if="creatingSoraProfile" class="inline-flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300 md:col-span-2">
                <input v-model="soraProfileForm.set_active" type="checkbox" />
                <span>{{ t('admin.settings.soraS3.setActive') }}</span>
              </label>
            </div>
          </div>

          <div class="flex flex-wrap justify-end gap-2 border-t border-gray-200 p-4 dark:border-dark-700">
            <button type="button" class="btn btn-secondary btn-sm" @click="closeSoraProfileDrawer">
              {{ t('common.cancel') }}
            </button>
            <button type="button" class="btn btn-secondary btn-sm" :disabled="testingSoraProfile || !soraProfileForm.enabled" @click="testSoraProfileConnection">
              {{ testingSoraProfile ? t('common.loading') : t('admin.settings.soraS3.testConnection') }}
            </button>
            <button type="button" class="btn btn-primary btn-sm" :disabled="savingSoraProfile" @click="saveSoraProfile">
              {{ savingSoraProfile ? t('common.loading') : t('admin.settings.soraS3.saveProfile') }}
            </button>
          </div>
        </div>
      </Transition>
    </Teleport>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { SoraS3Profile } from '@/api/admin/settings'
import { adminAPI } from '@/api'
import { getPostgresInfo, getPostgresExportUrl, initChunkedRestore, uploadRestoreChunk, completeChunkedRestore, abortChunkedRestore } from '@/api/admin/dataManagement'
import type { PostgresInfo } from '@/api/admin/dataManagement'
import { useAppStore } from '@/stores'

const { t } = useI18n()
const appStore = useAppStore()

// PostgreSQL backup/restore state
const pgInfo = ref<PostgresInfo | null>(null)
const pgExporting = ref(false)
const pgRestoring = ref(false)
const pgRestoreFile = ref<File | null>(null)
const pgRestoreConfirm = ref('')
const pgRestoreProgress = ref(0)
const pgRestoreTotal = ref(0)
const pgRestorePhase = ref<'idle' | 'uploading' | 'restoring'>('idle')

const pgRestoreConfirmValid = computed(() => {
  if (!pgInfo.value) return false
  return pgRestoreConfirm.value === `RESTORE ${pgInfo.value.dbname}`
})

async function loadPostgresInfo() {
  try {
    pgInfo.value = await getPostgresInfo()
  } catch (error) {
    console.error('Failed to load postgres info:', error)
  }
}

async function exportPostgres() {
  pgExporting.value = true
  try {
    const url = getPostgresExportUrl()
    const token = localStorage.getItem('auth_token')
    const resp = await fetch(url, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    })
    if (!resp.ok) {
      const text = await resp.text()
      throw new Error(text || resp.statusText)
    }
    const blob = await resp.blob()
    const disposition = resp.headers.get('Content-Disposition') || ''
    const match = disposition.match(/filename="?([^"]+)"?/)
    const filename = match ? match[1] : `sub2api-postgres-backup.dump`
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = filename
    a.click()
    URL.revokeObjectURL(a.href)
    appStore.showSuccess(t('admin.dataManagement.postgres.exportSuccess'))
  } catch (error) {
    appStore.showError((error as { message?: string })?.message || t('errors.networkError'))
  } finally {
    pgExporting.value = false
  }
}

function onRestoreFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  pgRestoreFile.value = input.files?.[0] || null
}

async function restorePostgres() {
  if (!pgRestoreFile.value || !pgRestoreConfirmValid.value) return
  const file = pgRestoreFile.value
  const confirm = pgRestoreConfirm.value

  pgRestoring.value = true
  pgRestorePhase.value = 'uploading'
  pgRestoreProgress.value = 0
  pgRestoreTotal.value = 0

  let uploadId = ''
  try {
    // 1. Init upload session
    const init = await initChunkedRestore(file.name, file.size)
    uploadId = init.upload_id
    const chunkSize = init.chunk_size
    const chunkCount = init.chunk_count
    pgRestoreTotal.value = chunkCount

    // 2. Upload chunks sequentially
    for (let i = 0; i < chunkCount; i++) {
      const start = i * chunkSize
      const end = Math.min(start + chunkSize, file.size)
      const chunk = file.slice(start, end)
      await uploadRestoreChunk(uploadId, i, chunk)
      pgRestoreProgress.value = i + 1
    }

    // 3. Complete: merge + pg_restore
    pgRestorePhase.value = 'restoring'
    const result = await completeChunkedRestore(uploadId, confirm)
    appStore.showSuccess(result.message || t('admin.dataManagement.postgres.restoreSuccess'))
    pgRestoreConfirm.value = ''
    pgRestoreFile.value = null
  } catch (error) {
    appStore.showError((error as { message?: string })?.message || t('errors.networkError'))
    if (uploadId) {
      try { await abortChunkedRestore(uploadId) } catch { /* ignore */ }
    }
  } finally {
    pgRestoring.value = false
    pgRestorePhase.value = 'idle'
    pgRestoreProgress.value = 0
    pgRestoreTotal.value = 0
  }
}

const loadingSoraProfiles = ref(false)
const savingSoraProfile = ref(false)
const testingSoraProfile = ref(false)
const activatingSoraProfile = ref(false)
const deletingSoraProfile = ref(false)
const creatingSoraProfile = ref(false)
const soraProfileDrawerOpen = ref(false)

const soraS3Profiles = ref<SoraS3Profile[]>([])
const selectedSoraProfileID = ref('')

type SoraS3ProfileForm = {
  profile_id: string
  name: string
  set_active: boolean
  enabled: boolean
  endpoint: string
  region: string
  bucket: string
  access_key_id: string
  secret_access_key: string
  secret_access_key_configured: boolean
  prefix: string
  force_path_style: boolean
  cdn_url: string
  default_storage_quota_gb: number
}

const soraProfileForm = ref<SoraS3ProfileForm>(newDefaultSoraS3ProfileForm())

async function loadSoraS3Profiles() {
  loadingSoraProfiles.value = true
  try {
    const result = await adminAPI.settings.listSoraS3Profiles()
    soraS3Profiles.value = result.items || []
    if (!creatingSoraProfile.value) {
      const stillExists = selectedSoraProfileID.value
        ? soraS3Profiles.value.some((item) => item.profile_id === selectedSoraProfileID.value)
        : false
      if (!stillExists) {
        selectedSoraProfileID.value = pickPreferredSoraProfileID()
      }
      syncSoraProfileFormWithSelection()
    }
  } catch (error) {
    appStore.showError((error as { message?: string })?.message || t('errors.networkError'))
  } finally {
    loadingSoraProfiles.value = false
  }
}

function startCreateSoraProfile() {
  creatingSoraProfile.value = true
  selectedSoraProfileID.value = ''
  soraProfileForm.value = newDefaultSoraS3ProfileForm()
  soraProfileDrawerOpen.value = true
}

function editSoraProfile(profileID: string) {
  selectedSoraProfileID.value = profileID
  creatingSoraProfile.value = false
  syncSoraProfileFormWithSelection()
  soraProfileDrawerOpen.value = true
}

function closeSoraProfileDrawer() {
  soraProfileDrawerOpen.value = false
  if (creatingSoraProfile.value) {
    creatingSoraProfile.value = false
    selectedSoraProfileID.value = pickPreferredSoraProfileID()
    syncSoraProfileFormWithSelection()
  }
}

async function saveSoraProfile() {
  if (!soraProfileForm.value.name.trim()) {
    appStore.showError(t('admin.settings.soraS3.profileNameRequired'))
    return
  }
  if (creatingSoraProfile.value && !soraProfileForm.value.profile_id.trim()) {
    appStore.showError(t('admin.settings.soraS3.profileIDRequired'))
    return
  }
  if (!creatingSoraProfile.value && !selectedSoraProfileID.value) {
    appStore.showError(t('admin.settings.soraS3.profileSelectRequired'))
    return
  }
  if (soraProfileForm.value.enabled) {
    if (!soraProfileForm.value.endpoint.trim()) {
      appStore.showError(t('admin.settings.soraS3.endpointRequired'))
      return
    }
    if (!soraProfileForm.value.bucket.trim()) {
      appStore.showError(t('admin.settings.soraS3.bucketRequired'))
      return
    }
    if (!soraProfileForm.value.access_key_id.trim()) {
      appStore.showError(t('admin.settings.soraS3.accessKeyRequired'))
      return
    }
  }

  savingSoraProfile.value = true
  try {
    if (creatingSoraProfile.value) {
      const created = await adminAPI.settings.createSoraS3Profile({
        profile_id: soraProfileForm.value.profile_id.trim(),
        name: soraProfileForm.value.name.trim(),
        set_active: soraProfileForm.value.set_active,
        enabled: soraProfileForm.value.enabled,
        endpoint: soraProfileForm.value.endpoint,
        region: soraProfileForm.value.region,
        bucket: soraProfileForm.value.bucket,
        access_key_id: soraProfileForm.value.access_key_id,
        secret_access_key: soraProfileForm.value.secret_access_key || undefined,
        prefix: soraProfileForm.value.prefix,
        force_path_style: soraProfileForm.value.force_path_style,
        cdn_url: soraProfileForm.value.cdn_url,
        default_storage_quota_bytes: Math.round((soraProfileForm.value.default_storage_quota_gb || 0) * 1024 * 1024 * 1024)
      })
      selectedSoraProfileID.value = created.profile_id
      creatingSoraProfile.value = false
      soraProfileDrawerOpen.value = false
      appStore.showSuccess(t('admin.settings.soraS3.profileCreated'))
    } else {
      await adminAPI.settings.updateSoraS3Profile(selectedSoraProfileID.value, {
        name: soraProfileForm.value.name.trim(),
        enabled: soraProfileForm.value.enabled,
        endpoint: soraProfileForm.value.endpoint,
        region: soraProfileForm.value.region,
        bucket: soraProfileForm.value.bucket,
        access_key_id: soraProfileForm.value.access_key_id,
        secret_access_key: soraProfileForm.value.secret_access_key || undefined,
        prefix: soraProfileForm.value.prefix,
        force_path_style: soraProfileForm.value.force_path_style,
        cdn_url: soraProfileForm.value.cdn_url,
        default_storage_quota_bytes: Math.round((soraProfileForm.value.default_storage_quota_gb || 0) * 1024 * 1024 * 1024)
      })
      soraProfileDrawerOpen.value = false
      appStore.showSuccess(t('admin.settings.soraS3.profileSaved'))
    }
    await loadSoraS3Profiles()
  } catch (error) {
    appStore.showError((error as { message?: string })?.message || t('errors.networkError'))
  } finally {
    savingSoraProfile.value = false
  }
}

async function testSoraProfileConnection() {
  testingSoraProfile.value = true
  try {
    const result = await adminAPI.settings.testSoraS3Connection({
      profile_id: creatingSoraProfile.value ? undefined : selectedSoraProfileID.value,
      enabled: soraProfileForm.value.enabled,
      endpoint: soraProfileForm.value.endpoint,
      region: soraProfileForm.value.region,
      bucket: soraProfileForm.value.bucket,
      access_key_id: soraProfileForm.value.access_key_id,
      secret_access_key: soraProfileForm.value.secret_access_key || undefined,
      prefix: soraProfileForm.value.prefix,
      force_path_style: soraProfileForm.value.force_path_style,
      cdn_url: soraProfileForm.value.cdn_url,
      default_storage_quota_bytes: Math.round((soraProfileForm.value.default_storage_quota_gb || 0) * 1024 * 1024 * 1024)
    })
    appStore.showSuccess(result.message || t('admin.settings.soraS3.testSuccess'))
  } catch (error) {
    appStore.showError((error as { message?: string })?.message || t('errors.networkError'))
  } finally {
    testingSoraProfile.value = false
  }
}

async function activateSoraProfile(profileID: string) {
  activatingSoraProfile.value = true
  try {
    await adminAPI.settings.setActiveSoraS3Profile(profileID)
    appStore.showSuccess(t('admin.settings.soraS3.profileActivated'))
    await loadSoraS3Profiles()
  } catch (error) {
    appStore.showError((error as { message?: string })?.message || t('errors.networkError'))
  } finally {
    activatingSoraProfile.value = false
  }
}

async function removeSoraProfile(profileID: string) {
  if (!window.confirm(t('admin.settings.soraS3.deleteConfirm', { profileID }))) {
    return
  }
  deletingSoraProfile.value = true
  try {
    await adminAPI.settings.deleteSoraS3Profile(profileID)
    if (selectedSoraProfileID.value === profileID) {
      selectedSoraProfileID.value = ''
    }
    appStore.showSuccess(t('admin.settings.soraS3.profileDeleted'))
    await loadSoraS3Profiles()
  } catch (error) {
    appStore.showError((error as { message?: string })?.message || t('errors.networkError'))
  } finally {
    deletingSoraProfile.value = false
  }
}

function formatDate(value?: string): string {
  if (!value) {
    return '-'
  }
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) {
    return value
  }
  return date.toLocaleString()
}

function formatStorageQuotaGB(bytes: number): string {
  if (!bytes || bytes <= 0) {
    return '0 GB'
  }
  const gb = bytes / (1024 * 1024 * 1024)
  return `${gb.toFixed(gb >= 10 ? 0 : 1)} GB`
}

function pickPreferredSoraProfileID(): string {
  const active = soraS3Profiles.value.find((item) => item.is_active)
  if (active) {
    return active.profile_id
  }
  return soraS3Profiles.value[0]?.profile_id || ''
}

function syncSoraProfileFormWithSelection() {
  const profile = soraS3Profiles.value.find((item) => item.profile_id === selectedSoraProfileID.value)
  soraProfileForm.value = newDefaultSoraS3ProfileForm(profile)
}

function newDefaultSoraS3ProfileForm(profile?: SoraS3Profile): SoraS3ProfileForm {
  if (!profile) {
    return {
      profile_id: '',
      name: '',
      set_active: false,
      enabled: false,
      endpoint: '',
      region: '',
      bucket: '',
      access_key_id: '',
      secret_access_key: '',
      secret_access_key_configured: false,
      prefix: 'sora/',
      force_path_style: false,
      cdn_url: '',
      default_storage_quota_gb: 0
    }
  }

  const quotaBytes = profile.default_storage_quota_bytes || 0

  return {
    profile_id: profile.profile_id,
    name: profile.name,
    set_active: false,
    enabled: profile.enabled,
    endpoint: profile.endpoint || '',
    region: profile.region || '',
    bucket: profile.bucket || '',
    access_key_id: profile.access_key_id || '',
    secret_access_key: '',
    secret_access_key_configured: Boolean(profile.secret_access_key_configured),
    prefix: profile.prefix || '',
    force_path_style: Boolean(profile.force_path_style),
    cdn_url: profile.cdn_url || '',
    default_storage_quota_gb: Number((quotaBytes / (1024 * 1024 * 1024)).toFixed(2))
  }
}

onMounted(async () => {
  await Promise.all([loadPostgresInfo(), loadSoraS3Profiles()])
})
</script>

<style scoped>
.dm-drawer-mask-enter-active,
.dm-drawer-mask-leave-active {
  transition: opacity 0.2s ease;
}

.dm-drawer-mask-enter-from,
.dm-drawer-mask-leave-to {
  opacity: 0;
}

.dm-drawer-panel-enter-active,
.dm-drawer-panel-leave-active {
  transition:
    transform 0.24s cubic-bezier(0.22, 1, 0.36, 1),
    opacity 0.2s ease;
}

.dm-drawer-panel-enter-from,
.dm-drawer-panel-leave-to {
  opacity: 0.96;
  transform: translateX(100%);
}

@media (prefers-reduced-motion: reduce) {
  .dm-drawer-mask-enter-active,
  .dm-drawer-mask-leave-active,
  .dm-drawer-panel-enter-active,
  .dm-drawer-panel-leave-active {
    transition-duration: 0s;
  }
}
</style>
