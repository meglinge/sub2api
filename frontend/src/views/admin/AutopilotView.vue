<template>
  <AppLayout>
    <div class="space-y-6">
      <!-- Header -->
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 class="text-xl font-semibold text-gray-900 dark:text-white">
            {{ t('admin.autopilot.title') }}
          </h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">
            {{ t('admin.autopilot.description') }}
          </p>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <button type="button" class="btn btn-secondary" :disabled="loading" @click="refresh">
            <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
          </button>
          <button type="button" class="btn btn-primary" :disabled="loading || analyzing" @click="onAnalyze">
            {{ analyzing ? t('admin.autopilot.analyzing') : t('admin.autopilot.analyzeNow') }}
          </button>
        </div>
      </div>

      <div v-if="error" class="rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700 dark:border-red-900/40 dark:bg-red-900/20 dark:text-red-300">
        {{ error }}
      </div>
      <div v-if="message" class="rounded-lg border border-green-200 bg-green-50 px-4 py-3 text-sm text-green-700 dark:border-green-900/40 dark:bg-green-900/20 dark:text-green-300">
        {{ message }}
      </div>

      <!-- Running banner (UpstreamRouter-style) -->
      <div
        v-if="liveStatus?.running"
        class="rounded-2xl border border-primary-300/60 bg-primary-50/60 px-4 py-3 dark:border-primary-700/50 dark:bg-primary-900/20"
      >
        <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
          <span class="relative flex h-2.5 w-2.5 shrink-0">
            <span class="absolute inline-flex h-full w-full animate-ping rounded-full bg-primary-500/70" />
            <span class="relative inline-flex h-2.5 w-2.5 rounded-full bg-primary-600" />
          </span>
          <div class="min-w-0 flex-1 space-y-1">
            <div class="flex flex-wrap items-center gap-2">
              <span class="text-sm font-semibold text-gray-900 dark:text-white">
                {{ t('admin.autopilot.runningBanner') }}
              </span>
              <span class="badge badge-gray">
                {{ liveStatus.trigger === 'manual' ? t('admin.autopilot.triggerManual') : t('admin.autopilot.triggerSchedule') }}
              </span>
              <span class="text-sm text-gray-600 dark:text-gray-300">{{ phaseLabel(liveStatus.phase) }}</span>
              <span v-if="liveStatus.turn > 0" class="badge badge-gray">
                {{
                  t('admin.autopilot.turnOf', {
                    turn: liveStatus.turn,
                    max: (liveStatus.max_turns || 0) > 0 ? liveStatus.max_turns + 1 : liveStatus.turn,
                  })
                }}
              </span>
            </div>
            <p class="text-xs text-gray-500">
              {{ t('admin.autopilot.startedAt') }}
              {{ formatStartedAt(liveStatus.started_at) }}
            </p>
          </div>
          <span class="shrink-0 text-2xl font-semibold tabular-nums text-gray-900 dark:text-white">
            {{ elapsedText }}
          </span>
        </div>
      </div>

      <!-- Master switch -->
      <div class="card border-primary-200 p-6 dark:border-primary-900/40">
        <div class="flex flex-wrap items-start justify-between gap-4">
          <div class="min-w-0 flex-1">
            <h3 class="text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.autopilot.masterSwitch') }}</h3>
            <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ t('admin.autopilot.masterHint') }}</p>
            <div class="mt-2 flex flex-wrap items-center gap-2">
              <span class="badge" :class="scheduleEnabled ? 'badge-success' : 'badge-gray'">
                {{ scheduleEnabled ? t('admin.autopilot.scheduleBadgeOn') : t('admin.autopilot.scheduleBadgeOff') }}
              </span>
              <span v-if="liveStatus || status" class="badge" :class="(liveStatus || status)?.running ? 'badge-warning' : 'badge-gray'">
                {{ (liveStatus || status)?.running ? t('admin.autopilot.running') : t('admin.autopilot.idle') }}
              </span>
              <span v-if="settings" class="text-xs text-gray-500">
                {{ settings.interval_minutes }}min ·
                {{ settings.apply_mode === 'auto' ? t('admin.autopilot.applyModeAuto') : t('admin.autopilot.applyModeSuggest') }}
              </span>
            </div>
          </div>
          <button
            type="button"
            class="btn shrink-0"
            :class="scheduleEnabled ? 'btn-secondary' : 'btn-primary'"
            :disabled="!settings || toggling || loading"
            @click="toggleSchedule"
          >
            {{
              toggling
                ? t('admin.autopilot.toggling')
                : scheduleEnabled
                  ? t('admin.autopilot.stopAutopilot')
                  : t('admin.autopilot.startAutopilot')
            }}
          </button>
        </div>
      </div>

      <!-- Tabs -->
      <div class="flex flex-wrap gap-2 border-b border-gray-200 pb-2 dark:border-dark-700">
        <button
          v-for="tab in tabs"
          :key="tab.key"
          type="button"
          class="rounded-lg px-3 py-1.5 text-sm font-medium transition"
          :class="activeTab === tab.key
            ? 'bg-primary-50 text-primary-700 dark:bg-primary-900/30 dark:text-primary-300'
            : 'text-gray-600 hover:bg-gray-50 dark:text-gray-300 dark:hover:bg-dark-800'"
          @click="activeTab = tab.key"
        >
          {{ tab.label }}
        </button>
        <button
          type="button"
          class="ml-auto rounded-lg px-3 py-1.5 text-sm font-medium text-gray-600 hover:bg-gray-50 dark:text-gray-300 dark:hover:bg-dark-800"
          :class="activeTab === 'settings' ? 'bg-primary-50 text-primary-700 dark:bg-primary-900/30 dark:text-primary-300' : ''"
          @click="activeTab = 'settings'"
        >
          {{ t('admin.autopilot.settingsTitle') }}
        </button>
      </div>

      <!-- Runs -->
      <div v-if="activeTab === 'runs'" class="space-y-4">
        <div
          v-if="settings?.apply_mode === 'suggest_only'"
          class="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-900 dark:border-amber-900/40 dark:bg-amber-900/20 dark:text-amber-100"
        >
          {{ t('admin.autopilot.observeModeHint') }}
        </div>
        <!-- 待审建议：仅观察模式（suggest_only）展示；auto 模式由后端自动执行/清理 -->
        <div
          v-if="settings?.apply_mode === 'suggest_only'"
          class="card p-6"
        >
          <div class="mb-4 flex items-center justify-between">
            <h3 class="text-base font-semibold text-gray-900 dark:text-white">
              {{ t('admin.autopilot.suggestionsTitle') }}
              <span class="ml-1 text-sm font-normal text-gray-500">({{ suggestions.length }})</span>
            </h3>
          </div>
          <DataTable :columns="suggestionColumns" :data="suggestions" :loading="loading && !suggestions.length">
            <template #cell-account="{ row }">
              <div class="min-w-0">
                <div class="truncate font-medium">{{ row.account_name || '-' }}</div>
                <div class="text-xs text-gray-500">#{{ row.account_id }}</div>
              </div>
            </template>
            <template #cell-op="{ value }">
              <code class="rounded bg-gray-100 px-1.5 py-0.5 text-xs dark:bg-dark-700">{{ value }}</code>
            </template>
            <template #cell-confidence="{ value }">
              <span class="tabular-nums">{{ Number(value || 0).toFixed(2) }}</span>
            </template>
            <template #cell-actions="{ row }">
              <div class="flex flex-wrap gap-1">
                <button type="button" class="btn btn-primary btn-sm" @click="onApprove(row.id)">{{ t('admin.autopilot.approve') }}</button>
                <button type="button" class="btn btn-secondary btn-sm" @click="onDismiss(row.id)">{{ t('admin.autopilot.dismiss') }}</button>
              </div>
            </template>
            <template #empty>
              <EmptyState :title="t('admin.autopilot.noSuggestions')" :description="t('admin.autopilot.noSuggestionsHint')" />
            </template>
          </DataTable>
        </div>

        <div class="card p-6">
          <h3 class="mb-4 text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.autopilot.runsTitle') }}</h3>
          <div class="space-y-3">
            <div
              v-for="run in runs"
              :key="run.id"
              class="rounded-xl border border-gray-100 p-4 dark:border-dark-700"
            >
              <div class="flex flex-wrap items-start justify-between gap-2">
                <div>
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="font-medium tabular-nums">#{{ run.id }}</span>
                    <span class="badge" :class="runStatusClass(run.status)">{{ run.status }}</span>
                    <span class="text-xs text-gray-500">{{ run.trigger }} · {{ run.model }}</span>
                    <span class="text-xs text-gray-500">{{ formatTs(run.ts) }}</span>
                  </div>
                  <p class="mt-2 text-sm text-gray-700 dark:text-gray-200">{{ run.summary || run.error || '-' }}</p>
                  <p v-if="run.latency_ms" class="mt-1 text-xs text-gray-500">
                    {{ run.latency_ms }}ms · in {{ run.input_tokens }} / out {{ run.output_tokens }} · turns {{ run.turns || 1 }}
                  </p>
                </div>
                <div class="flex flex-wrap gap-1">
                  <button
                    v-if="(run.suggested_count || 0) > 0 || (run.actions || []).some(a => a.state === 'suggested')"
                    type="button"
                    class="btn btn-primary btn-sm"
                    @click="onApproveRun(run.id)"
                  >
                    {{ t('admin.autopilot.approveAll') }}
                  </button>
                  <button
                    v-if="(run.suggested_count || 0) > 0 || (run.actions || []).some(a => a.state === 'suggested')"
                    type="button"
                    class="btn btn-secondary btn-sm"
                    @click="onDismissRun(run.id)"
                  >
                    {{ t('admin.autopilot.dismissAll') }}
                  </button>
                  <button type="button" class="btn btn-secondary btn-sm" @click="toggleRun(run.id)">
                    {{ expandedRun === run.id ? t('admin.autopilot.collapse') : t('admin.autopilot.expand') }}
                  </button>
                </div>
              </div>
              <div v-if="expandedRun === run.id" class="mt-3 space-y-1">
                <div
                  v-for="a in (run.actions || [])"
                  :key="a.id"
                  class="flex flex-wrap items-center gap-2 rounded border border-gray-100 bg-gray-50 px-2 py-1 text-xs dark:border-dark-700 dark:bg-dark-800"
                >
                  <span class="badge badge-gray">{{ a.state }}</span>
                  <span class="font-medium">{{ a.account_name }}</span>
                  <code>{{ a.op }}</code>
                  <span class="text-gray-500">{{ a.before }} → {{ a.after }}</span>
                  <span class="text-gray-500">{{ a.reason }}</span>
                  <button
                    v-if="a.state === 'applied'"
                    type="button"
                    class="text-primary-600 underline"
                    @click="onRollback(a.id)"
                  >
                    {{ t('admin.autopilot.rollback') }}
                  </button>
                  <span v-if="a.reject_reason" class="text-amber-600">{{ a.reject_reason }}</span>
                </div>
                <p v-if="!(run.actions || []).length" class="text-xs text-gray-500">{{ t('admin.autopilot.noActions') }}</p>
              </div>
            </div>
            <EmptyState v-if="!runs.length" :title="t('admin.autopilot.noRuns')" :description="t('admin.autopilot.noRunsHint')" />
          </div>
        </div>
      </div>

      <!-- Trends -->
      <div v-else-if="activeTab === 'trends'" class="space-y-4">
        <div v-if="!history?.runs?.length" class="card p-8 text-center text-sm text-gray-500">
          {{ t('admin.autopilot.noTrendData') }}
        </div>
        <template v-else>
          <div class="card p-4">
            <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
              <div>
                <div class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.autopilot.pickAccounts') }}</div>
                <p class="text-xs text-gray-500">{{ t('admin.autopilot.pickAccountsHint') }}</p>
              </div>
              <div class="flex flex-wrap items-center gap-2">
                <span class="badge badge-gray">
                  {{ t('admin.autopilot.recentStats', { runs: history.runs.length, actions: history.actions.length }) }}
                </span>
                <button v-if="accountPickDirty" type="button" class="btn btn-secondary btn-sm" @click="resetAccountPick">
                  {{ t('admin.autopilot.resetDefault') }}
                </button>
              </div>
            </div>
            <div class="flex flex-wrap gap-2">
              <button
                v-for="acc in accountPickOptions"
                :key="acc.id"
                type="button"
                class="rounded-full border px-3 py-1 text-xs transition"
                :class="selectedAccountIds.includes(acc.id)
                  ? 'border-primary-400 bg-primary-50 text-primary-800 dark:border-primary-600 dark:bg-primary-900/30 dark:text-primary-200'
                  : 'border-gray-200 bg-white text-gray-600 hover:border-gray-300 dark:border-dark-600 dark:bg-dark-800 dark:text-gray-300'"
                @click="toggleAccountPick(acc.id)"
              >
                {{ shortName(acc.name) }}
                <span class="text-gray-400">#{{ acc.id }}</span>
              </button>
            </div>
          </div>

          <div class="card p-6">
            <h3 class="text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.autopilot.weightChartTitle') }}</h3>
            <p class="mt-1 text-xs leading-relaxed text-gray-500">{{ t('admin.autopilot.weightChartDesc') }}</p>
            <div v-if="weightChartData" class="mt-4 h-72">
              <Line :data="weightChartData" :options="stepLineOptions" />
            </div>
            <p v-else class="mt-6 text-center text-sm text-gray-500">{{ t('admin.autopilot.noWeightSeries') }}</p>
          </div>

          <div class="card p-6">
            <h3 class="text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.autopilot.priorityChartTitle') }}</h3>
            <p class="mt-1 text-xs leading-relaxed text-gray-500">{{ t('admin.autopilot.priorityChartDesc') }}</p>
            <div v-if="priorityChartData" class="mt-4 h-72">
              <Line :data="priorityChartData" :options="stepLineOptions" />
            </div>
            <p v-else class="mt-6 text-center text-sm text-gray-500">{{ t('admin.autopilot.noPrioritySeries') }}</p>
          </div>

          <div class="card p-6">
            <h3 class="text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.autopilot.outcomeChartTitle') }}</h3>
            <p class="mt-1 text-xs leading-relaxed text-gray-500">{{ t('admin.autopilot.outcomeChartDesc') }}</p>
            <div v-if="outcomeChartData" class="mt-4 h-64">
              <Bar :data="outcomeChartData" :options="barOptions" />
            </div>
          </div>
        </template>
      </div>

      <!-- Scores -->
      <div v-else-if="activeTab === 'scores'" class="space-y-4">
        <div class="card p-6">
          <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
            <div>
              <h3 class="text-base font-semibold">{{ t('admin.autopilot.scoresTitle') }}</h3>
              <p class="text-xs text-gray-500">{{ t('admin.autopilot.scoresHint') }}</p>
            </div>
            <label class="flex items-center gap-2 text-xs text-gray-600 dark:text-gray-300">
              <span>{{ t('admin.autopilot.sortBy') }}</span>
              <select v-model="scoreSort" class="input w-36 text-sm">
                <option value="overall">{{ t('admin.autopilot.colOverall') }}</option>
                <option value="stability">{{ t('admin.autopilot.colStability') }}</option>
                <option value="latency">{{ t('admin.autopilot.colLatency') }}</option>
                <option value="throughput">{{ t('admin.autopilot.colThroughput') }}</option>
                <option value="cost">{{ t('admin.autopilot.colCost') }}</option>
              </select>
            </label>
          </div>
          <div class="overflow-x-auto">
            <table class="min-w-full text-left text-sm">
              <thead class="border-b text-xs text-gray-500">
                <tr>
                  <th class="px-2 py-2">{{ t('admin.autopilot.colAccount') }}</th>
                  <th class="px-2 py-2">{{ t('admin.autopilot.colState') }}</th>
                  <th class="px-2 py-2">{{ t('admin.autopilot.colPriority') }}</th>
                  <th class="px-2 py-2">{{ t('admin.autopilot.colWeight') }}</th>
                  <th class="px-2 py-2">{{ t('admin.autopilot.colOverall') }}</th>
                  <th class="px-2 py-2">{{ t('admin.autopilot.colStability') }}</th>
                  <th class="px-2 py-2">{{ t('admin.autopilot.colLatency') }}</th>
                  <th class="px-2 py-2">{{ t('admin.autopilot.colThroughput') }}</th>
                  <th class="px-2 py-2">{{ t('admin.autopilot.colCost') }}</th>
                </tr>
              </thead>
              <tbody>
                <template v-for="row in sortedScores" :key="row.account_id">
                  <tr class="cursor-pointer border-b border-gray-50 hover:bg-gray-50 dark:border-dark-800 dark:hover:bg-dark-800" @click="toggleScore(row.account_id)">
                    <td class="px-2 py-2">
                      <div class="font-medium">{{ row.name }}</div>
                      <div class="text-xs text-gray-500">#{{ row.account_id }}</div>
                      <div
                        v-if="row.scored && row.score.note"
                        class="mt-0.5 max-w-xs truncate text-xs text-gray-400"
                        :title="row.score.note"
                      >{{ row.score.note }}</div>
                    </td>
                    <td class="px-2 py-2">
                      <div class="flex flex-wrap gap-1">
                        <span v-if="row.ai_disabled" class="badge badge-warning">AI停用</span>
                        <span v-if="row.rate_limited" class="badge badge-warning">限流</span>
                        <span v-if="!row.schedulable" class="badge badge-gray">不可调度</span>
                        <span v-if="!row.scored" class="badge badge-gray">未评分</span>
                        <span v-else-if="!row.ai_disabled && row.schedulable" class="badge badge-success">在跑</span>
                      </div>
                    </td>
                    <td class="px-2 py-2 tabular-nums">{{ row.priority }}</td>
                    <td class="px-2 py-2 tabular-nums">{{ row.weight }}</td>
                    <td class="px-2 py-2"><ScoreBar :value="row.scored ? row.score.overall : 0" :missing="!row.scored" /></td>
                    <td class="px-2 py-2"><ScoreBar :value="row.scored ? row.score.stability : 0" :missing="scoreDimMissing(row, '稳—')" /></td>
                    <td class="px-2 py-2"><ScoreBar :value="row.scored ? row.score.latency : 0" :missing="scoreDimMissing(row, '延迟—')" /></td>
                    <td class="px-2 py-2"><ScoreBar :value="row.scored ? row.score.throughput : 0" :missing="scoreDimMissing(row, '流畅—')" /></td>
                    <td class="px-2 py-2"><ScoreBar :value="row.scored ? row.score.cost : 0" :missing="!row.scored" /></td>
                  </tr>
                  <tr v-if="openScoreId === row.account_id">
                    <td colspan="9" class="bg-gray-50 px-3 py-3 dark:bg-dark-800">
                      <div v-if="scoreHistoryLoading" class="text-xs text-gray-500">{{ t('common.loading') }}</div>
                      <div v-else-if="scoreHistoryChart" class="h-48">
                        <Line :data="scoreHistoryChart" :options="lineOptions" />
                      </div>
                      <p v-else class="text-xs text-gray-500">{{ t('admin.autopilot.noScoreHistory') }}</p>
                      <p v-if="row.scored && row.score.note" class="mt-2 text-xs text-gray-600">{{ row.score.note }}</p>
                    </td>
                  </tr>
                </template>
              </tbody>
            </table>
            <p v-if="!scores.length" class="py-8 text-center text-sm text-gray-500">{{ t('admin.autopilot.noScores') }}</p>
          </div>
        </div>
      </div>

      <!-- Settings -->
      <div v-else-if="activeTab === 'settings' && settings" class="card p-6">
        <h3 class="mb-2 text-base font-semibold">{{ t('admin.autopilot.settingsTitle') }}</h3>
        <p class="mb-4 text-xs text-gray-500">{{ t('admin.autopilot.settingsUsedNote') }}</p>
        <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.applyMode') }}</label>
            <select v-model="settings.apply_mode" class="input w-full">
              <option value="suggest_only">{{ t('admin.autopilot.applyModeSuggest') }}</option>
              <option value="auto">{{ t('admin.autopilot.applyModeAuto') }}</option>
            </select>
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.model') }}</label>
            <input v-model="settings.model" class="input w-full" />
          </div>
          <div class="md:col-span-2">
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.baseUrl') }}</label>
            <input v-model="settings.base_url" class="input w-full" placeholder="http://claude-code-hub-app1:23001" />
            <p class="mt-1 text-xs text-gray-500">{{ t('admin.autopilot.baseUrlHint') }}</p>
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.apiKey') }}</label>
            <input v-model="settings.api_key" type="password" class="input w-full" :placeholder="t('admin.autopilot.apiKeyPlaceholder')" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.intervalMinutes') }}</label>
            <input v-model.number="settings.interval_minutes" type="number" min="1" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.timeoutSeconds') }}</label>
            <input v-model.number="settings.timeout_seconds" type="number" min="30" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.windowMinutes') }}</label>
            <input v-model.number="settings.window_minutes" type="number" min="5" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.recentWindowMinutes') }}</label>
            <input v-model.number="settings.recent_window_minutes" type="number" min="1" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.memoryRuns') }}</label>
            <input v-model.number="settings.memory_runs" type="number" min="1" max="100" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.minAvailable') }}</label>
            <input v-model.number="settings.min_available_per_group" type="number" min="0" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.maxActions') }}</label>
            <input v-model.number="settings.max_actions_per_run" type="number" min="1" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.cooldown') }}</label>
            <input v-model.number="settings.channel_cooldown_minutes" type="number" min="0" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.manualImmunity') }}</label>
            <input v-model.number="settings.manual_immunity_hours" type="number" min="0" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.confidence') }}</label>
            <input v-model.number="settings.confidence_threshold" type="number" min="0" max="1" step="0.05" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.weightPct') }}</label>
            <input v-model.number="settings.weight_max_delta_percent" type="number" min="0" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.weightAbs') }}</label>
            <input v-model.number="settings.weight_max_delta_abs" type="number" min="0" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.priorityDelta') }}</label>
            <input v-model.number="settings.priority_max_delta" type="number" min="0" class="input w-full" />
          </div>
          <div class="md:col-span-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
            <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
              <div>
                <h4 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.autopilot.scoreWeightsTitle') }}</h4>
                <p class="mt-0.5 text-xs text-gray-500">{{ t('admin.autopilot.scoreWeightsHint') }}</p>
              </div>
              <span
                class="text-xs font-mono tabular-nums"
                :class="scoreWeightSum === 100 ? 'text-emerald-600 dark:text-emerald-400' : 'text-amber-600 dark:text-amber-400'"
              >
                {{ t('admin.autopilot.scoreWeightsSum', { sum: scoreWeightSum }) }}
              </span>
            </div>
            <div class="grid grid-cols-2 gap-3 sm:grid-cols-4">
              <div>
                <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.colStability') }} %</label>
                <input v-model.number="settings.score_weight_stability" type="number" min="0" max="100" class="input w-full" />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.colLatency') }} %</label>
                <input v-model.number="settings.score_weight_latency" type="number" min="0" max="100" class="input w-full" />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.colThroughput') }} %</label>
                <input v-model.number="settings.score_weight_throughput" type="number" min="0" max="100" class="input w-full" />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.colCost') }} %</label>
                <input v-model.number="settings.score_weight_cost" type="number" min="0" max="100" class="input w-full" />
              </div>
            </div>
            <p class="mt-2 text-xs text-gray-500">{{ t('admin.autopilot.scoreWeightsExample') }}</p>
          </div>
          <div class="md:col-span-2">
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.managedGroups') }}</label>
            <input class="input w-full" :value="managedGroupsText" :placeholder="t('admin.autopilot.managedGroupsPlaceholder')" @change="onManagedGroupsChange" />
          </div>
          <label class="inline-flex items-center gap-2 text-sm md:col-span-2">
            <input v-model="settings.activation_probe_enabled" type="checkbox" class="rounded" />
            <span>{{ t('admin.autopilot.activationProbe') }}</span>
          </label>
          <div class="md:col-span-2">
            <label class="mb-1 block text-xs font-medium">{{ t('admin.autopilot.probeModels') }}</label>
            <input
              v-model="settings.activation_probe_models"
              class="input w-full"
              :placeholder="t('admin.autopilot.probeModelsPlaceholder')"
            />
            <p class="mt-1 text-xs text-gray-500">{{ t('admin.autopilot.probeModelsHint') }}</p>
          </div>
        </div>
        <h4 class="mb-2 mt-6 text-sm font-semibold">{{ t('admin.autopilot.sectionOps') }}</h4>
        <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
          <label v-for="item in opSwitches" :key="item.key" class="flex items-start gap-2 rounded-lg border px-3 py-2 text-sm dark:border-dark-700">
            <input type="checkbox" class="mt-0.5 rounded" :checked="opChecked(item.key)" @change="setOp(item.key, ($event.target as HTMLInputElement).checked)" />
            <span>
              <span class="font-medium">{{ item.label }} <code class="text-xs">({{ item.op }})</code></span>
              <span class="mt-0.5 block text-xs text-gray-500">{{ item.hint }}</span>
            </span>
          </label>
        </div>
        <div class="mt-4">
          <button type="button" class="btn btn-primary btn-sm" :disabled="saving" @click="saveSettings">
            {{ saving ? t('common.loading') : t('common.save') }}
          </button>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, defineComponent, h, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Chart as ChartJS,
  CategoryScale,
  LinearScale,
  PointElement,
  LineElement,
  BarElement,
  Title,
  Tooltip,
  Legend,
  Filler,
} from 'chart.js'
import { Line, Bar } from 'vue-chartjs'
import AppLayout from '@/components/layout/AppLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import Icon from '@/components/icons/Icon.vue'
import {
  AI_OP_SWITCHES,
  analyzeNow,
  approveAIAction,
  approveRunSuggestions,
  dismissAIAction,
  dismissRunSuggestions,
  getAIHistory,
  getAIScoreHistory,
  getAISettings,
  getAIStatus,
  listAIRuns,
  listAIScores,
  listAISuggestions,
  rollbackAIAction,
  updateAISettings,
  type AIAction,
  type AIAutopilotSettings,
  type AIHistory,
  type AIPilotStatus,
  type AIRun,
  type AIScoreRow,
} from '@/api/admin/ai'
import { buildStepSeries } from './autopilotTrends'

ChartJS.register(CategoryScale, LinearScale, PointElement, LineElement, BarElement, Title, Tooltip, Legend, Filler)

const ScoreBar = defineComponent({
  name: 'ScoreBar',
  props: {
    value: { type: Number, default: 0 },
    missing: { type: Boolean, default: false },
  },
  setup(props) {
    return () => {
      if (props.missing) {
        return h('span', { class: 'text-xs tabular-nums text-gray-400' }, '—')
      }
      return h('div', { class: 'flex items-center gap-2' }, [
        h('div', { class: 'h-1.5 w-14 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600' }, [
          h('div', {
            class: 'h-full rounded-full',
            style: {
              width: `${Math.max(0, Math.min(100, props.value))}%`,
              backgroundColor: scoreColor(props.value),
            },
          }),
        ]),
        h('span', { class: 'w-7 text-right text-xs tabular-nums text-gray-500' }, String(Math.round(props.value || 0))),
      ])
    }
  },
})

function scoreColor(v: number) {
  if (v >= 80) return 'hsl(142 71% 45%)'
  if (v >= 60) return 'hsl(80 60% 45%)'
  if (v >= 40) return 'hsl(38 92% 50%)'
  return 'hsl(0 84% 60%)'
}

function scoreDimMissing(row: { scored?: boolean; score?: { note?: string } }, token: string) {
  if (!row.scored) return true
  return typeof row.score?.note === 'string' && row.score.note.includes(token)
}

const { t } = useI18n()
const loading = ref(false)
const analyzing = ref(false)
const saving = ref(false)
const toggling = ref(false)
const status = ref<AIPilotStatus | null>(null)
const liveStatus = ref<AIPilotStatus | null>(null)
const nowTick = ref(0)
const runs = ref<AIRun[]>([])
const suggestions = ref<AIAction[]>([])
const settings = ref<AIAutopilotSettings | null>(null)
const history = ref<AIHistory | null>(null)
const scores = ref<AIScoreRow[]>([])
const error = ref('')
const message = ref('')
const activeTab = ref<'runs' | 'trends' | 'scores' | 'settings'>('runs')
const expandedRun = ref<number | null>(null)
const selectedAccountIds = ref<number[]>([])
const accountPickDirty = ref(false)
const scoreSort = ref<'overall' | 'stability' | 'latency' | 'throughput' | 'cost'>('overall')
const openScoreId = ref<number | null>(null)
const scoreHistoryLoading = ref(false)
const scoreHistoryChart = ref<any>(null)
let statusPollTimer: ReturnType<typeof setInterval> | null = null
let elapsedTimer: ReturnType<typeof setInterval> | null = null
let wasRunning = false

const opSwitches = AI_OP_SWITCHES
const tabs = computed(() => [
  { key: 'runs' as const, label: t('admin.autopilot.tabRuns') },
  { key: 'trends' as const, label: t('admin.autopilot.tabTrends') },
  { key: 'scores' as const, label: t('admin.autopilot.tabScores') },
])
const scheduleEnabled = computed(() => !!(settings.value?.enabled || status.value?.enabled || liveStatus.value?.enabled))
const managedGroupsText = computed(() => (settings.value?.managed_group_ids || []).join(','))

const accountPickOptions = computed(() => {
  if (!history.value) return []
  const touched = new Set(history.value.actions.filter((a) => a.state === 'applied').map((a) => a.account_id))
  return [...history.value.accounts].sort(
    (a, b) => Number(touched.has(b.id)) - Number(touched.has(a.id)) || a.id - b.id,
  )
})

const elapsedText = computed(() => {
  // depend on nowTick so the display updates every second
  void nowTick.value
  const s = liveStatus.value
  if (!s?.running || !s.started_at) return '0:00'
  const ms = Date.now() - s.started_at
  const total = Math.max(0, Math.floor(ms / 1000))
  const m = Math.floor(total / 60)
  const sec = total % 60
  return `${m}:${String(sec).padStart(2, '0')}`
})

function phaseLabel(phase: string) {
  const map: Record<string, string> = {
    collecting: t('admin.autopilot.phaseCollecting'),
    thinking: t('admin.autopilot.phaseThinking'),
    probing: t('admin.autopilot.phaseProbing'),
    applying: t('admin.autopilot.phaseApplying'),
  }
  return map[phase] || phase || '-'
}

function formatStartedAt(ms: number) {
  if (!ms) return '-'
  try {
    return new Date(ms).toLocaleString()
  } catch {
    return String(ms)
  }
}

function shortName(name: string) {
  if (!name) return '-'
  return name.length > 22 ? name.slice(0, 20) + '…' : name
}

function defaultSelectedAccountIds(hist: AIHistory): number[] {
  const touched = hist.accounts.filter((a) => hist.actions.some((x) => x.account_id === a.id && x.state === 'applied'))
  const pool = touched.length ? touched : hist.accounts
  return pool.slice(0, 6).map((a) => a.id)
}

function toggleAccountPick(id: number) {
  accountPickDirty.value = true
  const set = new Set(selectedAccountIds.value)
  if (set.has(id)) set.delete(id)
  else set.add(id)
  selectedAccountIds.value = [...set]
}

function resetAccountPick() {
  accountPickDirty.value = false
  if (history.value) selectedAccountIds.value = defaultSelectedAccountIds(history.value)
}

async function pollLiveStatus() {
  try {
    const s = await getAIStatus()
    liveStatus.value = s
    status.value = s
    if (wasRunning && !s.running) {
      // finished a run — refresh lists
      await refresh()
    }
    wasRunning = !!s.running
  } catch {
    /* next tick */
  }
}

const suggestionColumns = computed(() => [
  { key: 'account', label: t('admin.autopilot.colAccount') },
  { key: 'op', label: t('admin.autopilot.colOp') },
  { key: 'after', label: t('admin.autopilot.colValue') },
  { key: 'confidence', label: t('admin.autopilot.colConfidence') },
  { key: 'reason', label: t('admin.autopilot.colReason') },
  { key: 'actions', label: t('common.actions') },
])

const COLORS = ['#3b82f6', '#10b981', '#f59e0b', '#ef4444', '#8b5cf6', '#06b6d4', '#f97316', '#14b8a6'] // outcome chart colors

function runStatusClass(s: string) {
  if (s === 'ok') return 'badge-success'
  if (s === 'skipped') return 'badge-gray'
  return 'badge-warning'
}

function formatTs(ts: string) {
  try {
    return new Date(ts).toLocaleString()
  } catch {
    return ts
  }
}

type OpKey = (typeof AI_OP_SWITCHES)[number]['key']
function opChecked(key: OpKey) {
  if (!settings.value) return true
  return (settings.value as any)[key] !== false
}
function setOp(key: OpKey, checked: boolean) {
  if (!settings.value) return
  ;(settings.value as any)[key] = !!checked
}
const scoreWeightSum = computed(() => {
  if (!settings.value) return 0
  const s = settings.value
  return (
    (Number(s.score_weight_stability) || 0) +
    (Number(s.score_weight_latency) || 0) +
    (Number(s.score_weight_throughput) || 0) +
    (Number(s.score_weight_cost) || 0)
  )
})

function ensureScoreWeights(cfg: AIAutopilotSettings): AIAutopilotSettings {
  const next = { ...cfg }
  const s = Number(next.score_weight_stability)
  const l = Number(next.score_weight_latency)
  const th = Number(next.score_weight_throughput)
  const c = Number(next.score_weight_cost)
  if (!(s >= 0) || !(l >= 0) || !(th >= 0) || !(c >= 0) || s + l + th + c <= 0) {
    next.score_weight_stability = 40
    next.score_weight_latency = 30
    next.score_weight_throughput = 20
    next.score_weight_cost = 10
  }
  return next
}

function normalizeSettingsForSave(cfg: AIAutopilotSettings): AIAutopilotSettings {
  const next = ensureScoreWeights({ ...cfg })
  for (const item of opSwitches) {
    ;(next as any)[item.key] = (cfg as any)[item.key] !== false
  }
  next.enabled = !!next.enabled
  next.managed_group_ids = Array.isArray(next.managed_group_ids) ? next.managed_group_ids : []
  return next
}

const weightChartData = computed(() => {
  if (!history.value || !selectedAccountIds.value.length) return null
  const s = buildStepSeries(history.value, 'set_weight', selectedAccountIds.value, (a) => a.weight)
  // only show if something actually changed (not flat from first to now for all)
  const hasChange = s.datasets.some((ds) => {
    const first = ds.data[0]
    return ds.data.some((v) => v !== first)
  })
  if (!s.labels.length || !hasChange) return null
  return s
})

const priorityChartData = computed(() => {
  if (!history.value || !selectedAccountIds.value.length) return null
  const s = buildStepSeries(history.value, 'set_priority', selectedAccountIds.value, (a) => a.priority)
  const hasChange = s.datasets.some((ds) => {
    const first = ds.data[0]
    return ds.data.some((v) => v !== first)
  })
  if (!s.labels.length || !hasChange) return null
  return s
})

const outcomeChartData = computed(() => {
  if (!history.value?.runs?.length) return null
  const states: Array<{ key: string; label: string }> = [
    { key: 'applied', label: t('admin.autopilot.stateApplied') },
    { key: 'suggested', label: t('admin.autopilot.stateSuggested') },
    { key: 'rejected', label: t('admin.autopilot.stateRejected') },
    { key: 'dismissed', label: t('admin.autopilot.stateDismissed') },
    { key: 'rolled_back', label: t('admin.autopilot.stateRolledBack') },
  ]
  const labels = history.value.runs.map((r) => `#${r.id}`)
  const counts = new Map<string, number[]>()
  for (const st of states) counts.set(st.key, history.value.runs.map(() => 0))
  const runIndex = new Map(history.value.runs.map((r, i) => [r.id, i]))
  for (const a of history.value.actions) {
    const i = runIndex.get(a.run_id)
    if (i === undefined) continue
    const arr = counts.get(a.state)
    if (arr) arr[i]++
  }
  return {
    labels,
    datasets: states.map((st, i) => ({
      label: st.label,
      data: counts.get(st.key) || [],
      backgroundColor: COLORS[i % COLORS.length],
      stack: 's',
    })),
  }
})

const lineOptions = {
  responsive: true,
  maintainAspectRatio: false,
  interaction: { mode: 'index' as const, intersect: false },
  plugins: {
    legend: { position: 'bottom' as const, labels: { boxWidth: 12, font: { size: 11 } } },
    tooltip: { callbacks: {} },
  },
  scales: {
    x: { ticks: { maxRotation: 0, autoSkip: true, maxTicksLimit: 12, font: { size: 10 } }, grid: { display: false } },
    y: { beginAtZero: true, ticks: { font: { size: 10 } } },
  },
}
const stepLineOptions = {
  ...lineOptions,
  elements: { line: { borderWidth: 2 } },
}
const barOptions = {
  responsive: true,
  maintainAspectRatio: false,
  plugins: { legend: { position: 'bottom' as const, labels: { boxWidth: 12, font: { size: 11 } } } },
  scales: {
    x: { stacked: true, ticks: { font: { size: 10 } }, grid: { display: false } },
    y: { stacked: true, beginAtZero: true, ticks: { stepSize: 1, font: { size: 10 } } },
  },
}

const sortedScores = computed(() => {
  const rows = [...scores.value]
  const key = scoreSort.value
  rows.sort((a, b) => {
    const av = a.scored ? (a.score as any)[key] || 0 : -1
    const bv = b.scored ? (b.score as any)[key] || 0 : -1
    return bv - av
  })
  return rows
})

function onManagedGroupsChange(event: Event) {
  if (!settings.value) return
  const raw = (event.target as HTMLInputElement).value
  settings.value.managed_group_ids = raw
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
    .map(Number)
    .filter((n) => Number.isFinite(n) && n > 0)
}

function toggleRun(id: number) {
  expandedRun.value = expandedRun.value === id ? null : id
  if (expandedRun.value && !runs.value.find((r) => r.id === id)?.actions) {
    // load full run
    import('@/api/admin/ai').then(async ({ getAIRun }) => {
      try {
        const full = await getAIRun(id)
        const idx = runs.value.findIndex((r) => r.id === id)
        if (idx >= 0) runs.value[idx] = full
      } catch {
        /* ignore */
      }
    })
  }
}

async function toggleScore(id: number) {
  if (openScoreId.value === id) {
    openScoreId.value = null
    scoreHistoryChart.value = null
    return
  }
  openScoreId.value = id
  scoreHistoryLoading.value = true
  scoreHistoryChart.value = null
  try {
    const items = await getAIScoreHistory(id, 50)
    if (items.length >= 2) {
      scoreHistoryChart.value = {
        labels: items.map((i) => formatTs(i.ts)),
        datasets: [
          { label: t('admin.autopilot.colOverall'), data: items.map((i) => i.overall), borderColor: COLORS[0], tension: 0.2, pointRadius: 1 },
          { label: t('admin.autopilot.colStability'), data: items.map((i) => i.stability), borderColor: COLORS[1], tension: 0.2, pointRadius: 1 },
          { label: t('admin.autopilot.colLatency'), data: items.map((i) => i.latency), borderColor: COLORS[2], tension: 0.2, pointRadius: 1 },
          { label: t('admin.autopilot.colThroughput'), data: items.map((i) => i.throughput), borderColor: COLORS[3], tension: 0.2, pointRadius: 1 },
          { label: t('admin.autopilot.colCost'), data: items.map((i) => i.cost), borderColor: COLORS[4], tension: 0.2, pointRadius: 1 },
        ],
      }
    } else {
      scoreHistoryChart.value = null
    }
  } finally {
    scoreHistoryLoading.value = false
  }
}

async function refresh() {
  loading.value = true
  error.value = ''
  try {
    const [st, runList, sug, cfg, hist, sc] = await Promise.all([
      getAIStatus(),
      listAIRuns({ limit: 30 }),
      listAISuggestions(50),
      getAISettings(),
      getAIHistory(80).catch(() => null),
      listAIScores().catch(() => []),
    ])
    status.value = st
    runs.value = Array.isArray(runList) ? runList : []
    suggestions.value = Array.isArray(sug) ? sug : []
    settings.value = normalizeSettingsForSave(cfg)
    history.value = hist
    scores.value = sc || []
    if (hist?.accounts?.length && (!selectedAccountIds.value.length || !accountPickDirty.value)) {
      selectedAccountIds.value = defaultSelectedAccountIds(hist)
    }
  } catch (e: any) {
    error.value = e?.response?.data?.message || e?.message || String(e)
  } finally {
    loading.value = false
  }
}

async function onAnalyze() {
  analyzing.value = true
  error.value = ''
  message.value = ''
  try {
    const run = await analyzeNow()
    message.value = `${t('admin.autopilot.analyzeDone')} (#${run.id} ${run.status})`
    await refresh()
  } catch (e: any) {
    error.value = e?.response?.data?.message || e?.message || String(e)
  } finally {
    analyzing.value = false
  }
}

async function toggleSchedule() {
  if (!settings.value) return
  toggling.value = true
  error.value = ''
  try {
    const next = normalizeSettingsForSave({ ...settings.value, enabled: !settings.value.enabled })
    settings.value = normalizeSettingsForSave(await updateAISettings(next))
    message.value = settings.value.enabled ? t('admin.autopilot.masterOn') : t('admin.autopilot.masterOff')
    status.value = await getAIStatus()
  } catch (e: any) {
    error.value = e?.response?.data?.message || e?.message || String(e)
  } finally {
    toggling.value = false
  }
}

async function saveSettings() {
  if (!settings.value) return
  saving.value = true
  error.value = ''
  try {
    settings.value = normalizeSettingsForSave(await updateAISettings(normalizeSettingsForSave(settings.value)))
    message.value = t('admin.autopilot.settingsSaved')
    status.value = await getAIStatus()
  } catch (e: any) {
    error.value = e?.response?.data?.message || e?.message || String(e)
  } finally {
    saving.value = false
  }
}

async function onApprove(id: number) {
  try {
    await approveAIAction(id)
    message.value = t('admin.autopilot.approveDone')
    await refresh()
  } catch (e: any) {
    error.value = e?.response?.data?.message || e?.message || String(e)
  }
}
async function onDismiss(id: number) {
  try {
    await dismissAIAction(id)
    await refresh()
  } catch (e: any) {
    error.value = e?.response?.data?.message || e?.message || String(e)
  }
}
async function onRollback(id: number) {
  try {
    await rollbackAIAction(id)
    message.value = t('admin.autopilot.rollbackDone')
    await refresh()
  } catch (e: any) {
    error.value = e?.response?.data?.message || e?.message || String(e)
  }
}
async function onApproveRun(id: number) {
  try {
    const r = await approveRunSuggestions(id)
    message.value = `approve-all done=${r.done} failed=${r.failed}`
    await refresh()
  } catch (e: any) {
    error.value = e?.response?.data?.message || e?.message || String(e)
  }
}
async function onDismissRun(id: number) {
  try {
    const r = await dismissRunSuggestions(id)
    message.value = `dismiss-all done=${r.done} failed=${r.failed}`
    await refresh()
  } catch (e: any) {
    error.value = e?.response?.data?.message || e?.message || String(e)
  }
}

watch(activeTab, (tab) => {
  if (tab === 'trends' && !history.value) refresh()
  if (tab === 'scores' && !scores.value.length) refresh()
})

onMounted(() => {
  void refresh()
  void pollLiveStatus()
  statusPollTimer = setInterval(() => void pollLiveStatus(), 1000)
  elapsedTimer = setInterval(() => {
    nowTick.value++
  }, 1000)
})

onUnmounted(() => {
  if (statusPollTimer) {
    clearInterval(statusPollTimer)
    statusPollTimer = null
  }
  if (elapsedTimer) {
    clearInterval(elapsedTimer)
    elapsedTimer = null
  }
})
</script>
