<template>
  <section class="browser-shell">
    <header class="browser-chrome">
      <div class="browser-tabs">
        <button
          v-for="tab in orderedTabs"
          :key="tab.id"
          class="browser-tab"
          :class="{ active: tab.id === activeID, pinned: tab.pinned }"
          type="button"
          draggable="true"
          @click="activate(tab.id)"
          @dragstart="draggingID = tab.id"
          @dragover.prevent="moveBefore(tab.id)"
          @dragend="draggingID = ''"
        >
          <span>{{ tab.title }}</span>
          <el-icon v-if="!tab.pinned" class="browser-tab-close" @click.stop="closeTab(tab.id)"><Close /></el-icon>
        </button>
        <el-tooltip content="新建标签页" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
          <el-button :icon="Plus" circle @click="createTab('新标签页', 'about:blank')" />
        </el-tooltip>
      </div>
      <div class="browser-tools">
        <el-tooltip content="后退" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
          <el-button :icon="ArrowLeft" circle :disabled="!canBack" @click="goBack" />
        </el-tooltip>
        <el-tooltip content="前进" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
          <el-button :icon="ArrowRight" circle :disabled="!canForward" @click="goForward" />
        </el-tooltip>
        <el-tooltip content="刷新" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
          <el-button :icon="Refresh" circle @click="reloadActive" />
        </el-tooltip>
        <form class="address-form" @submit.prevent="navigateActive(address)">
          <el-input v-model="address" placeholder="输入地址或本地路径" />
        </form>
        <el-tooltip content="下载历史" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
          <el-button :icon="Download" circle @click="openDownloadsPanel" />
        </el-tooltip>
      </div>
    </header>

    <main class="browser-pages">
      <section v-for="tab in tabs" :key="tab.id" class="browser-page" :class="{ active: tab.id === activeID }">
        <div v-if="tab.url === 'about:blank'" class="blank-page">
          <strong>新标签页</strong>
          <div class="quick-links">
            <el-button :icon="VideoPlay" type="primary" @click="navigateTab(tab.id, '/')">启动器</el-button>
            <el-button :icon="FolderOpened" @click="navigateTab(tab.id, '/sftp')">SFTP</el-button>
            <el-button :icon="Operation" @click="navigateTab(tab.id, '/launcher-logs')">后台</el-button>
          </div>
        </div>
        <iframe v-else :src="tab.url" :title="tab.title" @load="syncLoadedURL(tab.id)" />
      </section>
    </main>
    <div v-if="downloadsBackdrop" class="downloads-backdrop" @pointerdown="closeDownloadsPanel" @contextmenu.prevent="closeDownloadsPanel" />
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ArrowLeft, ArrowRight, Close, Download, FolderOpened, Operation, Plus, Refresh, VideoPlay } from '@element-plus/icons-vue'

interface BrowserTab {
  id: string
  title: string
  url: string
  pinned: boolean
  history: string[]
  historyIndex: number
}

declare global {
  interface Window {
    chemsshOpenDownloads?: () => Promise<void>
    chemsshCloseDownloads?: () => Promise<void>
  }
}

const tabs = reactive<BrowserTab[]>([])
const tabOrder = ref<string[]>([])
const activeID = ref('')
const address = ref('')
const draggingID = ref('')
const lastSessionURL = ref('')
const lastSessionOpenSeq = ref(0)
const downloadsBackdrop = ref(false)
let nextID = 1

const activeTab = computed(() => tabs.find(tab => tab.id === activeID.value))
const orderedTabs = computed(() => {
  const byID = new Map(tabs.map(tab => [tab.id, tab]))
  const ordered = tabOrder.value
    .map(id => byID.get(id))
    .filter((tab): tab is BrowserTab => Boolean(tab))
  const missing = tabs.filter(tab => !tabOrder.value.includes(tab.id))
  return [...ordered, ...missing]
})
const canBack = computed(() => Boolean(activeTab.value && activeTab.value.historyIndex > 0))
const canForward = computed(() => Boolean(activeTab.value && activeTab.value.historyIndex < activeTab.value.history.length - 1))

function normalizeAddressInput(value: string) {
  return String(value || '').trim().replace(/[\u3002\uff0e\uff61]/g, '.')
}

function looksLikeNetworkAddress(text: string) {
  return text.includes('.') || text.includes(':')
}

function defaultSchemeForAddress(text: string) {
  const host = text
    .split(/[/?#]/, 1)[0]
    .replace(/^\[/, '')
    .replace(/\]$/, '')
    .split(':', 1)[0]
    .toLowerCase()
  return host.startsWith('127.') ? 'http' : 'https'
}

function absoluteURL(value: string) {
  const text = normalizeAddressInput(value)
  if (!text) return 'about:blank'
  if (text === 'about:blank') return text
  if (/^[a-zA-Z][a-zA-Z\d+.-]*:/.test(text)) return text
  if (text.startsWith('/')) return new URL(text, window.location.origin).href
  if (looksLikeNetworkAddress(text)) return `${defaultSchemeForAddress(text)}://${text}`
  return new URL(text, window.location.origin).href
}

function shortTitle(url: string) {
  if (url === 'about:blank') return '新标签页'
  try {
    const parsed = new URL(url)
    if (parsed.origin === window.location.origin) {
      if (parsed.pathname === '/') return '启动器'
      if (parsed.pathname === '/sftp') return 'SFTP'
      if (parsed.pathname === '/launcher-logs') return '后台'
      const parts = parsed.pathname.split('/').filter(Boolean)
      if (parts.length >= 2 && parts[1] === 'chemssh') return decodeURIComponent(parts[0])
    }
    return parsed.hostname || url
  } catch {
    return url
  }
}

function createTab(title: string, url: string, pinned = false, focus = true) {
  const finalURL = absoluteURL(url)
  const tab: BrowserTab = {
    id: `tab-${nextID++}`,
    title: title || shortTitle(finalURL),
    url: finalURL,
    pinned,
    history: [finalURL],
    historyIndex: 0
  }
  tabs.push(tab)
  tabOrder.value.push(tab.id)
  if (focus) activate(tab.id)
  return tab.id
}

function activate(id: string) {
  const tab = tabs.find(item => item.id === id)
  if (!tab) return
  activeID.value = id
  address.value = tab.url === 'about:blank' ? '' : tab.url
}

function closeTab(id: string) {
  const index = tabs.findIndex(tab => tab.id === id)
  const tab = tabs[index]
  if (!tab || tab.pinned) return
  const orderIndex = tabOrder.value.indexOf(id)
  const nextActiveID = orderIndex >= 0
    ? (tabOrder.value[orderIndex - 1] || tabOrder.value[orderIndex + 1] || '')
    : (tabs[Math.max(0, index - 1)]?.id || tabs[0]?.id || '')
  tabs.splice(index, 1)
  tabOrder.value = tabOrder.value.filter(tabID => tabID !== id)
  if (activeID.value === id) activate(nextActiveID)
}

function navigateTab(id: string, url: string, addHistory = true) {
  const tab = tabs.find(item => item.id === id)
  if (!tab) return
  const finalURL = absoluteURL(url)
  tab.url = finalURL
  tab.title = shortTitle(finalURL)
  if (addHistory) {
    tab.history = tab.history.slice(0, tab.historyIndex + 1)
    tab.history.push(finalURL)
    tab.historyIndex = tab.history.length - 1
  }
  if (id === activeID.value) address.value = finalURL === 'about:blank' ? '' : finalURL
}

function navigateActive(url: string) {
  if (activeID.value) navigateTab(activeID.value, url)
}

function reloadActive() {
  const tab = activeTab.value
  if (!tab) return
  const url = tab.url
  tab.url = 'about:blank'
  window.setTimeout(() => {
    tab.url = url
  }, 0)
}

function goBack() {
  const tab = activeTab.value
  if (!tab || tab.historyIndex <= 0) return
  tab.historyIndex -= 1
  navigateTab(tab.id, tab.history[tab.historyIndex], false)
}

function goForward() {
  const tab = activeTab.value
  if (!tab || tab.historyIndex >= tab.history.length - 1) return
  tab.historyIndex += 1
  navigateTab(tab.id, tab.history[tab.historyIndex], false)
}

function moveBefore(targetID: string) {
  if (!draggingID.value || draggingID.value === targetID) return
  const from = tabOrder.value.findIndex(id => id === draggingID.value)
  const to = tabOrder.value.findIndex(id => id === targetID)
  if (from < 0 || to < 0) return
  const nextOrder = [...tabOrder.value]
  const [tabID] = nextOrder.splice(from, 1)
  nextOrder.splice(to, 0, tabID)
  tabOrder.value = nextOrder
}

function syncLoadedURL(id: string) {
  const tab = tabs.find(item => item.id === id)
  if (!tab) return
  tab.title = shortTitle(tab.url)
}

function findTabByURL(url: string) {
  const finalURL = absoluteURL(url)
  return tabs.find(tab => tab.url === finalURL)
}

function openOrFocus(title: string, url: string) {
  const existing = findTabByURL(url)
  if (existing) {
    existing.title = title || existing.title
    activate(existing.id)
    return
  }
  createTab(title, url)
}

async function openDownloadsPanel() {
  if (typeof window.chemsshOpenDownloads !== 'function') return
  await window.chemsshOpenDownloads()
  downloadsBackdrop.value = true
}

async function closeDownloadsPanel() {
  downloadsBackdrop.value = false
  if (typeof window.chemsshCloseDownloads === 'function') await window.chemsshCloseDownloads()
}

async function pollSession() {
  try {
    const res = await fetch('/api/session/status')
    const status = await res.json()
    const url = status.forwarding && status.url ? status.url : ''
    const openSeq = Number(status.open_seq || 0)
    const shouldOpen = openSeq > 0
      ? openSeq !== lastSessionOpenSeq.value
      : url !== lastSessionURL.value
    if (url && status.open_browser !== false && shouldOpen) {
      lastSessionURL.value = url
      lastSessionOpenSeq.value = openSeq
      openOrFocus(status.name || 'ChemSSH', url)
    }
    if (!url) {
      lastSessionURL.value = ''
      lastSessionOpenSeq.value = 0
    }
  } catch {
    // Polling should stay quiet while the backend is starting or stopping.
  }
}

onMounted(() => {
  createTab('启动器', '/', true, true)
  pollSession()
  window.setInterval(pollSession, 1500)
  window.addEventListener('message', event => {
    const data = event.data || {}
    if (data.type === 'chemssh-launcher:new-tab' && data.url) openOrFocus(shortTitle(data.url), data.url)
    if (data.type === 'chemssh-launcher:close-downloads') closeDownloadsPanel()
  })
})
</script>
