<template>
  <section class="terminal-view">
    <aside class="terminal-sessions">
      <header class="terminal-sessions-title">
        <span>{{ t('terminal.servers') }}</span>
        <span class="terminal-sessions-count">{{ remoteProfiles.length }}</span>
      </header>
      <div class="terminal-session-list" role="list">
        <button
          v-for="profile in remoteProfiles"
          :key="profile.id"
          class="terminal-session"
          :class="{ 'is-active': profile.id === activeProfile?.id }"
          type="button"
          :title="profileSessionTitle(profile)"
          @click="openProfileSession(profile)"
        >
          <span class="terminal-session-icon"><el-icon><Connection /></el-icon></span>
          <span class="terminal-session-text">
            <strong>{{ profile.name || profile.ssh_host || profile.id }}</strong>
            <small>{{ profileMeta(profile) }}</small>
          </span>
          <span
            v-if="profileHasOpenTab(profile.id)"
            class="terminal-session-dot"
            :class="{ 'is-live': profileIsConnected(profile.id) }"
          />
        </button>
        <div v-if="!remoteProfiles.length" class="terminal-sessions-empty">
          <span>{{ t('terminal.noProfiles') }}</span>
          <small>{{ t('terminal.noProfilesHint') }}</small>
        </div>
      </div>
    </aside>
    <div class="terminal-card">
      <div class="terminal-tabs">
        <button
          v-for="tab in tabs"
          :key="tab.id"
          class="terminal-tab"
          :class="{
            active: tab.id === activeTabID,
            'is-tab-dragging': isTabDragging(tab.id),
            'is-tab-drop-before': isTabDropTarget(tab.id, 'before'),
            'is-tab-drop-after': isTabDropTarget(tab.id, 'after')
          }"
          type="button"
          draggable="true"
          :title="tabTitle(tab)"
          @click="activateTab(tab.id)"
          @dblclick="duplicateTab(tab)"
          @contextmenu.prevent="openTabMenu(tab, $event)"
          @dragstart="startTabDrag(tab.id, $event)"
          @dragover="handleTabDragOver(tab.id, $event)"
          @dragleave="handleTabDragLeave(tab.id, $event)"
          @drop.prevent.stop="handleTabDrop(tab.id)"
          @dragend="finishTabDrag"
        >
          <span class="terminal-tab-dot" :class="`is-${tab.status}`" />
          <span class="terminal-tab-label">{{ tabLabel(tab) }}</span>
          <span class="terminal-tab-close" role="button" tabindex="-1" @click.stop="closeTab(tab.id)" @dblclick.stop>
            <el-icon><Close /></el-icon>
          </span>
        </button>
        <el-tooltip :content="t('terminal.newTab')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
          <el-button class="terminal-add-tab" :icon="Plus" circle @click="createTab()" />
        </el-tooltip>
      </div>
      <div
        ref="stageRef"
        class="terminal-stage"
        @dragenter="handleFileDragEnter"
        @dragover="handleFileDragOver"
        @dragleave="handleFileDragLeave"
        @drop="handleFileDrop"
      >
        <div
          v-for="tab in tabs"
          v-show="tab.id === activeTabID"
          :key="tab.id"
          :ref="el => setTabHost(tab.id, el)"
          class="terminal-host"
        />
        <div v-if="!tabs.length" class="terminal-placeholder">
          <strong>{{ t('terminal.noTabs') }}</strong>
          <span>{{ t('terminal.noTabsHint') }}</span>
        </div>
        <div v-else-if="!remoteProfiles.length" class="terminal-placeholder">
          <strong>{{ t('terminal.noProfiles') }}</strong>
          <span>{{ t('terminal.noProfilesHint') }}</span>
        </div>
        <div v-if="dropActive" class="terminal-drop-overlay">
          <el-icon><UploadFilled /></el-icon>
          <strong>{{ !connected ? t('terminal.connectFirst') : uploading ? t('terminal.uploadBusy') : t('terminal.dropToUpload') }}</strong>
          <span v-if="connected && !uploading">{{ t('terminal.uploadTarget', { path: uploadTargetLabel }) }}</span>
        </div>
      </div>
      <footer class="terminal-footer">
        <div class="terminal-footer-actions">
          <el-tooltip :content="t('terminal.clear')" placement="top" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button :icon="Delete" circle :disabled="!activeTab" @click="clearActiveTab" />
          </el-tooltip>
          <span
            class="terminal-upload-ring"
            :class="{ 'is-active': uploading }"
            :style="{ '--terminal-upload-progress': uploadPercent }"
          >
            <el-tooltip :content="t('terminal.upload')" placement="top" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
              <el-button :icon="Upload" circle :disabled="!connected || uploading" @click="pickAndUploadManually" />
            </el-tooltip>
            <span v-if="uploading" class="terminal-upload-percent">{{ uploadPercent }}%</span>
          </span>
        </div>
        <div class="terminal-footer-address" :title="connectionAddress">
          {{ connectionAddress || t('terminal.noSession') }}
        </div>
        <div class="terminal-status" :class="`is-${statusKind}`">
          <span class="state-dot" :class="{ running: statusKind === 'connected' }" />
          <span>{{ statusText }}</span>
          <el-tooltip v-if="connected" :content="t('terminal.disconnect')" placement="top" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button class="terminal-footer-disconnect" :icon="SwitchButton" circle @click="disconnectActiveTab" />
          </el-tooltip>
        </div>
      </footer>
    </div>
    <div
      v-if="tabMenu.visible"
      class="terminal-tab-menu"
      :style="{ left: `${tabMenu.x}px`, top: `${tabMenu.y}px` }"
      role="menu"
    >
      <button type="button" role="menuitem" @click="runTabAction('duplicate')">{{ t('terminal.tabDuplicate') }}</button>
      <button type="button" role="menuitem" @click="runTabAction('close')">{{ t('terminal.tabClose') }}</button>
      <button type="button" role="menuitem" class="is-danger" @click="runTabAction('closeAll')">{{ t('terminal.tabCloseAll') }}</button>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Close, Connection, Delete, Plus, SwitchButton, Upload, UploadFilled } from '@element-plus/icons-vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { api, APIError, postJSON, withLauncherToken, type Profile } from '../api'
import { confirmHostKey, hostKeyFromError } from '../hostKey'
import {
  collectDropUploadEntries,
  filesToUploadEntries,
  hasFileDrag,
  normalizeUploadEntries,
  type UploadEntry
} from '../uploadEntries'
import { t } from '../i18n'

const props = defineProps<{ active: boolean }>()

const TERMINAL_PROFILE_KEY = 'chemssh-launcher.terminal-profile'

type TerminalStatus = 'idle' | 'connecting' | 'connected' | 'error'
type TabAction = 'duplicate' | 'close' | 'closeAll'

interface TerminalTab {
  id: string
  profileID: string
  status: TerminalStatus
  cwd: string
  uploading: boolean
  uploadPercent: number
}

interface TerminalRuntime {
  term: Terminal
  fitAddon: FitAddon
  sessionID: string
  outputAbort: AbortController | null
  pendingInput: string
  inputFlushing: boolean
  resizeTimer: number | null
  lastCols: number
  lastRows: number
  controlBuffer: string
  zmodemProbe: string
  zmodemIntercepted: boolean
  sftpSessionID: string
  sftpProfileID: string
  sftpReleasePending: boolean
  pendingCd: string
  disposed: boolean
}

const profiles = ref<Profile[]>([])
const tabs = ref<TerminalTab[]>([])
const activeTabID = ref('')
const dropActive = ref(false)
const stageRef = ref<HTMLElement | null>(null)
const tabMenu = reactive({ visible: false, x: 0, y: 0, tabID: '' })
const tabDrag = reactive({ active: false, tabID: '', targetID: '', position: 'before' as 'before' | 'after' })

const runtimes = new Map<string, TerminalRuntime>()
const hosts = new Map<string, HTMLElement>()
let lastProfileID = localStorage.getItem(TERMINAL_PROFILE_KEY) || ''
let resizeObserver: ResizeObserver | null = null
let tabSeq = 0

const ZMODEM_SIGNATURES = [
  'rz waiting to receive',
  '**b00000000000000',
  '**b0100',
  '**\x18b00000000000000',
  '**\x18b0100'
]

const remoteProfiles = computed(() => profiles.value.filter(profile => profile.kind !== 'local'))
const activeTab = computed(() => tabs.value.find(tab => tab.id === activeTabID.value) || null)
const activeProfile = computed(() => remoteProfiles.value.find(profile => profile.id === activeTab.value?.profileID) || null)
const connectionAddress = computed(() => activeProfile.value ? profileSessionURL(activeProfile.value) : '')
const connected = computed(() => activeTab.value?.status === 'connected')
const statusKind = computed<TerminalStatus>(() => activeTab.value?.status || 'idle')
const currentDir = computed(() => activeTab.value?.cwd || '')
const uploading = computed(() => activeTab.value?.uploading || false)
const uploadPercent = computed(() => activeTab.value?.uploadPercent || 0)
const uploadTargetLabel = computed(() => currentDir.value.trim() || t('terminal.remoteHome'))
const statusText = computed(() => {
  switch (statusKind.value) {
    case 'connecting':
      return t('terminal.connecting')
    case 'connected':
      return t('terminal.connected')
    case 'error':
      return t('terminal.statusError')
    default:
      return t('terminal.notConnected')
  }
})

function persistProfile(value: string) {
  try {
    localStorage.setItem(TERMINAL_PROFILE_KEY, value)
  } catch {
    // Selection persistence is best-effort.
  }
}

function profileSessionURL(profile: Profile) {
  const host = profile.ssh_host || '?'
  const user = profile.ssh_user ? `${profile.ssh_user}@` : ''
  const port = profile.ssh_port ? `:${profile.ssh_port}` : ''
  return `ssh://${user}${host}${port}`
}

function profileMeta(profile: Profile) {
  const target = `${profile.ssh_user || '?'}@${profile.ssh_host || '?'}`
  return profile.ssh_port && profile.ssh_port !== 22 ? `${target}:${profile.ssh_port}` : target
}

function profileSessionTitle(profile: Profile) {
  return `${profile.name || profile.ssh_host || profile.id} — ${profileMeta(profile)}\n${t('terminal.sessionHint')}`
}

function profileHasOpenTab(profileID: string) {
  return tabs.value.some(tab => tab.profileID === profileID && (tab.status === 'connected' || tab.status === 'connecting'))
}

function profileIsConnected(profileID: string) {
  return tabs.value.some(tab => tab.profileID === profileID && tab.status === 'connected')
}

async function openProfileSession(profile: Profile) {
  const openTab = tabs.value.find(tab => tab.profileID === profile.id && (tab.status === 'connected' || tab.status === 'connecting'))
  if (openTab) {
    await activateTab(openTab.id)
    return
  }
  const current = activeTab.value
  if (current && current.status === 'idle') {
    current.profileID = profile.id
    lastProfileID = profile.id
    persistProfile(profile.id)
    await nextTick()
    await connect(current)
    return
  }
  const tab = await createTab(profile.id)
  await connect(tab)
}

function profileName(profileID: string) {
  const profile = profiles.value.find(item => item.id === profileID)
  return profile?.name || profile?.ssh_host || profileID || 'SSH'
}

function pathBasename(path: string) {
  const trimmed = path.replace(/[\\/]+$/, '')
  if (!trimmed) return ''
  return trimmed.slice(Math.max(trimmed.lastIndexOf('/'), trimmed.lastIndexOf('\\')) + 1)
}

function tabLabel(tab: TerminalTab) {
  const name = profileName(tab.profileID)
  const dir = pathBasename(tab.cwd)
  return dir ? `${name}:${dir}` : name
}

function tabTitle(tab: TerminalTab) {
  const name = profileName(tab.profileID)
  return tab.cwd ? `${name} — ${tab.cwd}` : name
}

function errorMessage(error: unknown) {
  if (error instanceof APIError) return error.message
  if (error instanceof Error) return error.message
  return String(error)
}

async function responseError(res: Response) {
  const text = await res.text().catch(() => '')
  try {
    const data = JSON.parse(text) as { error?: string }
    if (data.error) return data.error
  } catch {
    // Fall back to the raw body below.
  }
  return text || `HTTP ${res.status}`
}

function runtimeFor(tab: TerminalTab | null) {
  if (!tab) return null
  const runtime = runtimes.get(tab.id)
  return runtime && !runtime.disposed ? runtime : null
}

function setTabHost(id: string, el: unknown) {
  if (el instanceof HTMLElement) hosts.set(id, el)
  else hosts.delete(id)
}

function terminalSize(tab: TerminalTab) {
  const runtime = runtimeFor(tab)
  const proposed = runtime?.fitAddon.proposeDimensions()
  return {
    cols: proposed?.cols || runtime?.term.cols || 80,
    rows: proposed?.rows || runtime?.term.rows || 24
  }
}

function writeNotice(tab: TerminalTab, text: string) {
  runtimeFor(tab)?.term.write(`\r\n\x1b[90m${text}\x1b[0m\r\n`)
}

function resetStreamState(tab: TerminalTab) {
  tab.cwd = ''
  const runtime = runtimeFor(tab)
  if (!runtime) return
  runtime.controlBuffer = ''
  runtime.zmodemProbe = ''
  runtime.zmodemIntercepted = false
}

function setCurrentDir(tab: TerminalTab, value: string) {
  const path = value.replace(/\r/g, '').trim()
  if (!path || /[\r\n\0]/.test(path)) return
  tab.cwd = path
}

function parseOsc7Cwd(body: string) {
  const value = body.slice(2)
  if (!value.startsWith('file://')) return ''
  try {
    return decodeURIComponent(new URL(value).pathname)
  } catch {
    return ''
  }
}

function handleOsc(tab: TerminalTab, body: string) {
  if (body.startsWith('633;P;Cwd=')) {
    setCurrentDir(tab, body.slice('633;P;Cwd='.length))
    return ''
  }
  if (body.startsWith('7;file://')) {
    const path = parseOsc7Cwd(body)
    if (path) {
      setCurrentDir(tab, path)
      return ''
    }
  }
  if (body.startsWith('777;chemssh-transfer;')) {
    handleTransferMarker(tab, body.slice('777;chemssh-transfer;'.length))
    return ''
  }
  return `\x1b]${body}\x07`
}

function filterZmodem(tab: TerminalTab, text: string) {
  const runtime = runtimeFor(tab)
  if (!runtime || runtime.zmodemIntercepted) return text
  const probe = (runtime.zmodemProbe + text).toLowerCase()
  runtime.zmodemProbe = probe.slice(-96)
  const signature = ZMODEM_SIGNATURES.find(item => probe.includes(item))
  if (!signature) return text
  runtime.zmodemIntercepted = true
  void interceptNativeZmodem(tab, signature)
  const index = text.toLowerCase().indexOf(signature)
  return index < 0 ? '' : text.slice(0, index)
}

function consumeTerminalOutput(tab: TerminalTab, data: string) {
  const runtime = runtimeFor(tab)
  if (!runtime) return
  const text = runtime.controlBuffer + data
  runtime.controlBuffer = ''
  let visible = ''
  let index = 0
  while (index < text.length) {
    const start = text.indexOf('\x1b]', index)
    if (start < 0) {
      const tail = text.slice(index)
      if (tail.endsWith('\x1b')) {
        visible += tail.slice(0, -1)
        runtime.controlBuffer = '\x1b'
      } else {
        visible += tail
      }
      break
    }
    visible += text.slice(index, start)
    const rest = text.slice(start)
    const bel = rest.indexOf('\x07')
    const st = rest.indexOf('\x1b\\')
    let end = -1
    let terminatorLength = 1
    if (bel >= 0 && (st < 0 || bel < st)) {
      end = bel
    } else if (st >= 0) {
      end = st
      terminatorLength = 2
    }
    if (end < 0) {
      runtime.controlBuffer = rest
      break
    }
    visible += handleOsc(tab, rest.slice(2, end))
    index = start + end + terminatorLength
  }
  if (runtime.controlBuffer.length > 65536) {
    visible += runtime.controlBuffer
    runtime.controlBuffer = ''
  }
  if (!visible) return
  const filtered = filterZmodem(tab, visible)
  if (filtered) runtime.term.write(filtered)
}

function fitTerminal(tab: TerminalTab | null) {
  if (!props.active || !tab || tab.id !== activeTabID.value) return
  const runtime = runtimeFor(tab)
  const host = hosts.get(tab.id)
  if (!runtime || !host) return
  if (host.clientWidth <= 0 || host.clientHeight <= 0) return
  try {
    runtime.fitAddon.fit()
  } catch {
    // The terminal can be transiently unmeasurable while the view is switching.
  }
}

async function loadProfiles() {
  try {
    profiles.value = await api<Profile[]>('/api/profiles')
  } catch {
    profiles.value = []
  }
}

async function ensureRuntime(tab: TerminalTab) {
  const existing = runtimeFor(tab)
  if (existing) return existing
  await nextTick()
  const host = hosts.get(tab.id)
  if (!host) return null
  const term = new Terminal({
    cursorBlink: true,
    fontFamily: 'Consolas, "Cascadia Mono", "SFMono-Regular", Menlo, monospace',
    fontSize: 13,
    lineHeight: 1.2,
    scrollback: 5000,
    theme: {
      background: '#0b1218',
      foreground: '#d7e6ee',
      cursor: '#7dd3fc',
      cursorAccent: '#0b1218',
      selectionBackground: 'rgba(125, 211, 252, 0.28)',
      black: '#0f1a22',
      brightBlack: '#5a7382'
    }
  })
  const fitAddon = new FitAddon()
  term.loadAddon(fitAddon)
  term.open(host)
  const runtime: TerminalRuntime = {
    term,
    fitAddon,
    sessionID: '',
    outputAbort: null,
    pendingInput: '',
    inputFlushing: false,
    resizeTimer: null,
    lastCols: 0,
    lastRows: 0,
    controlBuffer: '',
    zmodemProbe: '',
    zmodemIntercepted: false,
    sftpSessionID: '',
    sftpProfileID: '',
    sftpReleasePending: false,
    pendingCd: '',
    disposed: false
  }
  runtimes.set(tab.id, runtime)
  term.onData(data => {
    if (tab.status !== 'connected' || !runtime.sessionID) return
    runtime.pendingInput += data
    void flushInput(tab)
  })
  term.onResize(() => scheduleResize(tab))
  writeNotice(tab, t('terminal.welcome'))
  fitTerminal(tab)
  return runtime
}

async function createTab(profileID = lastProfileID || remoteProfiles.value[0]?.id || '') {
  if (profileID) {
    lastProfileID = profileID
    persistProfile(profileID)
  }
  tabSeq += 1
  const tab = reactive<TerminalTab>({
    id: `terminal-tab-${Date.now()}-${tabSeq}`,
    profileID,
    status: 'idle',
    cwd: '',
    uploading: false,
    uploadPercent: 0
  })
  tabs.value.push(tab)
  activeTabID.value = tab.id
  tabMenu.visible = false
  await ensureRuntime(tab)
  await nextTick()
  fitTerminal(tab)
  runtimeFor(tab)?.term.focus()
  return tab
}

async function activateTab(id: string) {
  if (activeTabID.value === id) return
  activeTabID.value = id
  tabMenu.visible = false
  dropActive.value = false
  await nextTick()
  fitTerminal(activeTab.value)
  runtimeFor(activeTab.value)?.term.focus()
}

async function shutdownTab(tab: TerminalTab) {
  const runtime = runtimes.get(tab.id)
  if (!runtime) return
  await disconnect(tab, true)
  const sftpID = runtime.sftpSessionID
  runtime.sftpSessionID = ''
  runtime.sftpProfileID = ''
  if (sftpID) await postJSON('/api/sftp/disconnect', { session: sftpID }).catch(() => undefined)
  runtime.disposed = true
  if (runtime.resizeTimer !== null) window.clearTimeout(runtime.resizeTimer)
  runtime.term.dispose()
  runtimes.delete(tab.id)
  hosts.delete(tab.id)
}

async function closeTab(id: string) {
  const index = tabs.value.findIndex(tab => tab.id === id)
  if (index < 0) return
  const [tab] = tabs.value.splice(index, 1)
  if (activeTabID.value === id) {
    const next = tabs.value[Math.min(index, tabs.value.length - 1)] || null
    activeTabID.value = next?.id || ''
    if (next) {
      await nextTick()
      fitTerminal(next)
      runtimeFor(next)?.term.focus()
    }
  }
  await shutdownTab(tab)
}

async function closeAllTabs() {
  const all = [...tabs.value]
  tabs.value = []
  activeTabID.value = ''
  tabMenu.visible = false
  await Promise.all(all.map(tab => shutdownTab(tab)))
}

async function duplicateTab(tab: TerminalTab) {
  const targetDir = tab.status === 'connected' ? tab.cwd : ''
  const duplicate = await createTab(tab.profileID || lastProfileID)
  if (duplicate.profileID) await connect(duplicate, targetDir)
}

function isTabDragging(tabID: string) {
  return tabDrag.active && tabDrag.tabID === tabID
}

function isTabDropTarget(tabID: string, position: 'before' | 'after') {
  return tabDrag.active && tabDrag.targetID === tabID && tabDrag.position === position
}

function startTabDrag(tabID: string, event: DragEvent) {
  if (!event.dataTransfer) return
  event.stopPropagation()
  tabDrag.active = true
  tabDrag.tabID = tabID
  tabDrag.targetID = ''
  event.dataTransfer.effectAllowed = 'move'
  event.dataTransfer.setData('text/plain', tabID)
  dropActive.value = false
}

function handleTabDragOver(tabID: string, event: DragEvent) {
  if (!tabDrag.active || tabDrag.tabID === tabID) return
  event.preventDefault()
  event.stopPropagation()
  const rect = (event.currentTarget as HTMLElement).getBoundingClientRect()
  tabDrag.targetID = tabID
  tabDrag.position = event.clientX < rect.left + rect.width / 2 ? 'before' : 'after'
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'
}

function handleTabDragLeave(tabID: string, event: DragEvent) {
  if (!tabDrag.active || tabDrag.targetID !== tabID) return
  const current = event.currentTarget instanceof HTMLElement ? event.currentTarget : null
  const related = event.relatedTarget instanceof Node ? event.relatedTarget : null
  if (current && related && current.contains(related)) return
  tabDrag.targetID = ''
}

function handleTabDrop(tabID: string) {
  if (!tabDrag.active || tabDrag.tabID === tabID) return
  const fromIndex = tabs.value.findIndex(tab => tab.id === tabDrag.tabID)
  if (fromIndex < 0) return
  const position = tabDrag.position
  const [moved] = tabs.value.splice(fromIndex, 1)
  let targetIndex = tabs.value.findIndex(tab => tab.id === tabID)
  if (targetIndex < 0) {
    tabs.value.push(moved)
  } else {
    if (position === 'after') targetIndex += 1
    tabs.value.splice(targetIndex, 0, moved)
  }
  finishTabDrag()
}

function finishTabDrag() {
  tabDrag.active = false
  tabDrag.tabID = ''
  tabDrag.targetID = ''
}

function openTabMenu(tab: TerminalTab, event: MouseEvent) {
  const menuWidth = 190
  const menuHeight = 130
  tabMenu.tabID = tab.id
  tabMenu.x = Math.min(event.clientX, Math.max(8, window.innerWidth - menuWidth - 8))
  tabMenu.y = Math.min(event.clientY, Math.max(8, window.innerHeight - menuHeight - 8))
  tabMenu.visible = true
}

function runTabAction(action: TabAction) {
  const tab = tabs.value.find(item => item.id === tabMenu.tabID) || null
  tabMenu.visible = false
  if (action === 'duplicate') {
    if (tab) void duplicateTab(tab)
    return
  }
  if (action === 'close') {
    if (tab) void closeTab(tab.id)
    return
  }
  void closeAllTabs()
}

function handleTabMenuDismiss(event: Event) {
  if (!tabMenu.visible) return
  if (event instanceof MouseEvent && event.button === 2) return
  tabMenu.visible = false
}

function handleTabMenuKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') tabMenu.visible = false
}

async function openShell(tab: TerminalTab) {
  const size = terminalSize(tab)
  const payload = { profile_id: tab.profileID, cols: size.cols, rows: size.rows }
  try {
    return await postJSON<{ id: string }>('/api/session/shell/open', payload)
  } catch (error) {
    const hostKey = hostKeyFromError(error)
    if (!hostKey) throw error
    const trust = await confirmHostKey(hostKey)
    return await postJSON<{ id: string }>('/api/session/shell/open', { ...payload, ...trust })
  }
}

async function connectSftp(tab: TerminalTab) {
  const runtime = runtimeFor(tab)
  if (!runtime) throw new Error('terminal tab closed')
  if (runtime.sftpSessionID && runtime.sftpProfileID === tab.profileID) return runtime.sftpSessionID
  await disconnectSftp(tab)
  const payload = { id: tab.profileID }
  let session: { id: string }
  try {
    session = await postJSON<{ id: string }>('/api/sftp/connect', payload)
  } catch (error) {
    const hostKey = hostKeyFromError(error)
    if (!hostKey) throw error
    const trust = await confirmHostKey(hostKey)
    session = await postJSON<{ id: string }>('/api/sftp/connect', { ...payload, ...trust })
  }
  runtime.sftpSessionID = session.id
  runtime.sftpProfileID = tab.profileID
  runtime.sftpReleasePending = false
  return runtime.sftpSessionID
}

async function disconnectSftp(tab: TerminalTab) {
  const runtime = runtimeFor(tab)
  if (!runtime) return
  const id = runtime.sftpSessionID
  runtime.sftpSessionID = ''
  runtime.sftpProfileID = ''
  if (!id) return
  await postJSON('/api/sftp/disconnect', { session: id }).catch(() => undefined)
}

function releaseSftpIfIdle(tab: TerminalTab) {
  if (tab.uploading) {
    const runtime = runtimeFor(tab)
    if (runtime) runtime.sftpReleasePending = true
    return
  }
  void disconnectSftp(tab)
}

function uploadForm(form: FormData, onProgress?: (loaded: number) => void) {
  return new Promise<void>((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', '/api/sftp/upload')
    withLauncherToken().forEach((value, key) => xhr.setRequestHeader(key, value))
    if (onProgress) {
      xhr.upload.onprogress = event => {
        if (event.lengthComputable) onProgress(event.loaded)
      }
    }
    xhr.onload = () => {
      let message = ''
      try {
        message = (JSON.parse(xhr.responseText || '{}') as { error?: string }).error || ''
      } catch {
        // Non-JSON error bodies fall back to the status code below.
      }
      if (xhr.status >= 200 && xhr.status < 300) resolve()
      else reject(new Error(message || `HTTP ${xhr.status}`))
    }
    xhr.onerror = () => reject(new Error(t('terminal.uploadFailed')))
    xhr.send(form)
  })
}

async function resolveUploadDir(tab: TerminalTab, sessionID: string, target?: string | null) {
  const raw = (target ?? tab.cwd).trim().replace(/[\r\n\0]/g, '')
  if (raw && raw !== '.') return raw.replace(/\/+$/, '') || '/'
  const resolved = await api<{ path?: string }>(
    `/api/sftp/realpath?session=${encodeURIComponent(sessionID)}&path=.`
  ).catch(() => null)
  return resolved?.path?.trim() || '.'
}

async function resolveUploadConflicts(sessionID: string, targetDir: string, entries: UploadEntry[]) {
  let existing = new Set<string>()
  try {
    const listing = await api<{ entries: { name: string }[] }>(
      `/api/sftp/list?session=${encodeURIComponent(sessionID)}&path=${encodeURIComponent(targetDir)}`
    )
    existing = new Set(listing.entries.map(entry => entry.name))
  } catch {
    // A missing destination is created by the upload itself.
    return entries
  }
  const roots = [...new Set(entries.map(entry => entry.relativePath.split('/')[0] || entry.relativePath))]
  const conflicts = roots.filter(name => existing.has(name))
  if (!conflicts.length) return entries
  const names = conflicts.slice(0, 5).join(', ') + (conflicts.length > 5 ? ' …' : '')
  try {
    await ElMessageBox.confirm(
      t('terminal.overwriteBody', { dir: targetDir, names }),
      t('terminal.overwriteTitle'),
      {
        type: 'warning',
        confirmButtonText: t('terminal.overwrite'),
        cancelButtonText: t('terminal.skipExisting'),
        distinguishCancelAndClose: true
      }
    )
    return entries
  } catch (action) {
    if (action !== 'cancel') throw action
    const kept = entries.filter(entry => !conflicts.includes(entry.relativePath.split('/')[0] || entry.relativePath))
    if (!kept.length) ElMessage.info(t('terminal.uploadSkippedAll'))
    return kept
  }
}

async function uploadEntriesToDir(tab: TerminalTab, entries: UploadEntry[], target: string | null) {
  if (tab.uploading) {
    ElMessage.info(t('terminal.uploadBusy'))
    return
  }
  const runtime = runtimeFor(tab)
  if (!runtime) return
  tab.uploading = true
  tab.uploadPercent = 0
  runtime.sftpReleasePending = false
  try {
    const normalized = normalizeUploadEntries(entries)
    if (normalized.invalidCount > 0) {
      ElMessage.warning(t('terminal.unsafePathsSkipped', { count: normalized.invalidCount }))
    }
    if (normalized.renamedCount > 0) {
      ElMessage.info(t('terminal.pathsNormalized', { count: normalized.renamedCount }))
    }
    if (!normalized.entries.length) {
      ElMessage.warning(t('terminal.noDroppedFiles'))
      return
    }
    const sessionID = await connectSftp(tab)
    const targetDir = await resolveUploadDir(tab, sessionID, target)
    const accepted = await resolveUploadConflicts(sessionID, targetDir, normalized.entries)
    if (!accepted.length) return
    ElMessage.info(t('terminal.uploadStarted', { count: accepted.length, dir: targetDir }))
    const totalBytes = accepted.reduce((sum, entry) => sum + entry.file.size, 0) || 1
    let sentBytes = 0
    let uploadedCount = 0
    for (const entry of accepted) {
      const form = new FormData()
      form.append('session', sessionID)
      form.append('path', targetDir)
      form.append('relative_path', entry.relativePath)
      form.append('file', entry.file)
      const entryStart = sentBytes
      await uploadForm(form, loaded => {
        const done = Math.min(entryStart + loaded, totalBytes)
        tab.uploadPercent = Math.floor((done / totalBytes) * 100)
      })
      sentBytes = entryStart + entry.file.size
      tab.uploadPercent = Math.floor((sentBytes / totalBytes) * 100)
      uploadedCount += 1
    }
    ElMessage.success(t('terminal.uploadDone', { count: uploadedCount, dir: targetDir }))
  } catch (error) {
    if (error === 'cancel' || error === 'close') return
    if (!runtimeFor(tab)) return
    ElMessage.error(`${t('terminal.uploadFailed')}: ${errorMessage(error)}`)
  } finally {
    tab.uploading = false
    tab.uploadPercent = 0
    const live = runtimeFor(tab)
    if (live?.sftpReleasePending && tab.status !== 'connected') {
      live.sftpReleasePending = false
      await disconnectSftp(tab)
    }
  }
}

async function uploadDroppedFiles(tab: TerminalTab, event: DragEvent) {
  try {
    const entries = await collectDropUploadEntries(event)
    await uploadEntriesToDir(tab, entries, null)
  } catch (error) {
    if (error === 'cancel' || error === 'close') return
    if (!runtimeFor(tab)) return
    ElMessage.error(`${t('terminal.uploadFailed')}: ${errorMessage(error)}`)
  }
}

function pickTransferFiles() {
  return new Promise<File[]>(resolve => {
    const input = document.createElement('input')
    input.type = 'file'
    input.multiple = true
    input.style.position = 'fixed'
    input.style.left = '-9999px'
    input.style.top = '-9999px'
    let settled = false
    const finish = (files: File[]) => {
      if (settled) return
      settled = true
      window.removeEventListener('focus', handleFocus)
      input.remove()
      resolve(files)
    }
    const handleFocus = () => {
      window.setTimeout(() => {
        if (!settled && (!input.files || input.files.length === 0)) finish([])
      }, 350)
    }
    input.addEventListener('change', () => finish(Array.from(input.files ?? [])))
    input.addEventListener('cancel', () => finish([]))
    window.addEventListener('focus', handleFocus)
    document.body.appendChild(input)
    input.click()
  })
}

type ShellTransfer = {
  direction: 'upload' | 'download'
  cwd: string
  argv: string[]
}

function decodeMarkerField(value: string) {
  if (!value) return ''
  try {
    const padded = value + '='.repeat((4 - (value.length % 4)) % 4)
    const binary = atob(padded)
    const bytes = Uint8Array.from(binary, char => char.charCodeAt(0))
    return new TextDecoder().decode(bytes)
  } catch {
    return ''
  }
}

function parseTransferMarker(raw: string): ShellTransfer | null {
  const parts = raw.split('|')
  if (parts[0] !== 'v1' || (parts[1] !== 'upload' && parts[1] !== 'download')) return null
  return {
    direction: parts[1] === 'upload' ? 'upload' : 'download',
    cwd: decodeMarkerField(parts[2] || ''),
    argv: parts.slice(3).map(decodeMarkerField)
  }
}

function handleTransferMarker(tab: TerminalTab, raw: string) {
  const transfer = parseTransferMarker(raw)
  if (!transfer) return
  if (transfer.direction === 'upload') {
    void pickAndUploadFromShell(tab, transfer.cwd)
    return
  }
  void downloadFromShell(tab, transfer.cwd, transfer.argv)
}

async function pickAndUploadManually() {
  const tab = activeTab.value
  if (!tab || tab.status !== 'connected') {
    ElMessage.warning(t('terminal.connectFirst'))
    return
  }
  if (tab.uploading) {
    ElMessage.info(t('terminal.uploadBusy'))
    return
  }
  const files = await pickTransferFiles()
  if (!files.length) {
    ElMessage.info(t('terminal.transferCancelled'))
    return
  }
  await uploadEntriesToDir(tab, filesToUploadEntries(files), null)
}

async function pickAndUploadFromShell(tab: TerminalTab, cwd: string) {
  if (tab.uploading) {
    ElMessage.info(t('terminal.uploadBusy'))
    return
  }
  const files = await pickTransferFiles()
  if (!files.length) {
    ElMessage.info(t('terminal.transferCancelled'))
    return
  }
  await uploadEntriesToDir(tab, filesToUploadEntries(files), cwd || null)
}

function downloadPathsFromArgv(cwd: string, argv: string[]) {
  const candidates: string[] = []
  let skipNext = false
  let endOptions = false
  for (const raw of argv) {
    const arg = raw.replace(/[\r\n]+$/, '')
    if (!arg) continue
    if (endOptions) {
      candidates.push(arg)
      continue
    }
    if (skipNext) {
      skipNext = false
      continue
    }
    if (arg === '--') {
      endOptions = true
      continue
    }
    if (arg === '-e' || arg === '--escape' || arg === '-i' || arg === '--input-command') {
      skipNext = true
      continue
    }
    if (arg.startsWith('-')) continue
    candidates.push(arg)
  }
  return candidates.map(path => {
    if (path.startsWith('/')) return path
    if (!cwd || cwd === '.') return path
    return `${cwd.replace(/\/+$/, '')}/${path.replace(/^\/+/, '')}`
  })
}

async function downloadFromShell(tab: TerminalTab, cwd: string, argv: string[]) {
  const paths = downloadPathsFromArgv(cwd, argv)
  if (!paths.length) {
    ElMessage.warning(t('terminal.transferNoFiles'))
    return
  }
  try {
    const sessionID = await connectSftp(tab)
    for (const remotePath of paths) {
      const res = await fetch(
        `/api/sftp/download?session=${encodeURIComponent(sessionID)}&path=${encodeURIComponent(remotePath)}`,
        { headers: withLauncherToken() }
      )
      if (!res.ok) throw new Error(await responseError(res))
      const blob = await res.blob()
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = remotePath.split('/').filter(Boolean).pop() || 'download'
      document.body.appendChild(link)
      link.click()
      link.remove()
      window.setTimeout(() => URL.revokeObjectURL(url), 1000)
      if (paths.length > 1) await new Promise<void>(resolve => window.setTimeout(resolve, 300))
    }
    ElMessage.success(t('terminal.transferDownloadStarted', { count: paths.length }))
  } catch (error) {
    ElMessage.error(`${t('terminal.transferFailed')}: ${errorMessage(error)}`)
  }
}

async function sendRawInput(tab: TerminalTab, data: string) {
  const runtime = runtimeFor(tab)
  if (!runtime?.sessionID) return
  const res = await fetch(`/api/session/shell/input?id=${encodeURIComponent(runtime.sessionID)}`, {
    method: 'POST',
    headers: withLauncherToken({ 'Content-Type': 'text/plain; charset=utf-8' }),
    body: data
  })
  if (!res.ok) throw new Error(await responseError(res))
}

function quoteShellPath(path: string) {
  return `'${path.replace(/'/g, `'\\''`)}'`
}

async function interceptNativeZmodem(tab: TerminalTab, signature: string) {
  await sendRawInput(tab, '\x03').catch(() => undefined)
  if (signature.includes('rz waiting')) {
    ElMessage.warning(t('terminal.nativeRzIntercepted'))
    window.setTimeout(() => {
      void pickAndUploadFromShell(tab, tab.cwd)
    }, 400)
    return
  }
  ElMessage.warning(t('terminal.nativeSzIntercepted'))
}

function handleFileDragEnter(event: DragEvent) {
  if (!hasFileDrag(event)) return
  event.preventDefault()
  dropActive.value = true
}

function handleFileDragOver(event: DragEvent) {
  if (!hasFileDrag(event)) return
  event.preventDefault()
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy'
  dropActive.value = true
}

function handleFileDragLeave(event: DragEvent) {
  if (!dropActive.value) return
  const target = event.currentTarget instanceof HTMLElement ? event.currentTarget : null
  const related = event.relatedTarget instanceof Node ? event.relatedTarget : null
  if (target && related && target.contains(related)) return
  dropActive.value = false
}

async function handleFileDrop(event: DragEvent) {
  if (!hasFileDrag(event)) return
  event.preventDefault()
  dropActive.value = false
  const tab = activeTab.value
  if (!tab || tab.status !== 'connected') {
    ElMessage.warning(t('terminal.connectFirst'))
    return
  }
  if (tab.uploading) {
    ElMessage.info(t('terminal.uploadBusy'))
    return
  }
  await uploadDroppedFiles(tab, event)
}

async function connect(tab: TerminalTab, cdPath = '') {
  if (tab.status === 'connecting' || tab.status === 'connected') return
  if (!tab.profileID) {
    ElMessage.warning(t('terminal.selectProfile'))
    return
  }
  const runtime = await ensureRuntime(tab)
  if (!runtime) return
  const profile = profiles.value.find(item => item.id === tab.profileID)
  tab.status = 'connecting'
  resetStreamState(tab)
  runtime.term.reset()
  writeNotice(tab, t('terminal.connectingProfile', { name: profile?.name || profile?.ssh_host || '' }))
  await nextTick()
  fitTerminal(tab)
  try {
    const opened = await openShell(tab)
    if (!runtimeFor(tab)) {
      await postJSON('/api/session/shell/close', { id: opened.id }).catch(() => undefined)
      return
    }
    runtime.sessionID = opened.id
    tab.status = 'connected'
    runtime.lastCols = 0
    runtime.lastRows = 0
    if (cdPath.startsWith('/')) {
      tab.cwd = cdPath
      runtime.pendingCd = cdPath
    }
    void sendResize(tab, true)
    void streamOutput(tab, opened.id)
    runtime.term.focus()
  } catch (error) {
    if (!runtimeFor(tab)) return
    if (error === 'cancel' || error === 'close') {
      tab.status = 'idle'
      writeNotice(tab, t('terminal.hostKeyCancelled'))
      return
    }
    tab.status = 'error'
    writeNotice(tab, `${t('terminal.connectFailed')}: ${errorMessage(error)}`)
  }
}

async function streamOutput(tab: TerminalTab, id: string) {
  const runtime = runtimeFor(tab)
  if (!runtime) return
  runtime.outputAbort = new AbortController()
  let streamFailed = false
  let sentCd = false
  try {
    const res = await fetch(`/api/session/shell/output?id=${encodeURIComponent(id)}`, {
      method: 'GET',
      headers: withLauncherToken(),
      cache: 'no-store',
      signal: runtime.outputAbort.signal
    })
    if (!res.ok || !res.body) {
      throw new Error(await responseError(res))
    }
    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      if (!sentCd) {
        sentCd = true
        if (runtime.pendingCd) {
          const dir = runtime.pendingCd
          runtime.pendingCd = ''
          runtime.pendingInput += `cd ${quoteShellPath(dir)}\r`
          void flushInput(tab)
        }
      }
      consumeTerminalOutput(tab, decoder.decode(value, { stream: true }))
    }
  } catch (error) {
    if (!runtime.disposed && (error as Error)?.name !== 'AbortError') {
      streamFailed = true
      writeNotice(tab, `${t('terminal.connectionLost')}: ${errorMessage(error)}`)
    }
  } finally {
    if (!runtime.disposed && tab.status === 'connected' && runtime.sessionID === id) {
      tab.status = 'idle'
      runtime.sessionID = ''
      runtime.pendingInput = ''
      resetStreamState(tab)
      if (!streamFailed) writeNotice(tab, t('terminal.sessionEnded'))
      releaseSftpIfIdle(tab)
    }
  }
}

async function disconnect(tab: TerminalTab, quiet: boolean) {
  const runtime = runtimeFor(tab)
  if (!runtime) return
  const id = runtime.sessionID
  runtime.sessionID = ''
  runtime.pendingCd = ''
  tab.status = 'idle'
  runtime.pendingInput = ''
  dropActive.value = false
  resetStreamState(tab)
  if (runtime.resizeTimer !== null) {
    window.clearTimeout(runtime.resizeTimer)
    runtime.resizeTimer = null
  }
  if (runtime.outputAbort) {
    runtime.outputAbort.abort()
    runtime.outputAbort = null
  }
  if (id) {
    await postJSON('/api/session/shell/close', { id }).catch(() => undefined)
  }
  releaseSftpIfIdle(tab)
  if (!quiet) writeNotice(tab, t('terminal.disconnected'))
}

function disconnectActiveTab() {
  if (activeTab.value) void disconnect(activeTab.value, false)
}

function clearActiveTab() {
  if (activeTab.value) runtimeFor(activeTab.value)?.term.clear()
}

function activateCurrentTerminal() {
  if (activeTab.value) runtimeFor(activeTab.value)?.term.focus()
}

async function flushInput(tab: TerminalTab) {
  const runtime = runtimeFor(tab)
  if (!runtime || runtime.inputFlushing) return
  runtime.inputFlushing = true
  try {
    while (runtime.pendingInput && runtime.sessionID) {
      const chunk = runtime.pendingInput
      const id = runtime.sessionID
      runtime.pendingInput = ''
      const res = await fetch(`/api/session/shell/input?id=${encodeURIComponent(id)}`, {
        method: 'POST',
        headers: withLauncherToken({ 'Content-Type': 'text/plain; charset=utf-8' }),
        body: chunk
      })
      if (!res.ok) break
    }
  } catch {
    // The session may have been closed while typing; status handling covers it.
  } finally {
    runtime.inputFlushing = false
  }
}

async function sendResize(tab: TerminalTab, force = false) {
  const runtime = runtimeFor(tab)
  if (!runtime || tab.status !== 'connected' || !runtime.sessionID) return
  const { cols, rows } = terminalSize(tab)
  if (!force && cols === runtime.lastCols && rows === runtime.lastRows) return
  runtime.lastCols = cols
  runtime.lastRows = rows
  await postJSON('/api/session/shell/resize', { id: runtime.sessionID, cols, rows }).catch(() => undefined)
}

function scheduleResize(tab: TerminalTab) {
  const runtime = runtimeFor(tab)
  if (!runtime) return
  if (runtime.resizeTimer !== null) window.clearTimeout(runtime.resizeTimer)
  runtime.resizeTimer = window.setTimeout(() => {
    runtime.resizeTimer = null
    void sendResize(tab)
  }, 150)
}

watch(() => props.active, async active => {
  if (!active) return
  await loadProfiles()
  await nextTick()
  fitTerminal(activeTab.value)
  activateCurrentTerminal()
})

onMounted(async () => {
  const initialProfile = new URLSearchParams(window.location.search).get('profile')
  if (initialProfile) {
    lastProfileID = initialProfile
    persistProfile(initialProfile)
  }
  await loadProfiles()
  await createTab(lastProfileID || remoteProfiles.value[0]?.id || '')
  if (stageRef.value) {
    resizeObserver = new ResizeObserver(() => fitTerminal(activeTab.value))
    resizeObserver.observe(stageRef.value)
  }
  document.addEventListener('click', handleTabMenuDismiss)
  window.addEventListener('keydown', handleTabMenuKeydown)
  window.addEventListener('scroll', handleTabMenuDismiss, true)
  activateCurrentTerminal()
})

onBeforeUnmount(() => {
  resizeObserver?.disconnect()
  resizeObserver = null
  document.removeEventListener('click', handleTabMenuDismiss)
  window.removeEventListener('keydown', handleTabMenuKeydown)
  window.removeEventListener('scroll', handleTabMenuDismiss, true)
  for (const tab of [...tabs.value]) {
    void shutdownTab(tab)
  }
  tabs.value = []
})
</script>
