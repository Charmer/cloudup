<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, loadConnectionSettings, saveConnectionSettings } from '../api.js'
import { currentLanguage, loadLanguage, t } from '../i18n.js'

// Three tabs instead of one long scroll of unrelated forms - which tab is
// initially active comes from ?tab=... (see router.js), so a link from
// elsewhere (e.g. ConnectionsView's "OAuth client not configured" notice)
// can land a user directly on the right tab instead of making them find it
// by scrolling. Switching tabs updates the query param the same way, so
// the deep link stays shareable/refreshable.
const route = useRoute()
const router = useRouter()
const tabs = ['app', 'oauth', 'server']
const activeTab = ref(tabs.includes(route.query.tab) ? route.query.tab : 'app')
function selectTab(tab) {
  activeTab.value = tab
  router.replace({ query: { ...route.query, tab } })
}

const conn = reactive(loadConnectionSettings())
const connStatus = ref('')

function saveConn() {
  saveConnectionSettings({ baseUrl: conn.baseUrl, token: conn.token })
  connStatus.value = t('common.saved')
  checkHealth()
}

async function checkHealth() {
  try {
    await api.healthz()
    connStatus.value = t('settings.serverReachable')
  } catch (e) {
    connStatus.value = `${t('settings.serverUnreachable')}: ${e.message}`
  }
}

// Populated from GET /api/v1/languages - built-ins plus whatever JSON the
// operator dropped into the server's languages folder, so this list is
// never hardcoded here.
const languages = ref([])

async function loadLanguages() {
  try {
    languages.value = await api.languages()
  } catch (e) {
    appSettingsError.value = e.message
  }
}

// Switch immediately (no reload): swap the catalog first so the UI is
// already translated, then persist the choice server-side, since
// Settings.language is the shared source of truth.
async function changeLanguage(code) {
  appSettingsError.value = ''
  try {
    await loadLanguage(code)
    appSettings.language = code
    await api.setSettings(appSettings)
  } catch (e) {
    appSettingsError.value = e.message
  }
}

const appSettings = reactive({
  maxConcurrentUploadsPerProvider: 1,
  verifyChecksumAfterUpload: {}, // keyed by provider type - see loadProviderTypes
  language: 'en',
  multiThreadStreams: 4,
  multiThreadThresholdBytes: 268435456, // 256 MiB, mirrors settings.DefaultMultiThreadThresholdBytes
  maxUploadBytesPerSecond: 0, // 0 = unlimited, mirrors settings.Default()
  idleConnectionTimeoutMinutes: 10, // mirrors settings.DefaultIdleConnectionTimeoutMinutes
})
const appSettingsLoaded = ref(false)
const appSettingsError = ref('')
const appSettingsStatus = ref('')

// Every provider type, for the per-type verify-checksum-after-upload
// checkboxes below - a superset of oauthTypes (which is filtered to just
// the OAuth ones for the OAuth tab, further down this file).
const allProviderTypes = ref([])

// Purely a static, hand-maintained hint for the checkboxes below - NOT
// fetched from the server or backed by anything in the Go code, on
// purpose: the backend has no notion of "cheap" vs "expensive" here, it
// only stores a plain per-type on/off map (see internal/settings.
// Settings.VerifyChecksumAfterUpload). Keep this in sync with
// CONTRIBUTING.md's "Every provider decides its own checksum strategy"
// section, which is the source of truth for *why* each type is listed
// here or not.
const expensiveVerifyTypes = ['webdav', 's3', 'dropbox', 'ftp', 'sftp']

async function loadProviderTypes() {
  try {
    allProviderTypes.value = (await api.providerTypes()).map((t) => t.type)
  } catch (e) {
    appSettingsError.value = e.message
  }
}

// The API stores/reports the threshold in bytes (exact, unambiguous over
// the wire); the form shows MiB, which is what a user actually thinks in.
const multiThreadThresholdMiB = computed({
  get: () => Math.round(appSettings.multiThreadThresholdBytes / (1024 * 1024)),
  set: (mib) => {
    appSettings.multiThreadThresholdBytes = Math.round(mib * 1024 * 1024)
  },
})

// Same bytes-on-the-wire/human-unit split as multiThreadThresholdMiB above:
// the API stores/reports bytes/sec (exact), the form shows KB/s. 0 stays 0
// either way - that is the meaningful "unlimited" value, not a placeholder.
const maxUploadKBPerSecond = computed({
  get: () => Math.round(appSettings.maxUploadBytesPerSecond / 1024),
  set: (kb) => {
    appSettings.maxUploadBytesPerSecond = Math.max(0, Math.round(kb * 1024))
  },
})

async function loadAppSettings() {
  appSettingsError.value = ''
  try {
    const v = await api.getSettings()
    Object.assign(appSettings, v)
    appSettingsLoaded.value = true
  } catch (e) {
    appSettingsError.value = e.message
  }
}

async function saveAppSettings() {
  appSettingsStatus.value = ''
  appSettingsError.value = ''
  try {
    const v = await api.setSettings(appSettings)
    Object.assign(appSettings, v)
    appSettingsStatus.value = t('settings.savedApplied')
  } catch (e) {
    appSettingsError.value = e.message
  }
}

// One card per provider type that reports requiresOAuth - rather than a
// hardcoded "Google Drive OAuth client" card - so Dropbox (and any future
// OAuth provider) shows up here with no change to this view. Each entry
// keeps its own form state and its own configured/not-configured status;
// the server never returns stored values, only whether they exist.
const oauthTypes = ref([])
const oauthForms = reactive({})
const oauthError = ref('')

// Where to create the OAuth client for each known provider type. Purely a
// convenience link; an unlisted type simply gets no link.
const oauthConsoleUrls = {
  googledrive: 'https://console.cloud.google.com/apis/credentials',
  dropbox: 'https://www.dropbox.com/developers/apps',
  yandexdisk: 'https://oauth.yandex.ru/client/new',
  onedrive: 'https://portal.azure.com/#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade',
}

// The exact redirect URI to register with the provider - computed, not a
// static example, since it must match wherever this cloudup instance
// actually runs (see CONTRIBUTING.md's OAuth section). conn.baseUrl is
// '' for the normal same-origin run, in which case window.location.origin
// is the right value.
const oauthCallbackURL = computed(() => `${conn.baseUrl || window.location.origin}/api/v1/oauth/callback`)

async function loadOauthTypes() {
  oauthError.value = ''
  try {
    const types = await api.providerTypes()
    oauthTypes.value = types.filter((t) => t.requiresOAuth).map((t) => t.type)
    for (const type of oauthTypes.value) {
      oauthForms[type] = { clientId: '', clientSecret: '', configured: null, status: '', error: '' }
      await loadOauthCredentials(type)
    }
  } catch (e) {
    oauthError.value = e.message
  }
}

async function loadOauthCredentials(type) {
  try {
    const v = await api.oauthCredentials(type)
    oauthForms[type].configured = v.configured
  } catch (e) {
    oauthForms[type].error = e.message
  }
}

async function saveOauthCredentials(type) {
  const form = oauthForms[type]
  form.error = ''
  form.status = ''
  try {
    await api.setOauthCredentials(type, { clientId: form.clientId, clientSecret: form.clientSecret })
    form.clientId = ''
    form.clientSecret = ''
    form.status = t('settings.oauthStored')
    await loadOauthCredentials(type)
  } catch (e) {
    form.error = e.message
  }
}

// Update checks only ever happen when the user clicks the button below -
// there is no startup check and no polling anywhere in this app. See
// api.checkForUpdates and internal/httpapi/updates.go.
const updateCheck = reactive({ loading: false, error: '', result: null })

async function checkForUpdates() {
  updateCheck.loading = true
  updateCheck.error = ''
  updateCheck.result = null
  try {
    updateCheck.result = await api.checkForUpdates()
  } catch (e) {
    updateCheck.error = e.message
  } finally {
    updateCheck.loading = false
  }
}

onMounted(() => {
  checkHealth()
  loadAppSettings()
  loadLanguages()
  loadOauthTypes()
  loadProviderTypes()
})
</script>

<template>
  <h1>{{ t('settings.title') }}</h1>

  <nav class="subtabs">
    <button
      v-for="tab in tabs"
      :key="tab"
      type="button"
      class="subtab"
      :class="{ active: activeTab === tab }"
      @click="selectTab(tab)"
    >
      {{ t(`settings.tab.${tab}`) }}
    </button>
  </nav>

  <section v-if="activeTab === 'server'" class="card">
    <h2>{{ t('settings.server') }}</h2>
    <p class="muted">{{ t('settings.serverHint') }}</p>
    <div class="field-row">
      <label>{{ t('settings.serverUrl') }}</label>
      <input v-model="conn.baseUrl" placeholder="http://127.0.0.1:3000" />
      <span class="muted">{{ t('settings.serverUrlHint') }}</span>
    </div>
    <div class="field-row">
      <label>{{ t('settings.token') }}</label>
      <input v-model="conn.token" type="password" :placeholder="t('settings.tokenPlaceholder')" />
    </div>
    <div class="form-actions">
      <button class="primary" @click="saveConn">{{ t('common.save') }}</button>
      <button @click="checkHealth">{{ t('settings.testConnection') }}</button>
    </div>
    <p v-if="connStatus" class="muted">{{ connStatus }}</p>
  </section>

  <section v-if="activeTab === 'app'" class="card">
    <h2>{{ t('settings.app') }}</h2>
    <div v-if="appSettingsError" class="error-banner">{{ appSettingsError }}</div>
    <template v-if="appSettingsLoaded">
      <div class="field-row">
        <label>{{ t('settings.concurrency') }}</label>
        <input v-model.number="appSettings.maxConcurrentUploadsPerProvider" type="number" min="1" />
      </div>
      <div class="field-row">
        <label>{{ t('settings.verifyAfterUpload') }}</label>
        <span class="muted">{{ t('settings.verifyAfterUploadHint') }}</span>
        <label v-for="type in allProviderTypes" :key="type" class="checkbox-line">
          <input v-model="appSettings.verifyChecksumAfterUpload[type]" type="checkbox" style="width:auto" />
          {{ type }}
          <span v-if="expensiveVerifyTypes.includes(type)" class="muted">
            — {{ t('settings.verifyAfterUploadExpensive') }}
          </span>
        </label>
      </div>
      <div class="field-row">
        <label>{{ t('settings.multiThreadStreams') }}</label>
        <input v-model.number="appSettings.multiThreadStreams" type="number" min="1" />
        <span class="muted">{{ t('settings.multiThreadStreamsHint') }}</span>
      </div>
      <div class="field-row">
        <label>{{ t('settings.multiThreadThresholdMiB') }}</label>
        <input v-model.number="multiThreadThresholdMiB" type="number" min="1" />
        <span class="muted">{{ t('settings.multiThreadThresholdMiBHint') }}</span>
      </div>
      <div class="field-row">
        <label>{{ t('settings.maxUploadKBPerSecond') }}</label>
        <input v-model.number="maxUploadKBPerSecond" type="number" min="0" />
        <span class="muted">{{ t('settings.maxUploadKBPerSecondHint') }}</span>
      </div>
      <div class="field-row">
        <label>{{ t('settings.idleConnectionTimeoutMinutes') }}</label>
        <input v-model.number="appSettings.idleConnectionTimeoutMinutes" type="number" min="1" />
        <span class="muted">{{ t('settings.idleConnectionTimeoutMinutesHint') }}</span>
      </div>
      <div class="field-row">
        <label>{{ t('settings.language') }}</label>
        <!-- Options come from the server, and each name is the language's
             own name for itself, so it is readable before switching. -->
        <select :value="currentLanguage" @change="changeLanguage($event.target.value)">
          <option v-for="l in languages" :key="l.code" :value="l.code">{{ l.name }}</option>
        </select>
        <span class="muted">{{ t('settings.languageHint') }}</span>
      </div>
      <div class="form-actions">
        <button class="primary" @click="saveAppSettings">{{ t('common.save') }}</button>
      </div>
      <p v-if="appSettingsStatus" class="muted">{{ appSettingsStatus }}</p>
    </template>
    <p v-else-if="!appSettingsError" class="muted">{{ t('common.loading') }}</p>
  </section>

  <section v-if="activeTab === 'app'" class="card">
    <h2>{{ t('settings.updates') }}</h2>
    <p class="muted">{{ t('settings.updatesHint') }}</p>
    <div class="form-actions">
      <button @click="checkForUpdates" :disabled="updateCheck.loading">
        {{ updateCheck.loading ? t('common.checking') : t('settings.checkForUpdates') }}
      </button>
    </div>
    <div v-if="updateCheck.error" class="error-banner">{{ updateCheck.error }}</div>
    <p v-else-if="updateCheck.result" class="muted">
      <template v-if="updateCheck.result.updateAvailable">
        {{ t('settings.updateAvailable') }}: {{ updateCheck.result.latestVersion }}
        (<a :href="updateCheck.result.releaseUrl" target="_blank" rel="noopener">{{ t('settings.viewRelease') }}</a>)
      </template>
      <template v-else>
        {{ t('settings.upToDate') }}: {{ updateCheck.result.currentVersion }}
      </template>
    </p>
  </section>

  <template v-if="activeTab === 'oauth'">
    <div v-if="oauthError" class="error-banner">{{ oauthError }}</div>
    <p v-if="!oauthTypes.length" class="muted">{{ t('settings.oauthNone') }}</p>
    <section v-for="type in oauthTypes" :key="type" class="card">
    <h2>{{ type }} — {{ t('settings.oauthClient') }}</h2>
    <p class="muted">
      {{ t('settings.oauthClientHint') }}
      <a v-if="oauthConsoleUrls[type]" :href="oauthConsoleUrls[type]" target="_blank" rel="noopener">
        {{ t('settings.openConsole') }}
      </a>
    </p>
    <details v-if="type === 'yandexdisk'" class="oauth-guide">
      <summary>{{ t('settings.yandexGuide.summary') }}</summary>
      <ol>
        <li>{{ t('settings.yandexGuide.step1') }}</li>
        <li>{{ t('settings.yandexGuide.step2') }}</li>
        <li>{{ t('settings.yandexGuide.step3') }}: <code>{{ oauthCallbackURL }}</code></li>
        <li>
          {{ t('settings.yandexGuide.step4') }}
          <ul>
            <li>✅ <code>cloud_api:disk.info</code></li>
            <li>✅ <code>cloud_api:disk.read</code></li>
            <li>✅ <code>cloud_api:disk.write</code></li>
            <li>❌ <code>cloud_api:disk.app_folder</code> — {{ t('settings.yandexGuide.step4NoAppFolder') }}</li>
          </ul>
        </li>
        <li>{{ t('settings.yandexGuide.step5') }}</li>
        <li>{{ t('settings.yandexGuide.step6') }}</li>
      </ol>
    </details>
    <div v-if="oauthForms[type]?.error" class="error-banner">{{ oauthForms[type].error }}</div>
    <p class="muted">
      {{ t('common.status') }}:
      <span v-if="oauthForms[type]?.configured === true">{{ t('settings.configured') }}</span>
      <span v-else-if="oauthForms[type]?.configured === false">{{ t('settings.notConfigured') }}</span>
      <span v-else>{{ t('common.checking') }}</span>
    </p>
    <div class="field-row">
      <label>{{ t('settings.clientId') }}</label>
      <input v-model="oauthForms[type].clientId" />
    </div>
    <div class="field-row">
      <label>{{ t('settings.clientSecret') }}</label>
      <input v-model="oauthForms[type].clientSecret" type="password" />
    </div>
    <div class="form-actions">
      <button class="primary" @click="saveOauthCredentials(type)">{{ t('common.save') }}</button>
    </div>
    <p v-if="oauthForms[type]?.status" class="muted">{{ oauthForms[type].status }}</p>
    </section>
  </template>
</template>
