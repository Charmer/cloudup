<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { t } from '../i18n.js'

const connections = ref([])
const entries = ref([])
const listError = ref('')
const filters = reactive({ connectionId: '', status: '' })
const verifying = reactive({})

// Pagination state: pageSize is fixed (matches the server's
// DefaultHistoryPageSize), offset is "how many matching entries to skip"
// and total comes back from the server on every load - see api.listHistory.
const pageSize = 50
const offset = ref(0)
const total = ref(0)

async function loadConnections() {
  connections.value = await api.listConnections()
}

async function loadHistory() {
  listError.value = ''
  try {
    const page = await api.listHistory({
      connectionId: filters.connectionId || undefined,
      status: filters.status || undefined,
      limit: pageSize,
      offset: offset.value,
    })
    entries.value = page.entries
    total.value = page.total
  } catch (e) {
    listError.value = e.message
  }
}

// A filter change invalidates whatever page we were on (e.g. offset=100
// might not exist under the new filter) - always go back to the first page.
function applyFilters() {
  offset.value = 0
  loadHistory()
}

function prevPage() {
  offset.value = Math.max(0, offset.value - pageSize)
  loadHistory()
}

function nextPage() {
  offset.value += pageSize
  loadHistory()
}

const rangeStart = computed(() => (entries.value.length ? offset.value + 1 : 0))
const rangeEnd = computed(() => offset.value + entries.value.length)
const hasPrevPage = computed(() => offset.value > 0)
const hasNextPage = computed(() => offset.value + entries.value.length < total.value)

function connectionName(id) {
  return connections.value.find((c) => c.id === id)?.displayName || id
}

async function verify(id) {
  verifying[id] = true
  try {
    const updated = await api.verifyHistoryEntry(id)
    const idx = entries.value.findIndex((e) => e.ID === id)
    if (idx !== -1) entries.value[idx] = updated
  } catch (e) {
    listError.value = e.message
  } finally {
    verifying[id] = false
  }
}

// Upload status and check status are bare enum values from the server
// (history.StatusSuccess / history.CheckOK etc.), mirrored 1:1 by the
// status.* and check.* catalog keys.
function statusLabel(status) {
  return t(`status.${status}`)
}

function checkLabel(status) {
  return t(`check.${status}`)
}

async function remove(id) {
  if (!confirm(t('history.deleteConfirm'))) return
  await api.deleteHistoryEntry(id)
  // Deleting the last entry on a page beyond the first would otherwise
  // load and render an empty page - step back one page instead.
  if (entries.value.length === 1 && offset.value > 0) {
    offset.value = Math.max(0, offset.value - pageSize)
  }
  await loadHistory()
}

onMounted(() => {
  loadConnections()
  loadHistory()
})
</script>

<template>
  <h1>{{ t('history.title') }}</h1>

  <section class="card">
    <div style="display:flex; gap:1rem; align-items:flex-end; flex-wrap:wrap">
      <div class="field-row" style="margin-bottom:0">
        <label>{{ t('history.filterConnection') }}</label>
        <select v-model="filters.connectionId" @change="applyFilters">
          <option value="">{{ t('history.allConnections') }}</option>
          <option v-for="c in connections" :key="c.id" :value="c.id">{{ c.displayName }}</option>
        </select>
      </div>
      <div class="field-row" style="margin-bottom:0">
        <label>{{ t('history.filterStatus') }}</label>
        <select v-model="filters.status" @change="applyFilters">
          <option value="">{{ t('history.allStatuses') }}</option>
          <option value="success">{{ t('status.success') }}</option>
          <option value="error">{{ t('status.error') }}</option>
          <option value="cancelled">{{ t('status.cancelled') }}</option>
        </select>
      </div>
      <button @click="loadHistory">{{ t('common.refresh') }}</button>
    </div>
  </section>

  <div v-if="listError" class="error-banner">{{ listError }}</div>

  <section class="card">
    <table v-if="entries.length">
      <thead>
        <tr>
          <th>{{ t('common.file') }}</th>
          <th>{{ t('common.connection') }}</th>
          <th>{{ t('history.uploadedAt') }}</th>
          <th>{{ t('common.status') }}</th>
          <th>{{ t('history.checkStatus') }}</th>
          <th>{{ t('common.actions') }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="e in entries" :key="e.ID">
          <td>{{ e.LocalPath }} → {{ e.RemotePath }}</td>
          <td>{{ connectionName(e.ProviderID) }}</td>
          <td>{{ new Date(e.UploadedAt).toLocaleString() }}</td>
          <td><span class="badge" :class="e.Status">{{ statusLabel(e.Status) }}</span></td>
          <td>
            <span v-if="e.LastCheckStatus" class="badge" :class="e.LastCheckStatus">
              {{ checkLabel(e.LastCheckStatus) }}
            </span>
            <span v-else class="muted">{{ t('check.never') }}</span>
          </td>
          <td>
            <div style="display:flex; gap:0.4rem">
              <button :disabled="verifying[e.ID]" @click="verify(e.ID)">
                {{ verifying[e.ID] ? t('history.verifying') : t('history.verify') }}
              </button>
              <button class="danger" @click="remove(e.ID)">{{ t('common.delete') }}</button>
            </div>
          </td>
        </tr>
      </tbody>
    </table>
    <p v-else class="muted">{{ t('history.empty') }}</p>

    <div v-if="total" style="display:flex; gap:1rem; align-items:center; margin-top:0.75rem">
      <button :disabled="!hasPrevPage" @click="prevPage">{{ t('common.previous') }}</button>
      <button :disabled="!hasNextPage" @click="nextPage">{{ t('common.next') }}</button>
      <span class="muted">{{ rangeStart }}–{{ rangeEnd }} / {{ total }}</span>
    </div>
  </section>
</template>
