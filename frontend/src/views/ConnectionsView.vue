<script setup>
import { onMounted, onUnmounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { t } from '../i18n.js'

const connections = ref([])
const listError = ref('')

async function loadConnections() {
  listError.value = ''
  try {
    connections.value = await api.listConnections()
  } catch (e) {
    listError.value = e.message
  }
}

// providerTypes entries are { type, requiresOAuth } objects - the API
// reports which types need an interactive authorization step, so this view
// never hardcodes "googledrive" (and now covers Dropbox for free).
const providerTypes = ref([])
const schema = ref([])
const form = reactive({ providerType: '', displayName: '', values: {} })
const createError = ref('')
const creating = ref(false)

// Whether each OAuth provider type's app-wide client (Settings page) is
// already configured. Checked proactively so this view can steer a user
// away from ever seeing POST .../oauth/authorize's raw "OAuth client not
// configured - PUT /api/v1/provider-types/{type}/oauth-credentials first"
// - a perfectly clear message for a script calling the REST API directly,
// but not something a person clicking a button should ever have to read
// and decode into "go to Settings first".
const oauthConfigured = reactive({})

async function loadProviderTypes() {
  providerTypes.value = await api.providerTypes()
  if (providerTypes.value.length) {
    form.providerType = providerTypes.value[0].type
    await loadSchema()
  }
  for (const t of providerTypes.value) {
    if (!t.requiresOAuth) continue
    try {
      oauthConfigured[t.type] = (await api.oauthCredentials(t.type)).configured
    } catch {
      oauthConfigured[t.type] = false
    }
  }
}

function requiresOAuth(providerType) {
  return providerTypes.value.some((t) => t.type === providerType && t.requiresOAuth)
}

async function loadSchema() {
  form.values = {}
  if (!form.providerType) {
    schema.value = []
    return
  }
  schema.value = await api.providerSchema(form.providerType)
  for (const f of schema.value) form.values[f.Key] = ''
}

async function createConnection() {
  createError.value = ''
  creating.value = true
  try {
    const fields = {}
    const secrets = {}
    for (const f of schema.value) {
      if (f.Type === 'password') secrets[f.Key] = form.values[f.Key] || ''
      else fields[f.Key] = form.values[f.Key] || ''
    }
    await api.createConnection({
      providerType: form.providerType,
      displayName: form.displayName,
      fields,
      secrets,
    })
    form.displayName = ''
    await loadSchema()
    await loadConnections()
  } catch (e) {
    createError.value = e.message
  } finally {
    creating.value = false
  }
}

const testResults = reactive({})
async function testConnection(id) {
  testResults[id] = { pending: true }
  try {
    const result = await api.testConnection(id)
    testResults[id] = result
  } catch (e) {
    testResults[id] = { ok: false, error: e.message }
  }
}

async function removeConnection(id) {
  if (!confirm(t('connections.deleteConfirm'))) return
  await api.deleteConnection(id)
  await loadConnections()
}

const oauthAuth = reactive({})
let oauthPollHandle = null

async function startOauthAuthorize(id) {
  oauthAuth[id] = { starting: true }
  try {
    const res = await api.startOauthAuthorize(id)
    oauthAuth[id] = { authUrl: res.authUrl, done: false }
    window.open(res.authUrl, '_blank', 'noopener')
    pollOauthAuthorize(id)
  } catch (e) {
    oauthAuth[id] = { error: e.message }
  }
}

function pollOauthAuthorize(id) {
  const tick = async () => {
    try {
      const status = await api.oauthAuthorizeStatus(id)
      oauthAuth[id] = status
      if (!status.done) oauthPollHandle = setTimeout(tick, 2000)
    } catch (e) {
      oauthAuth[id] = { error: e.message }
    }
  }
  tick()
}

onMounted(() => {
  loadConnections()
  loadProviderTypes()
})
onUnmounted(() => {
  if (oauthPollHandle) clearTimeout(oauthPollHandle)
})
</script>

<template>
  <h1>{{ t('connections.title') }}</h1>

  <section class="card">
    <h2>{{ t('connections.add') }}</h2>
    <div v-if="createError" class="error-banner">{{ createError }}</div>
    <div class="field-row">
      <label>{{ t('connections.providerType') }}</label>
      <select v-model="form.providerType" @change="loadSchema">
        <option v-for="t in providerTypes" :key="t.type" :value="t.type">{{ t.type }}</option>
      </select>
    </div>
    <div class="field-row">
      <label>{{ t('connections.displayName') }}</label>
      <input v-model="form.displayName" :placeholder="t('connections.displayNamePlaceholder')" />
    </div>
    <div v-for="f in schema" :key="f.Key" class="field-row">
      <label>{{ f.Label }}<span v-if="f.Required"> *</span></label>
      <select v-if="f.Type === 'select'" v-model="form.values[f.Key]">
        <option v-for="opt in f.Options" :key="opt" :value="opt">{{ opt }}</option>
      </select>
      <input
        v-else
        v-model="form.values[f.Key]"
        :type="f.Type === 'password' ? 'password' : 'text'"
      />
    </div>
    <div class="form-actions">
      <button class="primary" :disabled="creating || !form.displayName" @click="createConnection">
        {{ t('common.create') }}
      </button>
    </div>
    <p v-if="requiresOAuth(form.providerType) && oauthConfigured[form.providerType] === false" class="warning-banner">
      {{ t('connections.oauthNotConfigured') }}
      <RouterLink :to="{ path: '/settings', query: { tab: 'oauth' } }">{{ t('connections.oauthGoToSettings') }}</RouterLink>
    </p>
    <p v-else-if="requiresOAuth(form.providerType)" class="muted">{{ t('connections.oauthHint') }}</p>
  </section>

  <section class="card">
    <h2>{{ t('connections.existing') }}</h2>
    <div v-if="listError" class="error-banner">{{ listError }}</div>
    <table v-if="connections.length">
      <thead>
        <tr>
          <th>{{ t('common.name') }}</th>
          <th>{{ t('common.type') }}</th>
          <th>{{ t('common.created') }}</th>
          <th>{{ t('common.actions') }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="c in connections" :key="c.id">
          <td>{{ c.displayName }}</td>
          <td>{{ c.providerType }}</td>
          <td>{{ new Date(c.createdAt).toLocaleString() }}</td>
          <td>
            <div style="display:flex; gap:0.4rem; flex-wrap:wrap; align-items:center">
              <button @click="testConnection(c.id)">{{ t('connections.test') }}</button>
              <button
                v-if="requiresOAuth(c.providerType) && oauthConfigured[c.providerType] !== false"
                @click="startOauthAuthorize(c.id)"
              >
                {{ t('connections.authorize') }}
              </button>
              <span v-else-if="requiresOAuth(c.providerType)" class="badge error">
                {{ t('connections.oauthNotConfiguredShort') }}
                <RouterLink :to="{ path: '/settings', query: { tab: 'oauth' } }">{{ t('connections.oauthGoToSettings') }}</RouterLink>
              </span>
              <button class="danger" @click="removeConnection(c.id)">{{ t('common.delete') }}</button>
              <span v-if="testResults[c.id]?.pending" class="muted">{{ t('connections.testing') }}</span>
              <span v-else-if="testResults[c.id]?.ok === true" class="badge success">
                {{ t('connections.testOk') }}
              </span>
              <span v-else-if="testResults[c.id]?.ok === false" class="badge error">
                {{ t('connections.testFailed') }}: {{ testResults[c.id].error }}
              </span>
              <span v-if="oauthAuth[c.id]?.authUrl && !oauthAuth[c.id]?.done" class="muted">
                {{ t('connections.authorizing') }}
                <a :href="oauthAuth[c.id].authUrl" target="_blank" rel="noopener">
                  {{ t('connections.openAuthAgain') }}
                </a>
              </span>
              <span v-if="oauthAuth[c.id]?.done && !oauthAuth[c.id]?.error" class="badge success">
                {{ t('connections.authorized') }}
              </span>
              <span v-if="oauthAuth[c.id]?.error" class="badge error">{{ oauthAuth[c.id].error }}</span>
            </div>
          </td>
        </tr>
      </tbody>
    </table>
    <p v-else class="muted">{{ t('connections.empty') }}</p>
  </section>
</template>
