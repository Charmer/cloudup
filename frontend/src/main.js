import { createApp } from 'vue'
import './style.css'
import App from './App.vue'
import router from './router.js'
import { initI18n } from './i18n.js'

// Load the translation catalog before the first paint so no view flashes
// raw keys; initI18n never rejects (see i18n.js), so a server that is
// unreachable still gets a mounted app with a working Settings page.
initI18n().then(() => {
  createApp(App).use(router).mount('#app')
})
