<template>
  <el-config-provider :locale="elementLocale">
    <ShellView v-if="route === 'shell'" />
    <div v-else class="lumen-desktop" :class="{ 'is-embedded': embedded }">
      <div class="lumen-window">
        <header v-if="!embedded" class="lumen-titlebar">
          <div class="lumen-title">
            <el-icon><Lock /></el-icon>
            <span>ChemSSH Launcher</span>
            <span class="lumen-title-sep">/</span>
            <strong>{{ routeTitle }}</strong>
          </div>
          <div class="lumen-titlebar-right">
            <button type="button" class="lumen-palette-trigger" :title="t('app.commandPalette')" @click="openPalette">
              <el-icon><Search /></el-icon>
              <kbd>Ctrl+Shift+P</kbd>
            </button>
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
    <Teleport v-if="route !== 'shell'" to="body">
      <div v-if="paletteOpen" class="lumen-command-palette-backdrop" @mousedown.self="closePalette">
        <div class="lumen-command-palette" role="dialog" aria-modal="true" :aria-label="t('palette.title')">
          <div class="lumen-command-palette-search">
            <el-icon><Search /></el-icon>
            <input
              ref="paletteInput"
              v-model="paletteQuery"
              type="text"
              spellcheck="false"
              autocomplete="off"
              :placeholder="t('palette.placeholder')"
              @keydown="handlePaletteKeydown"
            />
            <kbd>Esc</kbd>
          </div>
          <div v-if="filteredCommands.length" class="lumen-command-palette-list" role="listbox">
            <template v-for="(command, index) in filteredCommands" :key="command.id">
              <div
                v-if="index === 0 || filteredCommands[index - 1].group !== command.group"
                class="lumen-command-palette-group-label"
              >
                {{ t(command.group === 'navigate' ? 'palette.groupNavigate' : 'palette.groupActions') }}
              </div>
              <button
                type="button"
                role="option"
                class="lumen-command-palette-item"
                :class="{ 'is-active': index === paletteIndex }"
                :aria-selected="index === paletteIndex"
                @mousemove="paletteIndex = index"
                @click="runPaletteCommand(command)"
              >
                <span class="lumen-command-palette-icon"><el-icon><component :is="command.icon" /></el-icon></span>
                <span class="lumen-command-palette-label">{{ t(command.labelKey) }}</span>
                <span v-if="command.hint" class="lumen-command-palette-hint">{{ command.hint }}</span>
              </button>
            </template>
          </div>
          <div v-else class="lumen-command-palette-empty">{{ t('palette.noResults') }}</div>
          <div class="lumen-command-palette-footer">{{ t('palette.navigateHint') }}</div>
        </div>
      </div>
    </Teleport>
  </el-config-provider>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch, type Component } from 'vue'
import { ElMessage } from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import en from 'element-plus/es/locale/lang/en'
import { Connection, Folder, FolderOpened, House, Lock, Search, Setting } from '@element-plus/icons-vue'
import BackendView from './views/BackendView.vue'
import LauncherView from './views/LauncherView.vue'
import SftpView from './views/SftpView.vue'
import ShellView from './views/ShellView.vue'
import TerminalView from './views/TerminalView.vue'
import { api, postJSON, withLauncherToken } from './api'
import { locale, t } from './i18n'

type RouteName = 'launcher' | 'sftp' | 'backend' | 'shell' | 'terminal'

interface PaletteCommand {
  id: string
  labelKey: Parameters<typeof t>[0]
  group: 'navigate' | 'actions'
  hint?: string
  icon: Component
  run: () => void
}

const VIEW_SHORTCUT_PATHS: Record<string, string> = {
  Digit1: '/',
  Digit2: '/terminal',
  Digit3: '/sftp',
  Digit4: '/launcher-logs'
}

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

const paletteOpen = ref(false)
const paletteQuery = ref('')
const paletteIndex = ref(0)
const paletteInput = ref<HTMLInputElement | null>(null)

const paletteCommands = computed<PaletteCommand[]>(() => [
  { id: 'launcher', labelKey: 'palette.openLauncher', group: 'navigate', hint: 'Ctrl+Shift+1', icon: House, run: () => navigateTo('/') },
  { id: 'terminal', labelKey: 'palette.openTerminal', group: 'navigate', hint: 'Ctrl+Shift+2', icon: Connection, run: () => navigateTo('/terminal') },
  { id: 'sftp', labelKey: 'palette.openSftp', group: 'navigate', hint: 'Ctrl+Shift+3', icon: FolderOpened, run: () => navigateTo('/sftp') },
  { id: 'backend', labelKey: 'palette.openBackend', group: 'navigate', hint: 'Ctrl+Shift+4', icon: Setting, run: () => navigateTo('/launcher-logs') },
  { id: 'open-config-dir', labelKey: 'palette.openConfigDir', group: 'actions', icon: Folder, run: () => { void openConfigDirectory() } }
])

const filteredCommands = computed(() => {
  const query = paletteQuery.value.trim().toLowerCase()
  if (!query) return paletteCommands.value
  return paletteCommands.value.filter(command =>
    t(command.labelKey).toLowerCase().includes(query) ||
    command.id.includes(query) ||
    (command.hint || '').toLowerCase().includes(query)
  )
})

function navigateTo(path: string) {
  closePalette()
  if (location.pathname === path) return
  location.assign(path)
}

async function openConfigDirectory() {
  try {
    await postJSON('/api/backend/open-config-dir', {})
    ElMessage.success(t('palette.openConfigDirDone'))
  } catch (error) {
    ElMessage.error((error as Error)?.message || t('palette.openConfigDirFailed'))
  }
}

function openPalette() {
  paletteOpen.value = true
  paletteQuery.value = ''
  paletteIndex.value = 0
  void nextTick(() => paletteInput.value?.focus())
}

function closePalette() {
  paletteOpen.value = false
}

function movePaletteSelection(delta: number) {
  const count = filteredCommands.value.length
  if (!count) return
  paletteIndex.value = (paletteIndex.value + delta + count) % count
}

function runPaletteCommand(command: PaletteCommand) {
  closePalette()
  command.run()
}

function handlePaletteKeydown(event: KeyboardEvent) {
  if (event.key === 'ArrowDown') {
    event.preventDefault()
    movePaletteSelection(1)
    return
  }
  if (event.key === 'ArrowUp') {
    event.preventDefault()
    movePaletteSelection(-1)
    return
  }
  if (event.key === 'Enter') {
    event.preventDefault()
    const command = filteredCommands.value[paletteIndex.value]
    if (command) runPaletteCommand(command)
    return
  }
  if (event.key === 'Escape') {
    event.preventDefault()
    closePalette()
  }
}

function handleAppShortcut(event: KeyboardEvent) {
  if (route === 'shell') return
  const mod = event.ctrlKey || event.metaKey
  if (paletteOpen.value) {
    if (event.key === 'Escape') {
      event.preventDefault()
      event.stopPropagation()
      closePalette()
      return
    }
    if (mod && event.shiftKey && !event.altKey && event.key.toLowerCase() === 'p') {
      event.preventDefault()
      event.stopPropagation()
      closePalette()
      return
    }
    if (mod && !event.shiftKey && event.key.toLowerCase() === 'f') event.preventDefault()
    return
  }
  const target = event.target
  if (target instanceof HTMLElement && target.closest('.lumen-command-palette')) return
  if (mod && event.shiftKey && !event.altKey && event.key.toLowerCase() === 'p') {
    event.preventDefault()
    event.stopPropagation()
    openPalette()
    return
  }
  if (mod && event.shiftKey && !event.altKey) {
    const path = VIEW_SHORTCUT_PATHS[event.code]
    if (!path) return
    event.preventDefault()
    event.stopPropagation()
    navigateTo(path)
  }
}

watch(paletteQuery, () => {
  paletteIndex.value = 0
})

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
  window.addEventListener('keydown', handleAppShortcut, true)
})

onBeforeUnmount(() => {
  if (heartbeatTimer !== null) window.clearInterval(heartbeatTimer)
  window.removeEventListener('pagehide', notifyBrowserClientClosed)
  window.removeEventListener('keydown', handleAppShortcut, true)
  notifyBrowserClientClosed()
})
</script>
