<script setup>
import { t } from './i18n.js'
</script>

<template>
  <div class="shell">
    <nav class="topnav">
      <span class="brand">{{ t('nav.brand') }}</span>
      <RouterLink to="/connections">{{ t('nav.connections') }}</RouterLink>
      <RouterLink to="/queue">{{ t('nav.queue') }}</RouterLink>
      <RouterLink to="/watches">{{ t('nav.watches') }}</RouterLink>
      <RouterLink to="/history">{{ t('nav.history') }}</RouterLink>
      <RouterLink to="/settings">{{ t('nav.settings') }}</RouterLink>
    </nav>
    <main class="content">
      <RouterView />
    </main>
  </div>
</template>

<style scoped>
.shell {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
}
.topnav {
  display: flex;
  align-items: center;
  gap: 1.25rem;
  padding: 0.75rem 1.5rem;
  border-bottom: 1px solid var(--border);
  background: var(--surface);
}
.brand {
  font-weight: 600;
  margin-right: 1rem;
}
.topnav a {
  color: var(--text-muted);
  text-decoration: none;
  padding: 0.25rem 0;
  border-bottom: 2px solid transparent;
}
.topnav a.router-link-active {
  color: var(--text);
  border-bottom-color: var(--accent);
}
.content {
  flex: 1;
  padding: 1.5rem;
  max-width: 960px;
  width: 100%;
  margin: 0 auto;
  box-sizing: border-box;
}
</style>
