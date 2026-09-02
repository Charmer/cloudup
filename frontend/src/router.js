import { createRouter, createWebHashHistory } from 'vue-router'
import ConnectionsView from './views/ConnectionsView.vue'
import QueueView from './views/QueueView.vue'
import WatchesView from './views/WatchesView.vue'
import HistoryView from './views/HistoryView.vue'
import SettingsView from './views/SettingsView.vue'

// Hash history so this can be served as a plain static file (file:// or any
// dumb static server) without needing server-side rewrite rules - fitting
// for a frontend that's deliberately decoupled from cmd/server.
export default createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', redirect: '/connections' },
    { path: '/connections', name: 'connections', component: ConnectionsView },
    { path: '/queue', name: 'queue', component: QueueView },
    { path: '/watches', name: 'watches', component: WatchesView },
    { path: '/history', name: 'history', component: HistoryView },
    { path: '/settings', name: 'settings', component: SettingsView },
  ],
})
