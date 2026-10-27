<template>
  <section class="browser-shell">
    <header class="browser-chrome">
      <div class="browser-tools">
        <el-tooltip :content="t('shell.back')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
          <el-button :icon="ArrowLeft" circle :disabled="!canBack" @click="goBack" />
        </el-tooltip>
        <el-tooltip :content="t('shell.forward')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
          <el-button :icon="ArrowRight" circle :disabled="!canForward" @click="goForward" />
        </el-tooltip>
        <el-tooltip :content="t('shell.reload')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
          <el-button :icon="Refresh" circle @click="reloadActive" />
        </el-tooltip>
        <form class="address-form" @submit.prevent="navigateActive(address)">
          <el-input v-model="address" :placeholder="t('shell.addressPlaceholder')" />
        </form>
        <el-tooltip :content="t('shell.downloads')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
          <el-button :icon="Download" circle @click="openDownloadsPanel" />
        </el-tooltip>
      </div>
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
          <span>{{ tabLabel(tab) }}</span>
          <el-icon v-if="!tab.pinned" class="browser-tab-close" @click.stop="closeTab(tab.id)"><Close /></el-icon>
        </button>
        <el-tooltip :content="t('shell.newTab')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
          <el-button :icon="Plus" circle @click="createTab('', 'about:blank')" />
        </el-tooltip>
      </div>
    </header>

    <main class="browser-pages">
      <section v-for="tab in tabs" :key="tab.id" class="browser-page" :class="{ active: tab.id === activeID }">
        <div v-if="tab.url === 'about:blank'" class="blank-page">
          <strong>{{ t('shell.blankTitle') }}</strong>
          <p class="blank-hint">{{ t('shell.blankHint') }}</p>
          <div class="quick-links">
            <el-button :icon="VideoPlay" type="primary" @click="navigateTab(tab.id, '/')">{{ t('app.launcher') }}</el-button>
            <el-button :icon="Monitor" @click="navigateTab(tab.id, '/terminal')">{{ t('app.terminal') }}</el-button>
            <el-button :icon="FolderOpened" @click="navigateTab(tab.id, '/sftp')">{{ t('app.sftp') }}</el-button>
            <el-button :icon="Operation" @click="navigateTab(tab.id, '/launcher-logs')">{{ t('app.backend') }}</el-button>
          </div>
        </div>
        <iframe v-else :ref="el => setTabFrame(tab.id, el)" :src="tab.url" :title="tabLabel(tab)" @load="syncLoadedURL(tab.id)" />
      </section>
    </main>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { ArrowLeft, ArrowRight, Close, Download, FolderOpened, Monitor, Operation, Plus, Refresh, VideoPlay } from '@element-plus/icons-vue'
import { t } from '../i18n'

type PageTitleKey = 'app.launcher' | 'app.terminal' | 'app.sftp' | 'app.backend' | 'shell.blankTitle'

interface BrowserTab {
  id: string
  title: string
  titleKey: PageTitleKey | ''
  url: string
  pinned: boolean
  history: string[]
  historyIndex: number
}

interface TabTitle {
  title: string
  titleKey: PageTitleKey | ''
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
const currentSessionURL = ref('')
const currentSessionOpenSeq = ref(0)
const lastSessionURL = ref('')
const lastSessionOpenSeq = ref(0)
const dismissedSessionURL = ref('')
const dismissedSessionOpenSeq = ref(0)
// Highest open_seq the shell has ever observed. open_seq is a process-wide
// monotonic counter that only advances on an explicit Start (including a
// restart-while-running); switching the launcher's active profile merely
// re-promotes an already-running session and does not change its open_seq.
// Gating auto-open on "open_seq advanced past the high-water mark" therefore
// opens a tab only for a genuine Start, never because the user selected
// another running profile.
let maxSeenOpenSeq = 0
let suppressAutoOpenUntil = 0
let sessionStatusPrimed = false
let nextID = 1
let pollTimer: number | undefined
let pollInFlight = false
let disposed = false

const tabFrames = new Map<string, HTMLIFrameElement>()
const reloadTimers = new Map<string, number>()
// A page is only addressable once it announces it is ready: booting takes an
// async settings fetch, so the iframe load event fires before it can answer.
const readyTabs = new Set<string>()
const pendingSftpProfiles = new Map<string, string[]>()

function setTabFrame(id: string, element: unknown) {
  if (element instanceof HTMLIFrameElement) tabFrames.set(id, element)
  else tabFrames.delete(id)
}

function forgetTab(id: string) {
  tabFrames.delete(id)
  readyTabs.delete(id)
  pendingSftpProfiles.delete(id)
  const reloadTimer = reloadTimers.get(id)
  if (reloadTimer !== undefined) {
    window.clearTimeout(reloadTimer)
    reloadTimers.delete(id)
  }
}

function postToTab(id: string, message: Record<string, unknown>) {
  tabFrames.get(id)?.contentWindow?.postMessage(message, window.location.origin)
}

function markTabReady(source: Window | null) {
  const tab = tabs.find(item => tabFrames.get(item.id)?.contentWindow === source)
  if (!tab) return
  readyTabs.add(tab.id)
  const queued = pendingSftpProfiles.get(tab.id)
  if (!queued) return
  pendingSftpProfiles.delete(tab.id)
  queued.forEach(profileID => postToTab(tab.id, { type: 'chemssh-sftp:open-profile', profile: profileID }))
}

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

// A shell tab must never point back at the shell itself, so same-origin
// /shell addresses fall back to the launcher page.
function normalizeShellURL(value: string) {
  if (!value || value === 'about:blank') return value
  try {
    const parsed = new URL(value, window.location.origin)
    if (parsed.origin !== window.location.origin) return value
    if (parsed.pathname === '/shell' || parsed.pathname === '/shell/') {
      return new URL('/', parsed.origin).href
    }
  } catch {
    // Leave malformed addresses for the iframe to report.
  }
  return value
}

const embeddableSchemes = ['http:', 'https:']

function absoluteURL(value: string) {
  const text = normalizeAddressInput(value)
  if (!text) return 'about:blank'
  if (text === 'about:blank') return text
  const scheme = text.match(/^([a-zA-Z][a-zA-Z\d+.-]*:)/)?.[1].toLowerCase()
  // An iframe src accepts javascript:, data:, file: and friends; the shell only
  // ever embeds web pages, so any other scheme is dropped instead of loaded.
  if (scheme && !embeddableSchemes.includes(scheme)) return 'about:blank'
  let finalURL = ''
  if (scheme) {
    finalURL = text
  } else if (text.startsWith('/')) {
    finalURL = new URL(text, window.location.origin).href
  } else if (looksLikeNetworkAddress(text)) {
    finalURL = `${defaultSchemeForAddress(text)}://${text}`
  } else {
    finalURL = new URL(text, window.location.origin).href
  }
  return normalizeShellURL(finalURL)
}

function titleForURL(url: string): TabTitle {
  if (url === 'about:blank') return { title: '', titleKey: 'shell.blankTitle' }
  try {
    const parsed = new URL(url)
    if (parsed.origin === window.location.origin) {
      if (parsed.pathname === '/') return { title: '', titleKey: 'app.launcher' }
      if (parsed.pathname === '/terminal') return { title: '', titleKey: 'app.terminal' }
      if (parsed.pathname === '/sftp') return { title: '', titleKey: 'app.sftp' }
      if (parsed.pathname === '/launcher-logs') return { title: '', titleKey: 'app.backend' }
      const parts = parsed.pathname.split('/').filter(Boolean)
      if (parts.length >= 2 && parts[1] === 'chemssh') return { title: decodeURIComponent(parts[0]), titleKey: '' }
    }
    return { title: parsed.hostname || url, titleKey: '' }
  } catch {
    return { title: url, titleKey: '' }
  }
}

function tabLabel(tab: BrowserTab) {
  return tab.titleKey ? t(tab.titleKey) : tab.title
}

function applyTabTitle(tab: BrowserTab, preferredTitle = '') {
  const meta = titleForURL(tab.url)
  tab.titleKey = meta.titleKey
  tab.title = meta.titleKey ? '' : (preferredTitle || meta.title)
}

function createTab(title: string, url: string, pinned = false, focus = true) {
  const finalURL = absoluteURL(url)
  const tab: BrowserTab = {
    id: `tab-${nextID++}`,
    title: '',
    titleKey: '',
    url: finalURL,
    pinned,
    history: [finalURL],
    historyIndex: 0
  }
  applyTabTitle(tab, title)
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
  rememberClosedSessionTab(tab.url)
  const orderIndex = tabOrder.value.indexOf(id)
  const nextActiveID = orderIndex >= 0
    ? (tabOrder.value[orderIndex - 1] || tabOrder.value[orderIndex + 1] || '')
    : (tabs[Math.max(0, index - 1)]?.id || tabs[0]?.id || '')
  tabs.splice(index, 1)
  forgetTab(id)
  tabOrder.value = tabOrder.value.filter(tabID => tabID !== id)
  if (activeID.value === id) activate(nextActiveID)
}

function navigateTab(id: string, url: string, addHistory = true) {
  const tab = tabs.find(item => item.id === id)
  if (!tab) return
  const finalURL = absoluteURL(url)
  tab.url = finalURL
  readyTabs.delete(id)
  applyTabTitle(tab)
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
  readyTabs.delete(tab.id)
  pendingSftpProfiles.delete(tab.id)
  tab.url = 'about:blank'
  const previous = reloadTimers.get(tab.id)
  if (previous !== undefined) window.clearTimeout(previous)
  reloadTimers.set(tab.id, window.setTimeout(() => {
    reloadTimers.delete(tab.id)
    if (disposed) return
    tab.url = url
  }, 0))
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
  applyTabTitle(tab)
}

function isSftpTabURL(url: string) {
  try {
    const parsed = new URL(url, window.location.origin)
    return parsed.origin === window.location.origin && parsed.pathname === '/sftp'
  } catch {
    return false
  }
}

// A server profile opens inside the SFTP page that is already on screen, so
// repeated launches grow that page's pane tabs instead of stacking shell tabs.
function openSftpProfile(profileID: string, profileName: string) {
  const sftpTabs = tabs.filter(tab => isSftpTabURL(tab.url))
  const existing = sftpTabs.find(tab => tab.id === activeID.value) || sftpTabs[0]
  if (existing) {
    activate(existing.id)
    if (readyTabs.has(existing.id)) {
      postToTab(existing.id, { type: 'chemssh-sftp:open-profile', profile: profileID })
    } else {
      const queued = pendingSftpProfiles.get(existing.id) || []
      queued.push(profileID)
      pendingSftpProfiles.set(existing.id, queued)
    }
    return
  }
  const url = new URL('/sftp', window.location.origin)
  url.searchParams.set('profile', profileID)
  if (profileName) url.searchParams.set('name', profileName)
  createTab(profileName, url.href)
}

function findTabByURL(url: string) {
  const finalURL = absoluteURL(url)
  return tabs.find(tab => tab.url === finalURL)
}

function rememberClosedSessionTab(url: string) {
  const finalURL = absoluteURL(url)
  if (!currentSessionURL.value || finalURL !== currentSessionURL.value) return
  dismissedSessionURL.value = currentSessionURL.value
  dismissedSessionOpenSeq.value = currentSessionOpenSeq.value
  lastSessionURL.value = currentSessionURL.value
  lastSessionOpenSeq.value = currentSessionOpenSeq.value
}

function isDismissedSession(url: string, openSeq: number) {
  if (!url) return false
  if (openSeq > 0) return openSeq === dismissedSessionOpenSeq.value
  return url === dismissedSessionURL.value
}

function openOrFocus(title: string, url: string) {
  const existing = findTabByURL(url)
  if (existing) {
    if (title && !existing.titleKey) existing.title = title
    activate(existing.id)
    return
  }
  createTab(title, url)
}

async function openDownloadsPanel() {
  if (typeof window.chemsshOpenDownloads !== 'function') return
  await window.chemsshOpenDownloads()
}

async function closeDownloadsPanel() {
  if (typeof window.chemsshCloseDownloads === 'function') await window.chemsshCloseDownloads()
}

async function pollSession() {
  // The 1.5s tick is faster than a stalled /api/session/status can be; two
  // overlapping polls would race on maxSeenOpenSeq and could open a tab twice.
  if (pollInFlight || disposed) return
  pollInFlight = true
  try {
    const res = await fetch('/api/session/status')
    const status = await res.json()
    if (disposed) return
    const url = status.forwarding && status.url ? status.url : ''
    const openSeq = Number(status.open_seq || 0)
    currentSessionURL.value = url
    currentSessionOpenSeq.value = openSeq
    if (!sessionStatusPrimed) {
      sessionStatusPrimed = true
      lastSessionURL.value = url
      lastSessionOpenSeq.value = openSeq
      if (openSeq > maxSeenOpenSeq) maxSeenOpenSeq = openSeq
      return
    }
    // Only a genuine Start advances open_seq past the high-water mark.
    // Re-promoting an already-running session (selecting another profile in the
    // launcher, or another session being promoted after a stop) keeps its
    // open_seq, so this never re-opens a tab just because the user switched
    // which running profile they are viewing or stopped one.
    const shouldOpen = openSeq > maxSeenOpenSeq
    const autoOpenSuppressed = Date.now() < suppressAutoOpenUntil
    // Suppression (set after a stop action) blocks re-opening of an already-
    // seen session, but must NEVER block a genuine Start, whose open_seq has
    // advanced past the high-water mark.
    if (url && autoOpenSuppressed && !shouldOpen) {
      return
    }
    if (url && status.open_browser !== false && shouldOpen && !isDismissedSession(url, openSeq)) {
      lastSessionURL.value = url
      lastSessionOpenSeq.value = openSeq
      openOrFocus(status.name || 'ChemSSH', url)
    }
    if (openSeq > maxSeenOpenSeq) maxSeenOpenSeq = openSeq
    if (!url) {
      lastSessionURL.value = ''
      lastSessionOpenSeq.value = 0
    }
  } catch {
    // Polling should stay quiet while the backend is starting or stopping.
  } finally {
    pollInFlight = false
  }
}

function handleWindowMessage(event: MessageEvent) {
  const data = event.data || {}
  if (data.type === 'chemssh-sftp:ready') {
    markTabReady(event.source as Window | null)
  }
  if (data.type === 'chemssh-launcher:new-tab' && data.url) {
    const finalURL = absoluteURL(data.url)
    openOrFocus(titleForURL(finalURL).title, finalURL)
  }
  if (data.type === 'chemssh-launcher:open-sftp' && data.profile) {
    openSftpProfile(String(data.profile), String(data.name || ''))
  }
  if (data.type === 'chemssh-launcher:close-downloads') closeDownloadsPanel()
  if (data.type === 'chemssh-launcher:suppress-auto-open') suppressAutoOpenUntil = Date.now() + 10000
}

onMounted(() => {
  createTab('', '/', true, true)
  pollSession()
  pollTimer = window.setInterval(pollSession, 1500)
  window.addEventListener('message', handleWindowMessage)
})

onBeforeUnmount(() => {
  disposed = true
  if (pollTimer !== undefined) {
    window.clearInterval(pollTimer)
    pollTimer = undefined
  }
  window.removeEventListener('message', handleWindowMessage)
  reloadTimers.forEach(timer => window.clearTimeout(timer))
  reloadTimers.clear()
})
</script>
