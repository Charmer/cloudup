// Minimal UI localization layer - deliberately no vue-i18n (or any other
// npm dependency): the server does all the hard parts already.
//
// GET /api/v1/languages/{code} always returns a COMPLETE flat map of
// key -> string (internal/i18n fills anything a catalog is missing from
// English at load time, and an unknown code falls back instead of
// erroring), so there is no fallback/merge logic to write here - a plain
// lookup is always safe. A miss can therefore only mean the UI used a key
// that does not exist in en.json at all, which is a bug; we render the key
// itself so it is loudly visible rather than an invisible empty label.
//
// Persistence: Settings.language on the server is the source of truth (it
// is shared by every client). The chosen code is ALSO mirrored into
// localStorage purely as a first-paint hint, so a reload can fetch the
// catalog straight away instead of waiting for GET /settings first and
// only then fetching the catalog (two sequential round-trips, with the UI
// showing English in between). The server still wins: initI18n() always
// re-reads Settings.language afterwards and switches if it differs.
import { ref } from 'vue'
import { api } from './api.js'

const STORAGE_KEY = 'cloudup.language'
const FALLBACK = 'en'

const catalog = ref({})
export const currentLanguage = ref(hint() || FALLBACK)

function hint() {
  try {
    return localStorage.getItem(STORAGE_KEY) || ''
  } catch {
    return ''
  }
}

function rememberHint(code) {
  try {
    localStorage.setItem(STORAGE_KEY, code)
  } catch {
    // Private mode / storage disabled - the server copy still persists.
  }
}

// Reactive lookup: templates using t('x') re-render when the catalog is
// swapped, so switching language needs no reload.
export function t(key) {
  const v = catalog.value[key]
  return typeof v === 'string' ? v : key
}

export async function loadLanguage(code) {
  const strings = await api.language(code)
  // __name__ is catalog metadata (the language's own name), never a UI
  // string - drop it so no view can accidentally render it.
  delete strings.__name__
  catalog.value = strings
  currentLanguage.value = code
  rememberHint(code)
  if (typeof document !== 'undefined') document.documentElement.lang = code
}

// Called once before mounting the app: paint in the remembered language,
// then reconcile with whatever the server has stored.
export async function initI18n() {
  try {
    await loadLanguage(currentLanguage.value)
  } catch {
    // Server unreachable or a stale remembered code - t() falls back to
    // rendering keys, and Settings still lets the user fix the connection.
  }
  try {
    const settings = await api.getSettings()
    if (settings?.language && settings.language !== currentLanguage.value) {
      await loadLanguage(settings.language)
    }
  } catch {
    // Keep going with the remembered language.
  }
}
