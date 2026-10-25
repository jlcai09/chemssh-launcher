<template>
  <el-config-provider :locale="elementLocale">
    <ShellView v-if="route === 'shell'" />
    <div v-else class="lumen-desktop" :class="{ 'is-embedded': embedded }">
      <div class="lumen-window">
        <header v-if="!embedded" class="lumen-titlebar">
          <div class="lumen-lights" aria-hidden="true">
            <span class="lumen-light close" />
            <span class="lumen-light minimize" />
            <span class="lumen-light maximize" />
          </div>
          <div class="lumen-title">
            <el-icon><Lock /></el-icon>
            <span>ChemSSH Launcher</span>
            <span class="lumen-title-sep">/</span>
            <strong>{{ routeTitle }}</strong>
          </div>
          <div class="lumen-titlebar-right">
            <span v-if="serviceOnline" class="lumen-status-pill"><i />{{ t('app.serviceOnline') }}</span>
          </div>
        </header>
        <div class="app-shell">
          <main class="app-main">
            <LauncherView v-if="route === 'launcher'" />
            <TerminalView v-else-if="route === 'terminal'" :active="true" />
            <SftpView v-else-if="route === 'sftp'" />
            <BackendView v-else-if="route === 'backend'" />
          </main>
        </div>
      </div>
    </div>
  </el-config-provider>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import en from 'element-plus/es/locale/lang/en'
import { Lock } from '@element-plus/icons-vue'
import BackendView from './views/BackendView.vue'
import LauncherView from './views/LauncherView.vue'
import SftpView from './views/SftpView.vue'
import ShellView from './views/ShellView.vue'
import TerminalView from './views/TerminalView.vue'
import { api, withLauncherToken } from './api'
import { locale, t } from './i18n'

type RouteName = 'launcher' | 'sftp' | 'backend' | 'shell' | 'terminal'

function initialRoute(): RouteName {
  if (location.pathname === '/shell') {
    if (window.parent !== window) {
      // A shell page must never render inside itself.
      history.replaceState(null, '', '/')
      return 'launcher'
    }
    return 'shell'
  }
  if (location.pathname === '/sftp') return 'sftp'
  if (location.pathname === '/terminal') return 'terminal'
  if (location.pathname === '/launcher-logs') return 'backend'
  return 'launcher'
}

const route = initialRoute()
const embedded = window.parent !== window
const elementLocale = computed(() => (locale.value === 'zh' ? zhCn : en))
const serviceOnline = ref(false)
const routeTitle = computed(() => {
  if (route === 'terminal') return t('app.terminal')
  if (route === 'sftp') return t('app.sftp')
  if (route === 'backend') return t('app.backend')
  return t('app.launcher')
})
const browserClientID = `browser-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
let heartbeatTimer: number | null = null

async function sendHeartbeat() {
  try {
    await api('/api/browser-client/heartbeat', {
      method: 'POST',
      body: JSON.stringify({ id: browserClientID })
    })
    serviceOnline.value = true
  } catch {
    serviceOnline.value = false
  }
}

function notifyBrowserClientClosed() {
  const path = `/api/browser-client/close?id=${encodeURIComponent(browserClientID)}`
  fetch(path, { method: 'POST', keepalive: true, headers: withLauncherToken() }).catch(() => undefined)
}

onMounted(async () => {
  await sendHeartbeat()
  heartbeatTimer = window.setInterval(sendHeartbeat, 5000)
  window.addEventListener('pagehide', notifyBrowserClientClosed)
})

onBeforeUnmount(() => {
  if (heartbeatTimer !== null) window.clearInterval(heartbeatTimer)
  window.removeEventListener('pagehide', notifyBrowserClientClosed)
  notifyBrowserClientClosed()
})
</script>
