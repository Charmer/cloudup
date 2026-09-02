// Thin wrapper around cloudup's REST API (see openapi.yaml at the repo
// root - this file's shape follows that contract directly).
//
// Normal path: cmd/server serves this app itself (frontend/dist) from the
// same origin and injects window.__CLOUDUP_TOKEN__/__CLOUDUP_BASE_URL__
// into index.html (see internal/httpapi/server.go's staticHandler) - so
// baseUrl defaults to "" (same-origin relative requests) and the token is
// already filled in, no copy-pasting needed.
//
// Dev/standalone path: running this app separately (`npm run dev`, or any
// other static host) against a cloudup server on a different origin -
// window.__CLOUDUP_TOKEN__ won't exist there, so Settings lets the user
// paste in the server URL and the token it printed to its own console.
// Either way, whatever the user last saved on the Settings page in
// localStorage always wins on subsequent loads.
const STORAGE_KEY = 'cloudup.connection'

function injectedDefaults() {
  return {
    baseUrl: typeof window.__CLOUDUP_BASE_URL__ === 'string' ? window.__CLOUDUP_BASE_URL__ : '',
    token: typeof window.__CLOUDUP_TOKEN__ === 'string' ? window.__CLOUDUP_TOKEN__ : '',
  }
}

export function loadConnectionSettings() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return injectedDefaults()
    return JSON.parse(raw)
  } catch {
    return injectedDefaults()
  }
}

export function saveConnectionSettings(settings) {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(settings))
}

class ApiError extends Error {
  constructor(status, message) {
    super(message)
    this.status = status
  }
}

async function request(path, options = {}) {
  const { baseUrl, token } = loadConnectionSettings()
  const headers = options.headers ? { ...options.headers } : {}
  if (token) headers['Authorization'] = `Bearer ${token}`

  const res = await fetch(`${baseUrl}${path}`, { ...options, headers })
  if (res.status === 204) return null

  const contentType = res.headers.get('content-type') || ''
  const body = contentType.includes('application/json') ? await res.json() : await res.text()

  if (!res.ok) {
    const message = typeof body === 'object' && body?.error ? body.error : String(body)
    throw new ApiError(res.status, message || `HTTP ${res.status}`)
  }
  return body
}

function jsonRequest(path, method, payload) {
  return request(path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  })
}

export const api = {
  healthz: () => request('/healthz'),

  providerTypes: () => request('/api/v1/provider-types'),
  providerSchema: (type) => request(`/api/v1/provider-types/${encodeURIComponent(type)}/schema`),

  listConnections: () => request('/api/v1/connections'),
  getConnection: (id) => request(`/api/v1/connections/${encodeURIComponent(id)}`),
  createConnection: (payload) => jsonRequest('/api/v1/connections', 'POST', payload),
  updateConnection: (id, payload) => jsonRequest(`/api/v1/connections/${encodeURIComponent(id)}`, 'PUT', payload),
  deleteConnection: (id) => request(`/api/v1/connections/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  testConnection: (id) => request(`/api/v1/connections/${encodeURIComponent(id)}/test`, { method: 'POST' }),

  // Interactive OAuth is keyed by provider type rather than hardcoded per
  // provider (these were /drive/... endpoints): which types need it comes
  // from providerTypes()'s requiresOAuth flag, so nothing here - and
  // nothing in the views - has to know the list of OAuth providers.
  oauthCredentials: (type) => request(`/api/v1/provider-types/${encodeURIComponent(type)}/oauth-credentials`),
  setOauthCredentials: (type, payload) =>
    jsonRequest(`/api/v1/provider-types/${encodeURIComponent(type)}/oauth-credentials`, 'PUT', payload),
  startOauthAuthorize: (id) => request(`/api/v1/connections/${encodeURIComponent(id)}/oauth/authorize`, { method: 'POST' }),
  oauthAuthorizeStatus: (id) => request(`/api/v1/connections/${encodeURIComponent(id)}/oauth/authorize`),

  uploadFile: (connectionId, file, remotePath) => {
    const { baseUrl, token } = loadConnectionSettings()
    const form = new FormData()
    form.append('file', file)
    if (remotePath) form.append('remotePath', remotePath)
    const headers = {}
    if (token) headers['Authorization'] = `Bearer ${token}`
    return fetch(`${baseUrl}/api/v1/connections/${encodeURIComponent(connectionId)}/uploads`, {
      method: 'POST',
      headers,
      body: form,
    }).then(async (res) => {
      const body = await res.json()
      if (!res.ok) throw new ApiError(res.status, body?.error || `HTTP ${res.status}`)
      return body
    })
  },

  listTasks: (connectionId) => request(`/api/v1/tasks${connectionId ? `?connectionId=${encodeURIComponent(connectionId)}` : ''}`),
  getTask: (id) => request(`/api/v1/tasks/${encodeURIComponent(id)}`),
  cancelTask: (id) => request(`/api/v1/tasks/${encodeURIComponent(id)}/cancel`, { method: 'POST' }),

  pauseConnection: (id) => request(`/api/v1/connections/${encodeURIComponent(id)}/pause`, { method: 'POST' }),
  resumeConnection: (id) => request(`/api/v1/connections/${encodeURIComponent(id)}/resume`, { method: 'POST' }),
  cancelAllForConnection: (id) => request(`/api/v1/connections/${encodeURIComponent(id)}/cancel-all`, { method: 'POST' }),

  // Returns { entries, total, limit, offset } - GET /api/v1/history is
  // paginated (see openapi.yaml), never a bare array of every entry ever
  // recorded.
  listHistory: (params = {}) => {
    const query = new URLSearchParams()
    if (params.connectionId) query.set('connectionId', params.connectionId)
    if (params.status) query.set('status', params.status)
    if (params.limit) query.set('limit', params.limit)
    if (params.offset) query.set('offset', params.offset)
    const qs = query.toString()
    return request(`/api/v1/history${qs ? `?${qs}` : ''}`)
  },
  deleteHistoryEntry: (id) => request(`/api/v1/history/${id}`, { method: 'DELETE' }),
  verifyHistoryEntry: (id) => request(`/api/v1/history/${id}/verify`, { method: 'POST' }),

  // The available languages are discovered at runtime (built-in ones plus
  // any JSON dropped into the server's languages folder), so the picker in
  // Settings never hardcodes a list. A catalog comes back complete - see
  // i18n.js.
  languages: () => request('/api/v1/languages'),
  language: (code) => request(`/api/v1/languages/${encodeURIComponent(code)}`),

  getSettings: () => request('/api/v1/settings'),
  setSettings: (payload) => jsonRequest('/api/v1/settings', 'PUT', payload),

  // Watch rules: "watch this local file/folder, upload changes" - always
  // against the local filesystem of the machine the server itself runs
  // on, never a path on this browser's own machine. See WatchesView.vue.
  listWatches: () => request('/api/v1/watches'),
  createWatch: (payload) => jsonRequest('/api/v1/watches', 'POST', payload),
  updateWatch: (id, payload) => jsonRequest(`/api/v1/watches/${encodeURIComponent(id)}`, 'PUT', payload),
  deleteWatch: (id) => request(`/api/v1/watches/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  // Only ever called when the user clicks "Check for updates" in Settings -
  // this is the one outbound call cloudup makes on its own initiative
  // (server-side, to GitHub), and it never happens without that click.
  checkForUpdates: () => request('/api/v1/updates/check'),
}

export { ApiError }
