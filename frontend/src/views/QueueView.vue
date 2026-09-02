<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { t } from '../i18n.js'

const connections = ref([])
const selectedConnectionId = ref('')
const remoteFolder = ref('')
// Each entry is { file: File, relativePath: string } - relativePath is
// just the file name for a flat pick, but the full path under the picked
// folder (e.g. "2024/summer/pic1.jpg") when the file came from the folder
// dialog or a dropped folder, so upload() can preserve that structure on
// the remote side without knowing which input produced each entry.
const files = ref([])
const uploadError = ref('')
const uploading = ref(false)
const dropActive = ref(false)

async function loadConnections() {
  connections.value = await api.listConnections()
  if (!selectedConnectionId.value && connections.value.length) {
    selectedConnectionId.value = connections.value[0].id
  }
}

// FileList entries from a plain <input type="file"> carry no relative
// path; from one with the webkitdirectory attribute, the browser fills in
// webkitRelativePath (e.g. "photos/2024/summer/pic1.jpg") for every file.
function toEntries(fileList) {
  return Array.from(fileList || []).map((file) => ({
    file,
    relativePath: file.webkitRelativePath || file.name,
  }))
}

function onFilesPicked(e) {
  files.value = toEntries(e.target.files)
}

function onFolderPicked(e) {
  files.value = toEntries(e.target.files)
}

function onDragOver(e) {
  e.preventDefault()
  dropActive.value = true
}

function onDragLeave() {
  dropActive.value = false
}

// Drag-and-drop of a folder does not populate FileList/webkitRelativePath
// the way the folder dialog does - the only way to recover structure is
// walking the DataTransferItemList's FileSystemEntry tree by hand. This
// same walk also transparently covers plain dropped files (a
// FileSystemFileEntry with no directory ancestor), so this one handler
// covers both drag-and-drop cases at once.
async function onDrop(e) {
  e.preventDefault()
  dropActive.value = false

  const items = e.dataTransfer?.items
  if (!items || !items.length) return

  const entries = []
  for (const item of items) {
    const entry = item.webkitGetAsEntry?.()
    if (entry) entries.push(entry)
  }

  uploadError.value = ''
  try {
    const collected = []
    for (const entry of entries) {
      collected.push(...(await walkEntry(entry, '')))
    }
    files.value = collected
  } catch (err) {
    uploadError.value = err.message
  }
}

function readDirEntries(reader) {
  return new Promise((resolve, reject) => reader.readEntries(resolve, reject))
}

function fileFromEntry(entry) {
  return new Promise((resolve, reject) => entry.file(resolve, reject))
}

// Recursively expands one FileSystemEntry (file or directory) into
// { file, relativePath } entries, prefixing relativePath with basePath so
// nested calls build up the full path from the originally dropped root.
async function walkEntry(entry, basePath) {
  if (entry.isFile) {
    const file = await fileFromEntry(entry)
    return [{ file, relativePath: basePath + entry.name }]
  }
  if (!entry.isDirectory) return []

  const reader = entry.createReader()
  const collected = []
  // FileSystemDirectoryReader.readEntries only returns a batch at a time
  // (Chrome caps it around 100) - the empty result is what signals "done",
  // not a shorter-than-requested batch.
  let batch
  do {
    batch = await readDirEntries(reader)
    for (const child of batch) {
      collected.push(...(await walkEntry(child, basePath + entry.name + '/')))
    }
  } while (batch.length > 0)
  return collected
}

async function upload() {
  if (!selectedConnectionId.value || !files.value.length) return
  uploadError.value = ''
  uploading.value = true
  try {
    for (const { file, relativePath } of files.value) {
      const remotePath = remoteFolder.value
        ? `${remoteFolder.value.replace(/\/+$/, '')}/${relativePath}`
        : relativePath
      await api.uploadFile(selectedConnectionId.value, file, remotePath)
    }
    files.value = []
  } catch (e) {
    uploadError.value = e.message
  } finally {
    uploading.value = false
  }
}

const tasks = ref([])
let pollHandle = null

async function pollTasks() {
  try {
    tasks.value = await api.listTasks()
  } catch {
    // A transient poll failure isn't worth surfacing as an error banner -
    // the next tick tries again.
  }
}

function percent(t) {
  if (!t.total) return t.status === 'success' ? 100 : 0
  return Math.min(100, Math.round((t.sent / t.total) * 100))
}

function connectionName(id) {
  return connections.value.find((c) => c.id === id)?.displayName || id
}

async function cancelTask(id) {
  await api.cancelTask(id)
}

async function pause(id) {
  await api.pauseConnection(id)
}
async function resume(id) {
  await api.resumeConnection(id)
}
// Task/check status strings come back as bare enum values ("queued",
// "uploading", ...) - the catalogs mirror them 1:1 under status.*, so a
// new backend status shows as "status.<value>" rather than silently
// English.
function statusLabel(status) {
  return t(`status.${status}`)
}

async function cancelAll(id) {
  if (!confirm(t('queue.cancelAllConfirm'))) return
  await api.cancelAllForConnection(id)
}

const sortedTasks = computed(() =>
  [...tasks.value].sort((a, b) => (a.id < b.id ? 1 : -1))
)

onMounted(() => {
  loadConnections()
  pollTasks()
  pollHandle = setInterval(pollTasks, 1500)
})
onUnmounted(() => {
  if (pollHandle) clearInterval(pollHandle)
})
</script>

<template>
  <h1>{{ t('queue.title') }}</h1>

  <section class="card">
    <h2>{{ t('queue.uploadFiles') }}</h2>
    <div v-if="uploadError" class="error-banner">{{ uploadError }}</div>
    <div class="field-row">
      <label>{{ t('queue.selectConnection') }}</label>
      <select v-model="selectedConnectionId">
        <option v-for="c in connections" :key="c.id" :value="c.id">{{ c.displayName }}</option>
      </select>
    </div>
    <div class="field-row">
      <label>{{ t('queue.remotePrefix') }}</label>
      <input v-model="remoteFolder" :placeholder="t('queue.remotePrefixPlaceholder')" />
    </div>
    <div class="field-row">
      <label>{{ t('queue.files') }}</label>
      <input type="file" multiple @change="onFilesPicked" />
      <input
        type="file"
        webkitdirectory
        multiple
        :title="t('queue.selectFolder')"
        @change="onFolderPicked"
      />
    </div>
    <div
      class="drop-zone"
      :class="{ active: dropActive }"
      @dragover="onDragOver"
      @dragleave="onDragLeave"
      @drop="onDrop"
    >
      <span v-if="files.length" class="muted">{{ files.length }} {{ t('queue.filesSelected') }}</span>
      <span v-else class="muted">{{ t('queue.dropZoneHint') }}</span>
    </div>
    <div class="form-actions">
      <button class="primary" :disabled="uploading || !files.length || !selectedConnectionId" @click="upload">
        {{ uploading ? t('queue.uploading') : t('queue.upload') }}
      </button>
      <button v-if="selectedConnectionId" @click="pause(selectedConnectionId)">
        {{ t('queue.pause') }}
      </button>
      <button v-if="selectedConnectionId" @click="resume(selectedConnectionId)">
        {{ t('queue.resume') }}
      </button>
      <button v-if="selectedConnectionId" class="danger" @click="cancelAll(selectedConnectionId)">
        {{ t('queue.cancelAll') }}
      </button>
    </div>
  </section>

  <section class="card">
    <h2>{{ t('queue.tasks') }}</h2>
    <p class="muted">{{ t('queue.autoRefresh') }}</p>
    <table v-if="sortedTasks.length">
      <thead>
        <tr>
          <th>{{ t('common.file') }}</th>
          <th>{{ t('common.connection') }}</th>
          <th>{{ t('common.status') }}</th>
          <th>{{ t('queue.progress') }}</th>
          <th>{{ t('common.actions') }}</th>
        </tr>
      </thead>
      <tbody>
        <!-- The row variable is "task", not "t": "t" is the translation
             lookup imported above, and a v-for alias would shadow it. -->
        <tr v-for="task in sortedTasks" :key="task.id">
          <td>{{ task.localPath }} → {{ task.remotePath }}</td>
          <td>{{ connectionName(task.connectionId) }}</td>
          <td>
            <span class="badge" :class="task.status">{{ statusLabel(task.status) }}</span>
            <div v-if="task.error" class="muted">{{ task.error }}</div>
          </td>
          <td style="min-width:140px">
            <div class="progress-track">
              <div class="progress-fill" :style="{ width: percent(task) + '%' }"></div>
            </div>
            <span class="muted" v-if="task.total">
              {{ task.sent }}/{{ task.total }} {{ t('common.bytes') }}
            </span>
          </td>
          <td>
            <button
              v-if="task.status === 'queued' || task.status === 'uploading'"
              class="danger"
              @click="cancelTask(task.id)"
            >
              {{ t('common.cancel') }}
            </button>
          </td>
        </tr>
      </tbody>
    </table>
    <p v-else class="muted">{{ t('queue.empty') }}</p>
  </section>
</template>
