<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="space-y-4">
          <div
            class="inline-flex w-full rounded-lg border border-gray-200 bg-white p-1 dark:border-dark-700 dark:bg-dark-800 sm:w-auto"
            role="group"
            :aria-label="t('admin.relayStations.title')"
          >
            <button
              type="button"
              :aria-pressed="activeTab === 'stations'"
              :class="tabClass('stations')"
              @click="activeTab = 'stations'"
            >
              <Icon name="server" size="sm" />
              {{ t('admin.relayStations.tabs.stations') }}
            </button>
            <button
              type="button"
              :aria-pressed="activeTab === 'bindings'"
              :class="tabClass('bindings')"
              @click="activeTab = 'bindings'"
            >
              <Icon name="link" size="sm" />
              {{ t('admin.relayStations.tabs.bindings') }}
            </button>
            <button
              type="button"
              :aria-pressed="activeTab === 'profit'"
              :class="tabClass('profit')"
              @click="activeTab = 'profit'"
            >
              <Icon name="chart" size="sm" />
              {{ t('admin.relayStations.tabs.profit') }}
            </button>
          </div>

          <div v-if="activeTab === 'stations'" class="flex flex-wrap justify-end gap-2">
            <button
              type="button"
              class="btn btn-secondary"
              :disabled="loading"
              :title="t('common.refresh')"
              @click="loadAll"
            >
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button type="button" class="btn btn-primary" @click="openCreateStation">
              <Icon name="plus" size="md" class="mr-2" />
              {{ t('admin.relayStations.addStation') }}
            </button>
          </div>

          <div
            v-else-if="activeTab === 'bindings'"
            class="flex flex-col gap-3 xl:flex-row xl:items-end"
          >
            <div class="w-full xl:max-w-sm">
              <label class="input-label mb-1.5 block">
                {{ t('admin.relayStations.binding.selectGroup') }}
              </label>
              <Select
                v-model="selectedGroupId"
                :options="groupOptions"
                searchable
                :placeholder="t('admin.relayStations.binding.selectGroup')"
                :aria-label="t('admin.relayStations.binding.selectGroup')"
              />
            </div>
            <div class="flex min-w-0 flex-1 flex-col gap-2 sm:flex-row sm:items-end">
              <div class="min-w-0 flex-1">
                <label class="input-label mb-1.5 block">
                  {{ t('admin.relayStations.binding.selectStation') }}
                </label>
                <Select
                  v-model="stationToAdd"
                  :options="availableStationOptions"
                  searchable
                  :disabled="!selectedGroupId || availableStationOptions.length === 0"
                  :placeholder="t('admin.relayStations.binding.selectStation')"
                  :aria-label="t('admin.relayStations.binding.selectStation')"
                />
              </div>
              <button
                type="button"
                class="btn btn-secondary"
                :disabled="!stationToAdd"
                @click="addSource"
              >
                <Icon name="plus" size="md" class="mr-2" />
                {{ t('admin.relayStations.binding.addSource') }}
              </button>
            </div>
            <div class="flex flex-wrap justify-end gap-2">
              <button
                type="button"
                class="btn btn-secondary"
                :disabled="refreshingRates"
                @click="() => refreshAllRates()"
              >
                <Icon name="refresh" size="md" :class="refreshingRates ? 'animate-spin' : ''" />
                <span class="ml-2">{{ t('admin.relayStations.refreshRates') }}</span>
              </button>
              <button
                type="button"
                class="btn btn-primary"
                :disabled="!bindingsDirty || savingBindings"
                @click="saveBindings"
              >
                <Icon name="check" size="md" class="mr-2" />
                {{ t('admin.relayStations.saveBindings') }}
              </button>
            </div>
          </div>

          <div v-else class="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-end">
            <div class="w-full sm:w-44">
              <label for="relay-profit-start" class="input-label mb-1.5 block">
                {{ t('admin.relayStations.profit.startDate') }}
              </label>
              <input id="relay-profit-start" v-model="profitStartDate" type="date" class="input" />
            </div>
            <div class="w-full sm:w-44">
              <label for="relay-profit-end" class="input-label mb-1.5 block">
                {{ t('admin.relayStations.profit.endDate') }}
              </label>
              <input id="relay-profit-end" v-model="profitEndDate" type="date" class="input" />
            </div>
            <button
              type="button"
              class="btn btn-secondary"
              :disabled="profitLoading"
              @click="loadProfit"
            >
              <Icon name="refresh" size="md" :class="profitLoading ? 'animate-spin' : ''" />
              <span class="ml-2">{{ t('admin.relayStations.profit.load') }}</span>
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <DataTable
          v-if="activeTab === 'stations'"
          :columns="stationColumns"
          :data="stations"
          :loading="loading"
          row-key="id"
          :actions-count="3"
        >
          <template #cell-name="{ row }">
            <div class="min-w-0">
              <p class="font-medium text-gray-900 dark:text-white">{{ row.name }}</p>
              <p class="mt-1 max-w-52 truncate font-mono text-xs text-gray-400" :title="row.id">
                {{ row.id }}
              </p>
            </div>
          </template>

          <template #cell-type="{ value }">
            <span :class="['badge', stationTypeClass(value)]">{{ stationTypeLabel(value) }}</span>
          </template>

          <template #cell-address="{ row }">
            <div class="max-w-md space-y-1 text-xs">
              <a
                :href="row.base_url"
                target="_blank"
                rel="noopener noreferrer"
                class="flex items-center gap-1 break-all text-primary-600 hover:underline dark:text-primary-400"
              >
                <Icon name="globe" size="xs" class="flex-shrink-0" />
                {{ row.base_url }}
              </a>
              <a
                :href="row.control_url"
                target="_blank"
                rel="noopener noreferrer"
                class="flex items-center gap-1 break-all text-gray-500 hover:underline dark:text-dark-300"
              >
                <Icon name="cog" size="xs" class="flex-shrink-0" />
                {{ row.control_url }}
              </a>
            </div>
          </template>

          <template #cell-enabled="{ value }">
            <StatusBadge
              :status="value ? 'active' : 'disabled'"
              :label="t(`admin.relayStations.status.${value ? 'enabled' : 'disabled'}`)"
            />
          </template>

          <template #cell-actions="{ row }">
            <div class="space-y-1.5">
              <div class="flex items-center gap-1">
                <button
                  type="button"
                  class="action-button hover:bg-emerald-50 hover:text-emerald-600 dark:hover:bg-emerald-900/20 dark:hover:text-emerald-400"
                  :disabled="testingStationIds.has(row.id)"
                  :title="t('admin.relayStations.testConnection')"
                  @click="testConnection(row)"
                >
                  <Icon
                    name="play"
                    size="sm"
                    :class="testingStationIds.has(row.id) ? 'animate-pulse' : ''"
                  />
                  <span>{{ t('admin.relayStations.testConnection') }}</span>
                </button>
                <button
                  type="button"
                  class="action-button hover:bg-gray-100 hover:text-primary-600 dark:hover:bg-dark-700 dark:hover:text-primary-400"
                  :title="t('common.edit')"
                  @click="openEditStation(row)"
                >
                  <Icon name="edit" size="sm" />
                  <span>{{ t('common.edit') }}</span>
                </button>
                <button
                  type="button"
                  class="action-button hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400"
                  :title="t('common.delete')"
                  @click="confirmDeleteStation(row)"
                >
                  <Icon name="trash" size="sm" />
                  <span>{{ t('common.delete') }}</span>
                </button>
              </div>
              <p
                v-if="testResults[row.id]"
                :class="testResults[row.id].success
                  ? 'text-emerald-600 dark:text-emerald-400'
                  : 'text-red-600 dark:text-red-400'"
                class="max-w-72 whitespace-normal text-xs"
                :title="testResults[row.id].message"
              >
                {{ testResults[row.id].message }}
              </p>
            </div>
          </template>

          <template #empty>
            <EmptyState
              :title="t('admin.relayStations.empty.title')"
              :description="t('admin.relayStations.empty.description')"
              :action-text="t('admin.relayStations.addStation')"
              @action="openCreateStation"
            />
          </template>
        </DataTable>

        <div v-else-if="activeTab === 'bindings'" class="flex h-full min-h-0 flex-col">
          <div v-if="loading" class="flex flex-1 items-center justify-center p-12">
            <LoadingSpinner />
          </div>
          <EmptyState
            v-else-if="groups.length === 0"
            :title="t('admin.relayStations.binding.noGroups')"
            :description="t('admin.relayStations.binding.noGroupsDescription')"
          />
          <EmptyState
            v-else-if="!selectedGroupId"
            :title="t('admin.relayStations.binding.selectGroup')"
          />
          <DataTable
            v-else
            :columns="bindingColumns"
            :data="selectedSources"
            :loading="false"
            row-key="station_id"
            :actions-count="1"
          >
            <template #cell-station="{ row }">
              <div>
                <p class="font-medium text-gray-900 dark:text-white">
                  {{ stationById(row.station_id)?.name || row.station_id }}
                </p>
                <span
                  v-if="stationById(row.station_id)"
                  :class="['badge mt-1', stationTypeClass(stationById(row.station_id)?.type)]"
                >
                  {{ stationTypeLabel(stationById(row.station_id)?.type) }}
                </span>
              </div>
            </template>

            <template #cell-source_group="{ row }">
              <input
                v-model="row.source_group"
                type="text"
                maxlength="100"
                class="input min-w-40"
                :aria-label="bindingControlLabel('sourceGroup', row)"
                :placeholder="t('admin.relayStations.binding.sourceGroupPlaceholder')"
                @input="bindingsDirty = true"
              />
            </template>

            <template #cell-enabled="{ row }">
              <Toggle
                :model-value="row.enabled"
                :aria-label="bindingControlLabel('enabled', row)"
                @update:model-value="updateSourceEnabled(row, $event)"
              />
            </template>

            <template #cell-delta="{ row }">
              <input
                v-model.number="row.delta"
                type="number"
                step="0.0001"
                class="input w-28"
                :aria-label="bindingControlLabel('delta', row)"
                @input="bindingsDirty = true"
              />
            </template>

            <template #cell-rate="{ row }">
              <div class="space-y-1">
                <p class="font-mono font-medium text-gray-900 dark:text-white">
                  {{ formatRate(rateForSource(row)?.rate) }}
                </p>
                <span :class="['badge', rateStatusClass(rateForSource(row)?.status)]">
                  {{ rateStatusLabel(rateForSource(row)?.status) }}
                </span>
              </div>
            </template>

            <template #cell-effective_rate="{ row }">
              <div class="flex items-center gap-2">
                <span class="font-mono font-semibold text-gray-900 dark:text-white">
                  {{ formatRate(sourceEffectiveRate(row)) }}
                </span>
                <span v-if="isLowestSource(row)" class="badge badge-success">
                  {{ t('admin.relayStations.binding.lowest') }}
                </span>
              </div>
            </template>

            <template #cell-updated_at="{ row }">
              <span class="text-xs text-gray-500 dark:text-dark-300">
                {{ formatDateTime(rateForSource(row)?.updated_at) }}
              </span>
            </template>

            <template #cell-actions="{ row }">
              <button
                type="button"
                class="action-button hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400"
                :title="t('admin.relayStations.binding.removeSource')"
                @click="removeSource(row)"
              >
                <Icon name="trash" size="sm" />
                <span>{{ t('admin.relayStations.binding.removeSource') }}</span>
              </button>
            </template>

            <template #empty>
              <EmptyState
                :title="t('admin.relayStations.binding.noSources')"
                :description="t('admin.relayStations.binding.noSourcesDescription')"
              />
            </template>
          </DataTable>
        </div>

        <div v-else class="flex h-full min-h-0 flex-col">
          <div v-if="profitUnavailable" class="flex flex-1 items-center justify-center p-6">
            <EmptyState
              :title="t('admin.relayStations.profit.unavailable')"
              :description="t('admin.relayStations.profit.unavailableDescription')"
            />
          </div>
          <DataTable
            v-else
            :columns="profitColumns"
            :data="profitEstimates"
            :loading="profitLoading"
            :row-key="profitRowKey"
          >
            <template #cell-group_name="{ row }">
              <div>
                <p class="font-medium text-gray-900 dark:text-white">{{ row.group_name }}</p>
                <p class="mt-1 text-xs text-gray-400">#{{ row.group_id }}</p>
              </div>
            </template>
            <template #cell-station_name="{ row }">
              <div>
                <p class="font-medium text-gray-900 dark:text-white">{{ row.station_name }}</p>
                <p class="mt-1 text-xs text-gray-500 dark:text-dark-300">{{ row.source_group }}</p>
              </div>
            </template>
            <template #cell-rate_status="{ value }">
              <span :class="['badge', rateStatusClass(value)]">{{ rateStatusLabel(value) }}</span>
            </template>
            <template #cell-requests="{ value }">{{ formatInteger(value) }}</template>
            <template #cell-total_cost="{ value }">{{ formatCurrency(value) }}</template>
            <template #cell-downstream_rate="{ value }">{{ formatRate(value) }}</template>
            <template #cell-upstream_rate="{ value }">{{ formatRate(value) }}</template>
            <template #cell-estimated_revenue="{ value }">{{ formatCurrency(value) }}</template>
            <template #cell-estimated_cost="{ value }">{{ formatCurrency(value) }}</template>
            <template #cell-estimated_profit="{ value }">
              <span :class="profitValueClass(value)" class="font-semibold">
                {{ formatCurrency(value) }}
              </span>
            </template>
            <template #empty>
              <EmptyState
                :title="t('admin.relayStations.profit.noData')"
                :description="t('admin.relayStations.profit.noDataDescription')"
              />
            </template>
          </DataTable>
        </div>
      </template>
    </TablePageLayout>

    <BaseDialog
      :show="showStationDialog"
      :title="editingStation ? t('admin.relayStations.form.editTitle') : t('admin.relayStations.form.createTitle')"
      width="wide"
      @close="closeStationDialog"
    >
      <form id="relay-station-form" class="space-y-5" @submit.prevent="saveStation">
        <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <label for="relay-station-name" class="input-label">
              {{ t('admin.relayStations.form.name') }} <span class="text-red-500">*</span>
            </label>
            <input
              id="relay-station-name"
              v-model="stationForm.name"
              type="text"
              maxlength="100"
              required
              class="input"
              :placeholder="t('admin.relayStations.form.namePlaceholder')"
            />
          </div>
          <div>
            <label class="input-label">
              {{ t('admin.relayStations.form.type') }} <span class="text-red-500">*</span>
            </label>
            <Select
              v-model="stationForm.type"
              :options="stationTypeOptions"
              :disabled="!!editingStation"
              :aria-label="t('admin.relayStations.form.type')"
            />
          </div>
        </div>

        <div v-if="showConnectionFields" class="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <label for="relay-station-base-url" class="input-label">
              {{ t('admin.relayStations.form.baseUrl') }} <span class="text-red-500">*</span>
            </label>
            <input
              id="relay-station-base-url"
              v-model="stationForm.base_url"
              type="url"
              maxlength="2048"
              required
              class="input"
              :placeholder="t('admin.relayStations.form.baseUrlPlaceholder')"
            />
          </div>
          <div>
            <label for="relay-station-control-url" class="input-label">
              {{ t('admin.relayStations.form.controlUrl') }} <span class="text-red-500">*</span>
            </label>
            <input
              id="relay-station-control-url"
              v-model="stationForm.control_url"
              type="url"
              maxlength="2048"
              required
              class="input"
              :placeholder="t('admin.relayStations.form.controlUrlPlaceholder')"
            />
          </div>
        </div>

        <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <div class="mb-1.5 flex items-center gap-2">
              <label for="relay-station-username" class="input-label mb-0">
                {{ accountLabel }}
                <span v-if="loginCredentialsRequired" class="text-red-500">*</span>
              </label>
              <span v-if="credentialConfigured('username')" class="badge badge-success">
                {{ t('admin.relayStations.status.configured') }}
              </span>
            </div>
            <input
              id="relay-station-username"
              v-model="stationForm.username"
              :type="stationForm.type === 'aihub' ? 'email' : 'text'"
              maxlength="512"
              :required="loginCredentialsRequired"
              autocomplete="username"
              class="input"
              :placeholder="accountPlaceholder"
            />
            <p v-if="credentialConfigured('username')" class="input-hint mt-1.5">
              {{ t('admin.relayStations.form.leaveBlank') }}
            </p>
          </div>
          <div>
            <div class="mb-1.5 flex items-center gap-2">
              <label for="relay-station-password" class="input-label mb-0">
                {{ t('admin.relayStations.form.password') }}
                <span v-if="loginPasswordRequired" class="text-red-500">*</span>
              </label>
              <span v-if="credentialConfigured('password')" class="badge badge-success">
                {{ t('admin.relayStations.status.configured') }}
              </span>
            </div>
            <input
              id="relay-station-password"
              v-model="stationForm.password"
              type="password"
              maxlength="4096"
              :required="loginPasswordRequired"
              autocomplete="new-password"
              class="input"
              :placeholder="t('admin.relayStations.form.passwordPlaceholder')"
            />
            <p v-if="credentialConfigured('password')" class="input-hint mt-1.5">
              {{ t('admin.relayStations.form.leaveBlank') }}
            </p>
          </div>
        </div>

        <div v-if="showLegacySecrets" class="border-t border-gray-200 pt-5 dark:border-dark-700">
          <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
            <div v-if="showUIPasswordField">
              <div class="mb-1.5 flex items-center gap-2">
                <label for="relay-station-ui-password" class="input-label mb-0">
                  {{ t('admin.relayStations.form.uiPassword') }}
                </label>
                <span v-if="credentialConfigured('ui_password')" class="badge badge-success">
                  {{ t('admin.relayStations.status.configured') }}
                </span>
              </div>
              <input
                id="relay-station-ui-password"
                v-model="stationForm.ui_password"
                type="password"
                maxlength="2048"
                autocomplete="new-password"
                class="input"
              />
              <p v-if="credentialConfigured('ui_password')" class="input-hint mt-1.5">
                {{ t('admin.relayStations.form.leaveBlank') }}
              </p>
            </div>
            <div v-if="showProxyTokenField">
              <div class="mb-1.5 flex items-center gap-2">
                <label for="relay-station-proxy-token" class="input-label mb-0">
                  {{ t('admin.relayStations.form.proxyToken') }}
                </label>
                <span v-if="credentialConfigured('proxy_token')" class="badge badge-success">
                  {{ t('admin.relayStations.status.configured') }}
                </span>
              </div>
              <input
                id="relay-station-proxy-token"
                v-model="stationForm.proxy_token"
                type="password"
                maxlength="4096"
                autocomplete="new-password"
                class="input"
              />
              <p v-if="credentialConfigured('proxy_token')" class="input-hint mt-1.5">
                {{ t('admin.relayStations.form.leaveBlank') }}
              </p>
            </div>
          </div>
        </div>

        <label
          v-if="editingStation"
          class="flex items-center justify-between gap-4 border-t border-gray-200 pt-5 dark:border-dark-700"
        >
          <span class="font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.relayStations.form.enabled') }}
          </span>
          <Toggle v-model="stationForm.enabled" />
        </label>
      </form>

      <template #footer>
        <div class="flex justify-end gap-3">
          <button type="button" class="btn btn-secondary" @click="closeStationDialog">
            {{ t('common.cancel') }}
          </button>
          <button
            type="submit"
            form="relay-station-form"
            class="btn btn-primary"
            :disabled="savingStation"
          >
            {{ savingStation ? t('common.loading') : t('common.save') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <ConfirmDialog
      :show="!!stationToDelete"
      :title="t('admin.relayStations.deleteTitle')"
      :message="t('admin.relayStations.deleteMessage', { name: stationToDelete?.name || '' })"
      :confirm-text="t('common.delete')"
      danger
      @confirm="deleteSelectedStation"
      @cancel="stationToDelete = null"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import {
  effectiveRelayRate,
  relayErrorReason,
  type RelayGroupBinding,
  type RelayProfitEstimate,
  type RelayRate,
  type RelayRateStatus,
  type RelayStation,
  type RelayStationCreateInput,
  type RelayStationCredentials,
  type RelayStationSource,
  type RelayStationType,
  type RelayStationUpdateInput
} from '@/api/admin/relayStations'
import type { AdminGroup } from '@/types'
import type { Column } from '@/components/common/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import DataTable from '@/components/common/DataTable.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import Select from '@/components/common/Select.vue'
import StatusBadge from '@/components/common/StatusBadge.vue'
import Toggle from '@/components/common/Toggle.vue'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores/app'

const { locale, t } = useI18n()
const appStore = useAppStore()

type ActiveTab = 'stations' | 'bindings' | 'profit'
type CredentialField = keyof RelayStationCredentials

interface StationForm {
  type: RelayStationType
  name: string
  base_url: string
  control_url: string
  ui_password: string
  proxy_token: string
  username: string
  password: string
  enabled: boolean
}

interface TestResult {
  success: boolean
  message: string
}

const activeTab = ref<ActiveTab>('stations')
const loading = ref(true)
const stations = ref<RelayStation[]>([])
const groups = ref<AdminGroup[]>([])
const bindings = ref<RelayGroupBinding[]>([])
const rates = ref<RelayRate[]>([])
const selectedGroupId = ref<number | null>(null)
const stationToAdd = ref<string | null>(null)
const bindingsDirty = ref(false)
const savingBindings = ref(false)
const refreshingRates = ref(false)

const showStationDialog = ref(false)
const editingStation = ref<RelayStation | null>(null)
const stationToDelete = ref<RelayStation | null>(null)
const savingStation = ref(false)
const testingStationIds = ref(new Set<string>())
const testResults = reactive<Record<string, TestResult>>({})

const today = new Date()
const thirtyDaysAgo = new Date(today)
thirtyDaysAgo.setUTCDate(thirtyDaysAgo.getUTCDate() - 29)
const profitStartDate = ref(thirtyDaysAgo.toISOString().slice(0, 10))
const profitEndDate = ref(today.toISOString().slice(0, 10))
const profitEstimates = ref<RelayProfitEstimate[]>([])
const profitLoading = ref(false)
const profitLoaded = ref(false)
const profitUnavailable = ref(false)

const stationForm = reactive<StationForm>({
  type: 'aihub',
  name: '',
  base_url: '',
  control_url: '',
  ui_password: '',
  proxy_token: '',
  username: '',
  password: '',
  enabled: true
})

const stationColumns = computed<Column[]>(() => [
  { key: 'name', label: t('admin.relayStations.columns.name'), sortable: true },
  { key: 'type', label: t('admin.relayStations.columns.type'), sortable: true },
  { key: 'address', label: t('admin.relayStations.columns.address') },
  { key: 'enabled', label: t('admin.relayStations.columns.status'), sortable: true },
  { key: 'actions', label: t('admin.relayStations.columns.actions') }
])

const bindingColumns = computed<Column[]>(() => [
  { key: 'station', label: t('admin.relayStations.columns.station') },
  { key: 'source_group', label: t('admin.relayStations.columns.sourceGroup') },
  { key: 'enabled', label: t('admin.relayStations.columns.enabled') },
  { key: 'delta', label: t('admin.relayStations.columns.delta') },
  { key: 'rate', label: t('admin.relayStations.columns.rate') },
  { key: 'effective_rate', label: t('admin.relayStations.columns.effectiveRate') },
  { key: 'updated_at', label: t('admin.relayStations.columns.updatedAt') },
  { key: 'actions', label: t('admin.relayStations.columns.actions') }
])

const profitColumns = computed<Column[]>(() => [
  { key: 'group_name', label: t('admin.relayStations.columns.group'), sortable: true },
  { key: 'station_name', label: t('admin.relayStations.columns.station'), sortable: true },
  { key: 'rate_status', label: t('admin.relayStations.columns.status') },
  { key: 'requests', label: t('admin.relayStations.columns.requests'), sortable: true },
  { key: 'total_cost', label: t('admin.relayStations.columns.totalCost'), sortable: true },
  { key: 'downstream_rate', label: t('admin.relayStations.columns.downstreamRate') },
  { key: 'upstream_rate', label: t('admin.relayStations.columns.upstreamRate') },
  { key: 'estimated_revenue', label: t('admin.relayStations.columns.revenue') },
  { key: 'estimated_cost', label: t('admin.relayStations.columns.cost') },
  { key: 'estimated_profit', label: t('admin.relayStations.columns.profit'), sortable: true }
])

const stationTypeOptions = computed(() =>
  (['aihub', 'newapi', 'sub2api'] as RelayStationType[]).map((type) => ({
    value: type,
    label: stationTypeLabel(type)
  }))
)

const groupOptions = computed(() =>
  groups.value.map((group) => ({
    value: group.id,
    label: `${group.name} (${group.platform}, ${t(`admin.relayStations.status.${group.status}`)})`
  }))
)

const selectedBinding = computed(() =>
  bindings.value.find((binding) => binding.group_id === selectedGroupId.value)
)
const selectedSources = computed(() => selectedBinding.value?.sources ?? [])

const availableStationOptions = computed(() => {
  const used = new Set(selectedSources.value.map((source) => source.station_id))
  return stations.value
    .filter((station) => !used.has(station.id))
    .map((station) => ({
      value: station.id,
      label: `${station.name} (${stationTypeLabel(station.type)})`
    }))
})

const accountLabel = computed(() =>
  t(stationForm.type === 'aihub'
    ? 'admin.relayStations.form.aihubEmail'
    : 'admin.relayStations.form.username')
)
const accountPlaceholder = computed(() =>
  t(stationForm.type === 'aihub'
    ? 'admin.relayStations.form.aihubEmailPlaceholder'
    : 'admin.relayStations.form.usernamePlaceholder')
)
const showConnectionFields = computed(
  () => stationForm.type !== 'aihub' || !!editingStation.value
)
const showUIPasswordField = computed(
  () =>
    !!editingStation.value &&
    (stationForm.type === 'aihub' || credentialConfigured('ui_password'))
)
const showProxyTokenField = computed(
  () =>
    !!editingStation.value &&
    (stationForm.type === 'aihub' || credentialConfigured('proxy_token'))
)
const showLegacySecrets = computed(
  () => showUIPasswordField.value || showProxyTokenField.value
)
const loginCredentialsRequired = computed(
  () => !editingStation.value ||
    (stationForm.type !== 'aihub' && !credentialConfigured('username'))
)
const loginPasswordRequired = computed(
  () => !editingStation.value ||
    (stationForm.type !== 'aihub' && !credentialConfigured('password'))
)

const lowestSourceKey = computed(() => {
  let best: { key: string; value: number; stationId: string } | null = null
  for (const source of selectedSources.value) {
    if (!source.enabled || !stationById(source.station_id)?.enabled) continue
    const value = sourceEffectiveRate(source)
    if (value === null) continue
    if (
      !best ||
      value < best.value ||
      (value === best.value && source.station_id < best.stationId)
    ) {
      best = { key: sourceKey(source), value, stationId: source.station_id }
    }
  }
  return best?.key ?? null
})

function tabClass(tab: ActiveTab): string[] {
  return [
    'flex min-h-10 flex-1 items-center justify-center gap-2 rounded-md px-4 py-2 text-sm font-medium transition-colors sm:flex-none',
    activeTab.value === tab
      ? 'bg-primary-600 text-white shadow-sm'
      : 'text-gray-600 hover:bg-gray-100 dark:text-dark-300 dark:hover:bg-dark-700'
  ]
}

function stationTypeLabel(type?: RelayStationType): string {
  return type ? t(`admin.relayStations.types.${type}`) : '-'
}

function stationTypeClass(type?: RelayStationType): string {
  if (type === 'aihub') return 'badge-primary'
  if (type === 'newapi') return 'badge-success'
  return 'badge-gray'
}

function rateStatusLabel(status?: RelayRateStatus): string {
  return t(`admin.relayStations.status.${status || 'unknown'}`)
}

function bindingControlLabel(
  key: 'sourceGroup' | 'enabled' | 'delta',
  source: RelayStationSource
): string {
  return `${t(`admin.relayStations.columns.${key}`)}: ${stationById(source.station_id)?.name || source.station_id}`
}

function rateStatusClass(status?: RelayRateStatus): string {
  if (status === 'ready') return 'badge-success'
  if (status === 'unauthenticated') return 'badge-danger'
  if (status === 'stale') return 'badge-warning'
  return 'badge-gray'
}

function stationById(id: string): RelayStation | undefined {
  return stations.value.find((station) => station.id === id)
}

function sourceKey(source: RelayStationSource): string {
  return `${source.station_id}\u0000${source.source_group.trim() || 'default'}`
}

function rateForSource(source: RelayStationSource): RelayRate | undefined {
  const group = source.source_group.trim() || 'default'
  return rates.value.find(
    (rate) => rate.station_id === source.station_id && rate.source_group === group
  )
}

function sourceEffectiveRate(source: RelayStationSource): number | null {
  return effectiveRelayRate(rateForSource(source), source.delta)
}

function isLowestSource(source: RelayStationSource): boolean {
  return lowestSourceKey.value === sourceKey(source)
}

function formatRate(value: number | null | undefined): string {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '-'
  return `${new Intl.NumberFormat(locale.value, { maximumFractionDigits: 6 }).format(value)}x`
}

function formatCurrency(value: number | null | undefined): string {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '-'
  return new Intl.NumberFormat(locale.value, {
    style: 'currency',
    currency: 'USD',
    maximumFractionDigits: 4
  }).format(value)
}

function formatInteger(value: number): string {
  return new Intl.NumberFormat(locale.value, { maximumFractionDigits: 0 }).format(value || 0)
}

function formatDateTime(value?: string): string {
  if (!value || value.startsWith('0001-')) return '-'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '-' : new Intl.DateTimeFormat(locale.value, {
    dateStyle: 'short',
    timeStyle: 'short'
  }).format(date)
}

function profitValueClass(value: number | null | undefined): string {
  if (typeof value !== 'number') return 'text-gray-400'
  return value >= 0 ? 'text-emerald-600 dark:text-emerald-400' : 'text-red-600 dark:text-red-400'
}

function profitRowKey(row: RelayProfitEstimate): string {
  return `${row.group_id}:${row.station_id}:${row.source_group}`
}

function errorMessage(error: unknown, fallback: string): string {
  if (error && typeof error === 'object' && 'message' in error) {
    const message = (error as { message?: unknown }).message
    if (typeof message === 'string' && message.trim()) return message
  }
  return fallback
}

async function loadAll(): Promise<void> {
  loading.value = true
  try {
    const [nextStations, nextGroups, bindingResult, rateResult] = await Promise.all([
      adminAPI.relayStations.list(),
      adminAPI.groups.getAllIncludingInactive(),
      adminAPI.relayStations.listBindings(),
      adminAPI.relayStations.listRates()
    ])
    stations.value = nextStations
    groups.value = nextGroups
    bindings.value = bindingResult.bindings
    rates.value = rateResult.rates
    bindingsDirty.value = false

    if (!groups.value.some((group) => group.id === selectedGroupId.value)) {
      const firstBoundGroup = bindings.value.find((binding) =>
        groups.value.some((group) => group.id === binding.group_id)
      )
      selectedGroupId.value = firstBoundGroup?.group_id ?? groups.value[0]?.id ?? null
    }
  } catch (error) {
    appStore.showError(errorMessage(error, t('admin.relayStations.loadFailed')))
  } finally {
    loading.value = false
  }
}

function resetStationForm(): void {
  Object.assign(stationForm, {
    type: 'aihub',
    name: '',
    base_url: '',
    control_url: '',
    ui_password: '',
    proxy_token: '',
    username: '',
    password: '',
    enabled: true
  })
}

function openCreateStation(): void {
  editingStation.value = null
  resetStationForm()
  showStationDialog.value = true
}

function openEditStation(station: RelayStation): void {
  editingStation.value = station
  Object.assign(stationForm, {
    type: station.type,
    name: station.name,
    base_url: station.base_url,
    control_url: station.control_url,
    ui_password: '',
    proxy_token: '',
    username: '',
    password: '',
    enabled: station.enabled
  })
  showStationDialog.value = true
}

function closeStationDialog(): void {
  showStationDialog.value = false
  editingStation.value = null
  resetStationForm()
}

function credentialConfigured(field: CredentialField): boolean {
  return editingStation.value?.credentials[field] === true
}

async function saveStation(): Promise<void> {
  savingStation.value = true
  try {
    if (editingStation.value) {
      const payload: RelayStationUpdateInput = {
        name: stationForm.name.trim(),
        username: stationForm.username,
        password: stationForm.password,
        enabled: stationForm.enabled
      }
      if (showConnectionFields.value) {
        payload.base_url = stationForm.base_url.trim()
        payload.control_url = stationForm.control_url.trim()
      }
      if (showUIPasswordField.value) payload.ui_password = stationForm.ui_password
      if (showProxyTokenField.value) payload.proxy_token = stationForm.proxy_token
      await adminAPI.relayStations.update(editingStation.value.id, payload)
      appStore.showSuccess(t('admin.relayStations.updateSuccess'))
    } else {
      const payload: RelayStationCreateInput = {
        type: stationForm.type,
        name: stationForm.name.trim(),
        username: stationForm.username.trim(),
        password: stationForm.password.trim(),
        enabled: true
      }
      if (stationForm.type !== 'aihub') {
        payload.base_url = stationForm.base_url.trim()
        payload.control_url = stationForm.control_url.trim()
      }
      await adminAPI.relayStations.create(payload)
      appStore.showSuccess(t('admin.relayStations.createSuccess'))
    }
    closeStationDialog()
    await loadAll()
  } catch (error) {
    appStore.showError(errorMessage(error, t('admin.relayStations.saveFailed')))
  } finally {
    savingStation.value = false
  }
}

function confirmDeleteStation(station: RelayStation): void {
  stationToDelete.value = station
}

async function deleteSelectedStation(): Promise<void> {
  const station = stationToDelete.value
  if (!station) return
  stationToDelete.value = null
  try {
    await adminAPI.relayStations.delete(station.id)
    delete testResults[station.id]
    appStore.showSuccess(t('admin.relayStations.deleteSuccess'))
    await loadAll()
  } catch (error) {
    appStore.showError(errorMessage(error, t('admin.relayStations.deleteFailed')))
  }
}

function mergeRates(nextRates: RelayRate[]): void {
  const merged = new Map(rates.value.map((rate) => [`${rate.station_id}\u0000${rate.source_group}`, rate]))
  for (const rate of nextRates) merged.set(`${rate.station_id}\u0000${rate.source_group}`, rate)
  rates.value = Array.from(merged.values())
}

async function testConnection(station: RelayStation): Promise<void> {
  testingStationIds.value = new Set(testingStationIds.value).add(station.id)
  try {
    const result = await adminAPI.relayStations.test(station.id)
    mergeRates(result.rates)
    const message = result.rates.length
      ? t('admin.relayStations.testSuccessWithRates', { count: result.rates.length })
      : t('admin.relayStations.testSuccess')
    testResults[station.id] = { success: true, message }
    appStore.showSuccess(message)
  } catch (error) {
    const reason = errorMessage(error, t('admin.relayStations.testFailed'))
    const message = t('admin.relayStations.testFailedWithReason', { reason })
    testResults[station.id] = { success: false, message }
    appStore.showError(message)
  } finally {
    const next = new Set(testingStationIds.value)
    next.delete(station.id)
    testingStationIds.value = next
  }
}

function ensureSelectedBinding(): RelayGroupBinding | null {
  if (!selectedGroupId.value) return null
  let binding = bindings.value.find((item) => item.group_id === selectedGroupId.value)
  if (!binding) {
    binding = { group_id: selectedGroupId.value, sources: [] }
    bindings.value.push(binding)
  }
  return binding
}

function addSource(): void {
  if (!stationToAdd.value) return
  const binding = ensureSelectedBinding()
  if (!binding || binding.sources.some((source) => source.station_id === stationToAdd.value)) return
  binding.sources.push({
    station_id: stationToAdd.value,
    enabled: true,
    source_group: 'default',
    delta: 0
  })
  stationToAdd.value = null
  bindingsDirty.value = true
}

function removeSource(source: RelayStationSource): void {
  const binding = selectedBinding.value
  if (!binding) return
  binding.sources = binding.sources.filter((item) => item !== source)
  bindingsDirty.value = true
}

function updateSourceEnabled(source: RelayStationSource, enabled: boolean): void {
  source.enabled = enabled
  bindingsDirty.value = true
}

function hasControlCharacter(value: string): boolean {
  return Array.from(value).some((character) => {
    const code = character.charCodeAt(0)
    return code < 0x20 || code === 0x7f
  })
}

function normalizedBindings(): RelayGroupBinding[] | null {
  const normalized = bindings.value
    .map((binding) => ({
      group_id: binding.group_id,
      sources: binding.sources.map((source) => ({
        ...source,
        station_id: source.station_id.trim(),
        source_group: source.source_group.trim() || 'default',
        delta: Number(source.delta)
      }))
    }))
    .filter((binding) => binding.sources.length > 0)

  for (const binding of normalized) {
    for (const source of binding.sources) {
      if (!source.source_group || source.source_group.length > 100 || hasControlCharacter(source.source_group)) {
        appStore.showError(t('admin.relayStations.binding.sourceGroupInvalid'))
        return null
      }
      if (!Number.isFinite(source.delta)) {
        appStore.showError(t('admin.relayStations.binding.deltaInvalid'))
        return null
      }
    }
  }
  return normalized
}

async function saveBindings(): Promise<void> {
  const payload = normalizedBindings()
  if (!payload) return
  savingBindings.value = true
  try {
    const result = await adminAPI.relayStations.updateBindings(payload)
    bindings.value = result.bindings
    bindingsDirty.value = false
    appStore.showSuccess(t('admin.relayStations.bindingsSaved'))
  } catch (error) {
    appStore.showError(errorMessage(error, t('admin.relayStations.bindingsSaveFailed')))
    savingBindings.value = false
    return
  }
  savingBindings.value = false
  await refreshAllRates(false)
}

async function refreshAllRates(showSuccess = true): Promise<void> {
  refreshingRates.value = true
  try {
    const result = await adminAPI.relayStations.refreshRates()
    rates.value = result.rates
    if (!result.refreshed) appStore.showWarning(t('admin.relayStations.ratesRefreshPartial'))
    else if (showSuccess) appStore.showSuccess(t('admin.relayStations.ratesRefreshSuccess'))
  } catch (error) {
    appStore.showError(errorMessage(error, t('admin.relayStations.ratesRefreshFailed')))
  } finally {
    refreshingRates.value = false
  }
}

async function loadProfit(): Promise<void> {
  if (!profitStartDate.value || !profitEndDate.value || profitStartDate.value > profitEndDate.value) {
    appStore.showError(t('admin.relayStations.profit.invalidRange'))
    return
  }
  profitLoading.value = true
  profitUnavailable.value = false
  try {
    const startAt = `${profitStartDate.value}T00:00:00Z`
    const endAt = `${profitEndDate.value}T23:59:59Z`
    const result = await adminAPI.relayStations.getProfit(startAt, endAt)
    profitEstimates.value = result.estimates
  } catch (error) {
    profitEstimates.value = []
    if (relayErrorReason(error) === 'RELAY_PROFIT_UNAVAILABLE') {
      profitUnavailable.value = true
    } else {
      appStore.showError(errorMessage(error, t('admin.relayStations.profit.loadFailed')))
    }
  } finally {
    profitLoading.value = false
    profitLoaded.value = true
  }
}

watch(selectedGroupId, () => {
  stationToAdd.value = null
})

watch(activeTab, (tab) => {
  if (tab === 'profit' && !profitLoaded.value) void loadProfit()
})

onMounted(() => {
  void loadAll()
})
</script>

<style scoped>
.action-button {
  @apply flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-xs text-gray-500 transition-colors disabled:cursor-not-allowed disabled:opacity-50;
}
</style>
