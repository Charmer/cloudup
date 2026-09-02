<script setup>
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { t } from '../i18n.js'

const connections = ref([])
const rules = ref([])
const listError = ref('')

async function loadConnections() {
  connections.value = await api.listConnections()
}

async function loadRules() {
  listError.value = ''
  try {
    rules.value = await api.listWatches()
  } catch (e) {
    listError.value = e.message
  }
}

function connectionName(id) {
  return connections.value.find((c) => c.id === id)?.displayName || id
}

const form = reactive({ localPath: '', connectionId: '', remoteFolder: '' })
const createError = ref('')
const creating = ref(false)

async function createRule() {
  createError.value = ''
  creating.value = true
  try {
    await api.createWatch({
      localPath: form.localPath,
      connectionId: form.connectionId,
      remoteFolder: form.remoteFolder,
    })
    form.localPath = ''
    form.remoteFolder = ''
    await loadRules()
  } catch (e) {
    createError.value = e.message
  } finally {
    creating.value = false
  }
}

async function toggleEnabled(rule) {
  await api.updateWatch(rule.id, {
    localPath: rule.localPath,
    connectionId: rule.connectionId,
    remoteFolder: rule.remoteFolder,
    enabled: !rule.enabled,
  })
  await loadRules()
}

async function removeRule(id) {
  if (!confirm(t('watches.deleteConfirm'))) return
  await api.deleteWatch(id)
  await loadRules()
}

onMounted(() => {
  loadConnections()
  loadRules()
})
</script>

<template>
  <h1>{{ t('watches.title') }}</h1>
  <p class="muted">{{ t('watches.intro') }}</p>

  <section class="card">
    <h2>{{ t('watches.add') }}</h2>
    <div v-if="createError" class="error-banner">{{ createError }}</div>
    <div class="field-row">
      <label>{{ t('watches.localPath') }}</label>
      <input v-model="form.localPath" :placeholder="t('watches.localPathPlaceholder')" />
      <span class="muted">{{ t('watches.localPathHint') }}</span>
    </div>
    <div class="field-row">
      <label>{{ t('common.connection') }}</label>
      <select v-model="form.connectionId">
        <option value="" disabled>{{ t('watches.selectConnection') }}</option>
        <option v-for="c in connections" :key="c.id" :value="c.id">{{ c.displayName }}</option>
      </select>
    </div>
    <div class="field-row">
      <label>{{ t('queue.remotePrefix') }}</label>
      <input v-model="form.remoteFolder" :placeholder="t('queue.remotePrefixPlaceholder')" />
    </div>
    <div class="form-actions">
      <button
        class="primary"
        :disabled="creating || !form.localPath || !form.connectionId"
        @click="createRule"
      >
        {{ t('common.create') }}
      </button>
    </div>
    <p class="muted">{{ t('watches.addHint') }}</p>
  </section>

  <section class="card">
    <h2>{{ t('watches.existing') }}</h2>
    <div v-if="listError" class="error-banner">{{ listError }}</div>
    <table v-if="rules.length">
      <thead>
        <tr>
          <th>{{ t('watches.localPath') }}</th>
          <th>{{ t('common.connection') }}</th>
          <th>{{ t('queue.remotePrefix') }}</th>
          <th>{{ t('common.status') }}</th>
          <th>{{ t('common.actions') }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="r in rules" :key="r.id">
          <td>{{ r.localPath }}</td>
          <td>{{ connectionName(r.connectionId) }}</td>
          <td>{{ r.remoteFolder || t('watches.remoteRoot') }}</td>
          <td>
            <span class="badge" :class="r.status">{{ t(`watches.status.${r.status}`) }}</span>
            <span v-if="r.statusMessage" class="muted"> - {{ r.statusMessage }}</span>
          </td>
          <td>
            <div style="display:flex; gap:0.4rem">
              <button @click="toggleEnabled(r)">
                {{ r.enabled ? t('watches.pause') : t('watches.resume') }}
              </button>
              <button class="danger" @click="removeRule(r.id)">{{ t('common.delete') }}</button>
            </div>
          </td>
        </tr>
      </tbody>
    </table>
    <p v-else class="muted">{{ t('watches.empty') }}</p>
  </section>
</template>
