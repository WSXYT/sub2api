<template>
  <section class="mb-4 border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-800">
    <header class="flex flex-wrap items-center justify-between gap-3 border-b border-gray-200 px-4 py-3 dark:border-dark-700">
      <div>
        <h2 class="text-sm font-semibold text-gray-900 dark:text-white">
          {{ t('admin.relayStations.accounts.title') }}
        </h2>
        <p class="mt-0.5 text-xs text-gray-500 dark:text-dark-300">
          {{ t('admin.relayStations.accounts.description') }}
        </p>
      </div>
      <button type="button" class="btn btn-secondary" :disabled="loading" @click="load">
        <Icon name="refresh" size="sm" :class="{ 'animate-spin': loading }" />
        <span>{{ t('admin.relayStations.accounts.refresh') }}</span>
      </button>
    </header>

    <div class="overflow-x-auto">
      <table class="w-full min-w-[760px] text-left text-sm">
        <thead class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-900 dark:text-dark-300">
          <tr>
            <th class="px-4 py-2.5 font-medium">{{ t('admin.relayStations.accounts.station') }}</th>
            <th class="px-4 py-2.5 font-medium">{{ t('admin.relayStations.accounts.group') }}</th>
            <th class="px-4 py-2.5 font-medium">{{ t('admin.relayStations.accounts.priority') }}</th>
            <th class="px-4 py-2.5 font-medium">{{ t('admin.relayStations.accounts.rate') }}</th>
            <th class="px-4 py-2.5 font-medium">{{ t('admin.relayStations.accounts.balance') }}</th>
            <th class="px-4 py-2.5 font-medium">{{ t('admin.relayStations.accounts.status') }}</th>
            <th class="px-4 py-2.5 font-medium"></th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
          <tr v-if="loading">
            <td colspan="7" class="px-4 py-6 text-center text-gray-500 dark:text-dark-300">
              {{ t('common.loading') }}
            </td>
          </tr>
          <tr v-else-if="accounts.length === 0">
            <td colspan="7" class="px-4 py-6 text-center text-gray-500 dark:text-dark-300">
              {{ t('admin.relayStations.accounts.empty') }}
            </td>
          </tr>
          <tr v-for="account in accounts" :key="accountKey(account)" class="text-gray-700 dark:text-dark-200">
            <td class="px-4 py-3">
              <div class="font-medium text-gray-900 dark:text-white">{{ account.station_name }}</div>
              <div class="mt-0.5 text-xs text-gray-500 dark:text-dark-300">{{ account.station_type }}</div>
            </td>
            <td class="px-4 py-3">
              <div>{{ account.group_name }}</div>
              <div v-if="account.station_type !== 'aihub'" class="mt-0.5 text-xs text-gray-500 dark:text-dark-300">
                {{ account.source_group }}
              </div>
            </td>
            <td class="px-4 py-3">
              <input
                :value="account.priority"
                type="number"
                min="-1000000"
                max="1000000"
                step="1"
                class="input w-24"
                :disabled="saving.has(accountKey(account))"
                :aria-label="`${t('admin.relayStations.accounts.priority')}: ${account.station_name}`"
                @change="updatePriority(account, $event)"
              />
            </td>
            <td class="px-4 py-3 font-mono">{{ formatRate(account.effective_rate) }}</td>
            <td class="px-4 py-3 font-mono">{{ formatBalance(account.balance) }}</td>
            <td class="px-4 py-3">
              <div class="flex items-center gap-2">
                <Toggle
                  :model-value="account.enabled"
                  :disabled="saving.has(accountKey(account))"
                  :aria-label="`${t('admin.relayStations.accounts.status')}: ${account.station_name}`"
                  @update:model-value="updateEnabled(account, $event)"
                />
                <span :class="['badge', statusClass(account.rate_status)]">{{ account.rate_status || '-' }}</span>
              </div>
            </td>
            <td class="px-4 py-3 text-right">
              <RouterLink class="btn btn-secondary" to="/admin/relay-stations">
                {{ t('admin.relayStations.accounts.manage') }}
              </RouterLink>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { RelayAccount } from '@/api/admin/relayStations'
import Icon from '@/components/icons/Icon.vue'
import Toggle from '@/components/common/Toggle.vue'
import { useAppStore } from '@/stores/app'

const { t } = useI18n()
const appStore = useAppStore()
const accounts = ref<RelayAccount[]>([])
const loading = ref(false)
const saving = ref(new Set<string>())

function accountKey(account: RelayAccount): string {
  return `${account.station_id}\u0000${account.group_id}\u0000${account.source_group}`
}

function formatRate(value: number | null | undefined): string {
  return typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(4)}x` : '-'
}

function formatBalance(value: number | null | undefined): string {
  return typeof value === 'number' && Number.isFinite(value) ? value.toFixed(4) : '-'
}

function statusClass(status: string): string {
  if (status === 'ready') return 'badge-success'
  if (status === 'unavailable' || status === 'unauthenticated') return 'badge-danger'
  return 'badge-gray'
}

function errorMessage(error: unknown): string {
  if (error && typeof error === 'object' && 'message' in error) {
    const message = (error as { message?: unknown }).message
    if (typeof message === 'string' && message) return message
  }
  return t('admin.relayStations.loadFailed')
}

async function load(): Promise<void> {
  loading.value = true
  try {
    accounts.value = await adminAPI.relayStations.listRelayAccounts()
  } catch (error) {
    appStore.showError(errorMessage(error))
  } finally {
    loading.value = false
  }
}

async function update(account: RelayAccount, updates: { enabled?: boolean; priority?: number }): Promise<void> {
  const key = accountKey(account)
  saving.value = new Set(saving.value).add(key)
  try {
    await adminAPI.relayStations.updateRelayAccount(account.station_id, {
      group_id: account.group_id,
      source_group: account.source_group,
      ...updates
    })
    await load()
  } catch (error) {
    appStore.showError(errorMessage(error))
  } finally {
    const next = new Set(saving.value)
    next.delete(key)
    saving.value = next
  }
}

function updateEnabled(account: RelayAccount, enabled: boolean): void {
  void update(account, { enabled })
}

function updatePriority(account: RelayAccount, event: Event): void {
  const priority = Number((event.target as HTMLInputElement).value)
  if (!Number.isInteger(priority)) {
    void load()
    return
  }
  void update(account, { priority })
}

onMounted(() => void load())
</script>
