<template>
  <section class="terminal-view" :style="{ '--terminal-sidebar-width': `${sidebarWidth}px` }">
    <aside class="terminal-sessions">
      <section class="terminal-side-section terminal-servers-section" :class="{ 'is-collapsed': serverSectionCollapsed }">
        <header class="terminal-sessions-title">
          <button
            type="button"
            class="terminal-side-toggle"
            :title="serverSectionCollapsed ? t('terminal.serversExpand') : t('terminal.serversCollapse')"
            @click="toggleServerSection"
          >
            <el-icon><ArrowRight v-if="serverSectionCollapsed" /><ArrowDown v-else /></el-icon>
            <span class="terminal-sessions-label">{{ t('terminal.servers') }}</span>
          </button>
          <span class="terminal-sessions-actions">
            <button
              type="button"
              class="terminal-sessions-refresh"
              :class="{ 'is-refreshing': refreshingProfiles }"
              :title="t('terminal.refreshServers')"
              :aria-label="t('terminal.refreshServers')"
              :disabled="refreshingProfiles"
              @click="refreshProfiles"
            >
              <el-icon><Refresh /></el-icon>
            </button>
            <span class="terminal-sessions-count">{{ remoteProfiles.length }}</span>
          </span>
        </header>
        <div v-show="!serverSectionCollapsed" class="terminal-session-list" role="list">
          <button
            v-for="profile in remoteProfiles"
            :key="profile.id"
            class="terminal-session"
            :class="{ 'is-active': profile.id === selectedProfileID }"
            type="button"
            :title="profileSessionTitle(profile)"
            @click="selectProfile(profile)"
            @dblclick="openProfileSession(profile)"
            @contextmenu.prevent.stop="openProfileMenu(profile, $event)"
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
      </section>
      <section
        class="terminal-side-section terminal-sftp-section"
        :class="{ 'is-open': sftpPanelOpen }"
      >
        <header class="terminal-sessions-title terminal-sftp-title">
          <button
            type="button"
            class="terminal-side-toggle"
            :title="sftpPanelOpen ? t('terminal.sftpCollapse') : t('terminal.sftpExpand')"
            @click="toggleSftpSection"
          >
            <el-icon><ArrowRight v-if="!sftpPanelOpen" /><ArrowDown v-else /></el-icon>
            <span class="terminal-sessions-label">SFTP</span>
          </button>
          <span v-if="sftpPanelOpen" class="terminal-sessions-actions">
            <button
              type="button"
              class="terminal-sftp-sync"
              :class="{ 'is-on': sftpSyncOn }"
              :title="sftpSyncOn ? t('terminal.sftpSyncDisable') : t('terminal.sftpSyncEnable')"
              :aria-pressed="sftpSyncOn"
              :disabled="!sftpPanelAvailable"
              @click="toggleSftpSync"
            >
              <el-icon><Link /></el-icon>
            </button>
            <button
              type="button"
              class="terminal-sessions-refresh"
              :class="{ 'is-refreshing': sftpPanelLoading }"
              :title="t('terminal.sftpRefresh')"
              :aria-label="t('terminal.sftpRefresh')"
              :disabled="sftpPanelLoading || !sftpPanelAvailable"
              @click="refreshSftpPanel"
            >
              <el-icon><Refresh /></el-icon>
            </button>
          </span>
        </header>
        <div
          v-if="sftpPanelOpen"
          class="terminal-sftp-body"
          :class="{ 'is-drop-active': sftpDropActive }"
          @dragenter="handleSftpDragEnter"
          @dragover="handleSftpDragOver"
          @dragleave="handleSftpDragLeave"
          @drop="handleSftpDrop"
        >
          <div v-if="!sftpPanelAvailable" class="terminal-sftp-hint">{{ t('terminal.sftpNeedsTerminal') }}</div>
          <template v-else>
            <div class="terminal-sftp-pathbar">
              <button
                type="button"
                class="terminal-sftp-path-action"
                :title="t('terminal.sftpBack')"
                :disabled="!sftpCanGoBack"
                @click="goSftpBack"
              >
                <el-icon><Back /></el-icon>
              </button>
              <input
                v-model="sftpPathInput"
                class="terminal-sftp-path-input"
                type="text"
                spellcheck="false"
                autocomplete="off"
                :placeholder="t('terminal.sftpPathPlaceholder')"
                @keydown.enter.prevent.stop="goSftpPath"
              />
            </div>
            <div v-if="sftpPanelError" class="terminal-sftp-hint is-error">
              {{ t('terminal.sftpLoadFailed', { error: sftpPanelError }) }}
            </div>
            <div v-else-if="sftpPanelLoading && !sftpEntries.length && !sftpParentPath" class="terminal-sftp-hint">
              {{ t('terminal.sftpLoading') }}
            </div>
            <div v-else-if="!sftpEntries.length && !sftpParentPath" class="terminal-sftp-hint">
              {{ t('terminal.sftpEmpty') }}
            </div>
            <div v-else class="terminal-sftp-list" role="list">
              <button
                v-if="sftpParentPath"
                type="button"
                class="terminal-sftp-entry is-dir is-parent"
                :title="t('terminal.sftpUp')"
                @dblclick="openSftpParent"
              >
                <span class="terminal-sftp-entry-icon"><el-icon><FolderOpened /></el-icon></span>
                <span class="terminal-sftp-entry-name">..</span>
              </button>
              <button
                v-for="entry in sftpEntries"
                :key="entry.path || entry.name"
                type="button"
                class="terminal-sftp-entry"
                :class="{ 'is-dir': entry.is_dir }"
                draggable="true"
                :title="entry.is_dir ? t('terminal.sftpEntryDirTitle', { name: entry.path }) : t('terminal.sftpEntryFileTitle', { name: entry.path })"
                @dblclick="openSftpEntry(entry)"
                @dragstart="startSftpEntryDrag(entry, $event)"
              >
                <span class="terminal-sftp-entry-icon">
                  <el-icon><FolderOpened v-if="entry.is_dir" /><Document v-else /></el-icon>
                </span>
                <span class="terminal-sftp-entry-name">{{ entry.name }}</span>
                <span v-if="!entry.is_dir" class="terminal-sftp-entry-size">{{ formatBytes(entry.size) }}</span>
              </button>
            </div>
          </template>
          <div v-if="sftpDropActive" class="terminal-sftp-drop-hint">{{ t('terminal.sftpDropUpload') }}</div>
        </div>
      </section>
    </aside>
    <div
      class="terminal-side-resizer"
      role="separator"
      aria-orientation="vertical"
      :title="t('terminal.sidebarResize')"
      @mousedown.prevent="beginSidebarResize"
    />
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
        <div v-if="searchVisible" class="terminal-search" @mousedown.stop>
          <el-icon class="terminal-search-icon"><Search /></el-icon>
          <input
            ref="searchInputRef"
            v-model="searchQuery"
            class="terminal-search-input"
            type="text"
            spellcheck="false"
            autocomplete="off"
            :placeholder="t('terminal.searchPlaceholder')"
            @input="runSearch(false)"
            @keydown.enter.prevent.stop="runSearch($event.shiftKey)"
            @keydown.esc.prevent.stop="closeSearch()"
          />
          <span class="terminal-search-count">{{ searchStatus }}</span>
          <button type="button" class="terminal-search-action" :title="t('terminal.searchPrevious')" @mousedown.prevent @click="runSearch(true)">
            <el-icon><ArrowUp /></el-icon>
          </button>
          <button type="button" class="terminal-search-action" :title="t('terminal.searchNext')" @mousedown.prevent @click="runSearch(false)">
            <el-icon><ArrowDown /></el-icon>
          </button>
          <button type="button" class="terminal-search-action" :title="t('terminal.searchClose')" @mousedown.prevent @click="closeSearch()">
            <el-icon><Close /></el-icon>
          </button>
        </div>
      </div>
      <footer class="terminal-footer">
        <div class="terminal-footer-actions">
          <el-tooltip :content="t('terminal.clear')" placement="top" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button circle :disabled="!activeTab" @click="clearActiveTab">
              <el-icon>
                <svg
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  stroke-width="1.8"
                  stroke-linecap="round"
                  stroke-linejoin="round"
                  aria-hidden="true"
                >
                  <g transform="translate(0 -0.6) rotate(-45 12 12)">
                    <rect x="5.5" y="9.5" width="13" height="5" rx="1.4" />
                    <path d="M14.4 9.5v5" />
                  </g>
                  <path d="M4 19.1h16" stroke-width="1.6" />
                </svg>
              </el-icon>
            </el-button>
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
          <el-tooltip :content="t('terminal.search')" placement="top" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button :icon="Search" circle :disabled="!activeTab" @click="toggleSearch" />
          </el-tooltip>
          <el-popover placement="top-start" :width="260" trigger="click" popper-class="terminal-settings-popover">
            <template #reference>
              <el-button :icon="Setting" circle :title="t('terminal.displaySettings')" />
            </template>
            <div class="terminal-settings-panel">
              <div class="terminal-settings-title">{{ t('terminal.displaySettings') }}</div>
              <div class="terminal-settings-row">
                <span class="terminal-settings-label">{{ t('terminal.fontSize') }}</span>
                <div class="terminal-font-stepper">
                  <button
                    type="button"
                    :disabled="displaySettings.fontSize <= TERMINAL_FONT_MIN"
                    :title="t('terminal.fontSizeDecrease')"
                    @click="adjustFontSize(-1)"
                  >
                    <el-icon><Minus /></el-icon>
                  </button>
                  <span class="terminal-font-value">{{ t('terminal.fontSizeValue', { size: displaySettings.fontSize }) }}</span>
                  <button
                    type="button"
                    :disabled="displaySettings.fontSize >= TERMINAL_FONT_MAX"
                    :title="t('terminal.fontSizeIncrease')"
                    @click="adjustFontSize(1)"
                  >
                    <el-icon><Plus /></el-icon>
                  </button>
                </div>
              </div>
              <div class="terminal-settings-row">
                <span class="terminal-settings-label">{{ t('terminal.scrollback') }}</span>
                <el-select v-model="displaySettings.scrollback" size="small" class="terminal-scrollback-select" @change="applyScrollback">
                  <el-option
                    v-for="value in SCROLLBACK_OPTIONS"
                    :key="value"
                    :label="t('terminal.scrollbackValue', { count: value })"
                    :value="value"
                  />
                </el-select>
              </div>
              <div class="terminal-settings-hint">{{ t('terminal.searchShortcutHint') }}</div>
            </div>
          </el-popover>
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
          <el-tooltip v-else-if="canReconnect" :content="t('terminal.reconnect')" placement="top" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button class="terminal-footer-reconnect" :icon="RefreshRight" circle @click="reconnectActiveTab" />
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
      <button type="button" role="menuitem" @click="runTabAction('sftp')">
        {{ t('terminal.tabOpenSftp') }}
        <span class="terminal-tab-menu-hint">Ctrl+Alt+F</span>
      </button>
      <button type="button" role="menuitem" @click="runTabAction('close')">{{ t('terminal.tabClose') }}</button>
      <button type="button" role="menuitem" class="is-danger" @click="runTabAction('closeAll')">{{ t('terminal.tabCloseAll') }}</button>
    </div>
    <Teleport to="body">
      <div
        v-if="profileMenu.visible"
        class="sftp-context-menu profile-context-menu"
        :style="{ left: `${profileMenu.x}px`, top: `${profileMenu.y}px` }"
        role="menu"
        @contextmenu.prevent
      >
        <button type="button" role="menuitem" @click="openMenuProfileInSftp">{{ t('launcher.openSftp') }}</button>
      </div>
    </Teleport>
  </section>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  ArrowDown,
  ArrowRight,
  ArrowUp,
  Back,
  Close,
  Connection,
  Document,
  FolderOpened,
  Link,
  Minus,
  Plus,
  Refresh,
  RefreshRight,
  Search,
  Setting,
  SwitchButton,
  Upload,
  UploadFilled
} from '@element-plus/icons-vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { SearchAddon } from '@xterm/addon-search'
import '@xterm/xterm/css/xterm.css'
import { api, APIError, formatBytes, joinPath, parentPath, postJSON, withLauncherToken, type FileEntry, type Profile } from '../api'
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
const SIDEBAR_WIDTH_KEY = 'chemssh-launcher.terminal-sidebar-width'
const SERVER_COLLAPSED_KEY = 'chemssh-launcher.terminal-server-collapsed'
const SFTP_SYNC_KEY = 'chemssh-launcher.terminal-sftp-sync'
const SIDEBAR_WIDTH_DEFAULT = 224
const SIDEBAR_WIDTH_MIN = 172
const SIDEBAR_WIDTH_MAX = 560

type TerminalStatus = 'idle' | 'connecting' | 'connected' | 'error'
type TabAction = 'duplicate' | 'sftp' | 'close' | 'closeAll'

// Same limit the SFTP page applies before it opens a cached, auto-synced copy.
const REMOTE_OPEN_SYNC_MAX_BYTES = 100 * 1024 * 1024
const OPEN_SYNC_POLL_INTERVAL = 1500
const SFTP_HISTORY_LIMIT = 20
const SFTP_EXTERNAL_DOWNLOAD_MIME = 'application/octet-stream'

interface OpenSyncEvent {
  seq: number
  session: string
  remote_path: string
  local_path: string
  status: 'done' | 'error'
  error?: string
}

// Per-tab sidebar SFTP browser. It stays closed until the user opens it, so a
// new terminal never dials an extra SFTP channel on its own.
interface TerminalSftpPanel {
  open: boolean
  sync: boolean
  path: string
  loadedPath: string
  // Previous directories, most recent first, mirroring the SFTP page's
  // back-history dropdown.
  history: string[]
  entries: FileEntry[]
  loading: boolean
  error: string
  dropActive: boolean
  seq: number
}

interface TerminalTab {
  id: string
  profileID: string
  status: TerminalStatus
  cwd: string
  // Directory the shell sat in when the session dropped, so a reconnect can
  // `cd` back into it instead of landing in the remote home.
  resumePath: string
  // Only a tab that already reached the server offers reconnect; a brand new
  // tab still starts from the server list.
  wasConnected: boolean
  uploading: boolean
  uploadPercent: number
  sftp: TerminalSftpPanel
}

interface TerminalRuntime {
  term: Terminal
  fitAddon: FitAddon
  searchAddon: SearchAddon
  sessionID: string
  outputAbort: AbortController | null
  pendingInput: string
  inputFlushing: boolean
  resizeTimer: number | null
  resizeRunning: boolean
  resizePending: boolean
  lastCols: number
  lastRows: number
  controlBuffer: string
  zmodemProbe: string
  zmodemIntercepted: boolean
  sftpSessionID: string
  sftpProfileID: string
  sftpReleasePending: boolean
  sftpConnectPromise: Promise<string> | null
  // Remote paths opened for local editing; the backend keeps watching them and
  // uploading edits, so this session must outlive a collapsed sidebar panel.
  sftpSyncedFiles: Set<string>
  pendingCd: string
  disposed: boolean
}

const profiles = ref<Profile[]>([])
const refreshingProfiles = ref(false)
const tabs = ref<TerminalTab[]>([])
const activeTabID = ref('')
const selectedProfileID = ref('')
const dropActive = ref(false)
const stageRef = ref<HTMLElement | null>(null)
const searchVisible = ref(false)
const searchQuery = ref('')
const searchResultCount = ref(0)
const searchResultIndex = ref(-1)
const searchInputRef = ref<HTMLInputElement | null>(null)
const displaySettings = reactive({ fontSize: 13, scrollback: 5000 })
const tabMenu = reactive({ visible: false, x: 0, y: 0, tabID: '' })
const profileMenu = reactive({ visible: false, x: 0, y: 0, profileID: '' })
const tabDrag = reactive({ active: false, tabID: '', targetID: '', position: 'before' as 'before' | 'after' })
const sidebarWidth = ref(readSidebarWidth())
const serverSectionCollapsed = ref(readStoredFlag(SERVER_COLLAPSED_KEY))
const sftpSyncPreferred = ref(readStoredFlag(SFTP_SYNC_KEY))

const TERMINAL_FONT_MIN = 10
const TERMINAL_FONT_MAX = 24
const SCROLLBACK_OPTIONS = [1000, 5000, 10000, 20000, 50000]
const SEARCH_OPTIONS = {
  decorations: {
    matchBackground: '#1f3b4d',
    matchBorder: '#2b5a75',
    matchOverviewRuler: '#7dd3fc',
    activeMatchBackground: '#2c6e91',
    activeMatchBorder: '#7dd3fc',
    activeMatchColorOverviewRuler: '#7dd3fc'
  }
}

const runtimes = new Map<string, TerminalRuntime>()
const hosts = new Map<string, HTMLElement>()
let lastProfileID = localStorage.getItem(TERMINAL_PROFILE_KEY) || ''
let resizeObserver: ResizeObserver | null = null
let settingsSaveTimer: number | null = null
let tabSeq = 0
let viewDisposed = false
// Own cursor into the backend's edited-file upload log; each page polls the
// same global log with its own sequence, so the SFTP page is unaffected.
let lastOpenSyncSeq = 0
let openSyncPolling = false
let openSyncPollTimer: number | undefined

const ZMODEM_SIGNATURES = [
  'rz waiting to receive',
  '**b00000000000000',
  '**b0100',
  '**\x18b00000000000000',
  '**\x18b0100'
]

const remoteProfiles = computed(() => profiles.value.filter(profile => profile.kind !== 'local'))
const menuProfile = computed(() => profiles.value.find(profile => profile.id === profileMenu.profileID) || null)
const activeTab = computed(() => tabs.value.find(tab => tab.id === activeTabID.value) || null)
const activeProfile = computed(() => remoteProfiles.value.find(profile => profile.id === activeTab.value?.profileID) || null)
const connectionAddress = computed(() => activeProfile.value ? profileSessionURL(activeProfile.value) : '')
const connected = computed(() => activeTab.value?.status === 'connected')
const statusKind = computed<TerminalStatus>(() => activeTab.value?.status || 'idle')
const canReconnect = computed(() => {
  const tab = activeTab.value
  if (!tab || !tab.profileID) return false
  if (tab.status === 'connected' || tab.status === 'connecting') return false
  return tab.wasConnected || tab.status === 'error'
})
const currentDir = computed(() => activeTab.value?.cwd || '')
const uploading = computed(() => activeTab.value?.uploading || false)
const uploadPercent = computed(() => activeTab.value?.uploadPercent || 0)
const uploadTargetLabel = computed(() => currentDir.value.trim() || t('terminal.remoteHome'))
const sftpPanelOpen = computed(() => Boolean(activeTab.value?.sftp.open))
const sftpPanelAvailable = computed(() => activeTab.value?.status === 'connected')
const sftpSyncOn = computed(() => Boolean(activeTab.value?.sftp.open && activeTab.value?.sftp.sync))
const sftpPanelLoading = computed(() => Boolean(activeTab.value?.sftp.loading))
const sftpPanelError = computed(() => activeTab.value?.sftp.error || '')
const sftpEntries = computed(() => activeTab.value?.sftp.entries || [])
const sftpCanGoBack = computed(() => Boolean(activeTab.value?.sftp.history.length))
const sftpDropActive = computed(() => Boolean(activeTab.value?.sftp.dropActive))
const sftpParentPath = computed(() => {
  const tab = activeTab.value
  if (!tab?.sftp.open) return ''
  const current = tab.sftp.loadedPath || tab.sftp.path
  if (!current) return ''
  const parent = parentPath(current, 'remote')
  return parent && parent !== current ? parent : ''
})
const sftpPathInput = computed({
  get: () => activeTab.value?.sftp.path || '',
  set: value => {
    const tab = activeTab.value
    if (tab) tab.sftp.path = value
  }
})
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
const searchStatus = computed(() => {
  if (!searchVisible.value || !searchQuery.value) return ''
  if (searchResultCount.value <= 0) return t('terminal.searchNoMatch')
  const current = Math.min(Math.max(searchResultIndex.value + 1, 1), searchResultCount.value)
  return t('terminal.searchResults', { current, total: searchResultCount.value })
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

function selectProfile(profile: Profile) {
  selectedProfileID.value = profile.id
  lastProfileID = profile.id
  persistProfile(profile.id)
}

function readStoredFlag(key: string) {
  try {
    return localStorage.getItem(key) === '1'
  } catch {
    return false
  }
}

function persistFlag(key: string, enabled: boolean) {
  try {
    localStorage.setItem(key, enabled ? '1' : '0')
  } catch {
    // Sidebar preferences are best-effort.
  }
}

function readSidebarWidth() {
  try {
    const stored = Number(localStorage.getItem(SIDEBAR_WIDTH_KEY))
    if (Number.isFinite(stored) && stored >= SIDEBAR_WIDTH_MIN && stored <= SIDEBAR_WIDTH_MAX) return Math.round(stored)
  } catch {
    // Fall through to the default width.
  }
  return SIDEBAR_WIDTH_DEFAULT
}

function toggleServerSection() {
  serverSectionCollapsed.value = !serverSectionCollapsed.value
  persistFlag(SERVER_COLLAPSED_KEY, serverSectionCollapsed.value)
}

function beginSidebarResize(event: MouseEvent) {
  const startX = event.clientX
  const startWidth = sidebarWidth.value
  const previousCursor = document.body.style.cursor
  document.body.style.cursor = 'col-resize'
  document.body.style.userSelect = 'none'
  const handleMove = (move: MouseEvent) => {
    const next = Math.min(SIDEBAR_WIDTH_MAX, Math.max(SIDEBAR_WIDTH_MIN, startWidth + move.clientX - startX))
    sidebarWidth.value = Math.round(next)
  }
  const handleUp = () => {
    window.removeEventListener('mousemove', handleMove)
    window.removeEventListener('mouseup', handleUp)
    document.body.style.cursor = previousCursor
    document.body.style.userSelect = ''
    try {
      localStorage.setItem(SIDEBAR_WIDTH_KEY, String(sidebarWidth.value))
    } catch {
      // Width persistence is best-effort.
    }
    fitTerminal(activeTab.value)
  }
  window.addEventListener('mousemove', handleMove)
  window.addEventListener('mouseup', handleUp)
}

function emptySftpPanel(): TerminalSftpPanel {
  return {
    open: false,
    sync: sftpSyncPreferred.value,
    path: '',
    loadedPath: '',
    history: [],
    entries: [],
    loading: false,
    error: '',
    dropActive: false,
    seq: 0
  }
}

// Clears the browser contents when the shell session goes away; the panel keeps
// its open/sync state so reconnecting restores the same place.
function resetSftpPanelData(tab: TerminalTab) {
  tab.sftp.entries = []
  tab.sftp.error = ''
  tab.sftp.loading = false
  tab.sftp.loadedPath = ''
  tab.sftp.seq += 1
}

function sortSftpEntries(entries: FileEntry[]) {
  return [...entries].sort((left, right) => {
    if (left.is_dir !== right.is_dir) return left.is_dir ? -1 : 1
    return left.name.localeCompare(right.name)
  })
}

// Relative input (".", "..", "user/data") is resolved through the server so
// the panel always shows an absolute path that the shell can `cd` into.
async function resolveSftpPanelPath(sessionID: string, target: string) {
  if (target.startsWith('/')) return target
  const resolved = await api<{ path?: string }>(
    `/api/sftp/realpath?session=${encodeURIComponent(sessionID)}&path=${encodeURIComponent(target)}`
  ).catch(() => null)
  const value = resolved?.path?.trim() || ''
  return value.startsWith('/') ? value : target
}

// `recordHistory: false` is used when the new path is itself a history entry,
// matching the SFTP page: going back does not push the directory you left.
async function loadSftpPanel(
  tab: TerminalTab,
  path?: string,
  options: { recordHistory?: boolean } = {}
) {
  const state = tab.sftp
  if (!state.open) return ''
  if (tab.status !== 'connected') {
    resetSftpPanelData(tab)
    return ''
  }
  const requested = (path ?? state.path) || tab.cwd || '.'
  state.path = requested
  const seq = state.seq + 1
  state.seq = seq
  state.loading = true
  state.error = ''
  try {
    const sessionID = await connectSftp(tab)
    const target = await resolveSftpPanelPath(sessionID, requested)
    if (state.seq !== seq || !state.open || !runtimeFor(tab)) return ''
    state.path = target
    const data = await api<{ path: string; entries: FileEntry[] }>(
      `/api/sftp/list?session=${encodeURIComponent(sessionID)}&path=${encodeURIComponent(target)}`
    )
    if (state.seq !== seq || !state.open || !runtimeFor(tab)) return ''
    const previous = state.loadedPath
    state.path = data.path || target
    state.loadedPath = state.path
    state.entries = sortSftpEntries(data.entries || [])
    if (options.recordHistory !== false && previous && previous !== state.loadedPath) {
      state.history = [previous, ...state.history.filter(item => item !== previous)].slice(0, SFTP_HISTORY_LIMIT)
    }
    return state.loadedPath
  } catch (error) {
    if (state.seq !== seq || !state.open || !runtimeFor(tab)) return ''
    state.entries = []
    state.error = errorMessage(error)
    return ''
  } finally {
    if (state.seq === seq) state.loading = false
  }
}

function toggleSftpSection() {
  const tab = activeTab.value
  if (!tab) return
  const open = !tab.sftp.open
  tab.sftp.open = open
  if (!open) {
    resetSftpPanelData(tab)
    tab.sftp.path = ''
    tab.sftp.history = []
    tab.sftp.dropActive = false
    // Closing the browser frees the extra SFTP channel; uploads reconnect on
    // demand, and an in-flight upload delays the release until it finishes.
    releaseSftpIfIdle(tab)
    return
  }
  tab.sftp.path = tab.cwd || tab.sftp.path || '.'
  void loadSftpPanel(tab, tab.sftp.path)
}

function toggleSftpSync() {
  const tab = activeTab.value
  if (!tab || !tab.sftp.open) return
  tab.sftp.sync = !tab.sftp.sync
  sftpSyncPreferred.value = tab.sftp.sync
  persistFlag(SFTP_SYNC_KEY, tab.sftp.sync)
  // Enabling sync treats the shell as the source of truth.
  if (tab.sftp.sync && tab.cwd && tab.cwd !== tab.sftp.loadedPath) void loadSftpPanel(tab, tab.cwd)
}

function refreshSftpPanel() {
  const tab = activeTab.value
  if (!tab || !tab.sftp.open) return
  void loadSftpPanel(tab, tab.sftp.loadedPath || tab.sftp.path || tab.cwd || '.')
}

function goSftpPath() {
  const tab = activeTab.value
  if (!tab || !tab.sftp.open) return
  void navigateSftpPanel(tab, tab.sftp.path.trim() || tab.cwd || '.')
}

function goSftpBack() {
  const tab = activeTab.value
  if (!tab || !tab.sftp.open) return
  const path = tab.sftp.history[0]
  if (!path) return
  tab.sftp.history = tab.sftp.history.slice(1)
  void navigateSftpPanel(tab, path, { recordHistory: false })
}

function openSftpParent() {
  const tab = activeTab.value
  if (!tab || !tab.sftp.open) return
  const current = tab.sftp.loadedPath || tab.sftp.path
  const parent = parentPath(current || '/', 'remote')
  if (!parent || parent === current) return
  void navigateSftpPanel(tab, parent)
}

function openSftpEntry(entry: FileEntry) {
  const tab = activeTab.value
  if (!tab || !tab.sftp.open) return
  const path = entry.path || joinPath(tab.sftp.loadedPath || '.', entry.name, 'remote')
  if (entry.is_dir) {
    void navigateSftpPanel(tab, path)
    return
  }
  void openSftpFile(tab, entry, path)
}

// Mirrors the SFTP page's Open action: cache the file locally, hand it to the
// OS file association, and let the backend watch it and upload every change
// back to the remote path while the session stays connected.
async function openSftpFile(tab: TerminalTab, entry: FileEntry, path: string) {
  if (entry.size > REMOTE_OPEN_SYNC_MAX_BYTES) {
    ElMessage.warning(t('sftp.openTooLarge'))
    return
  }
  try {
    const sessionID = await connectSftp(tab)
    await postJSON<{ path: string }>('/api/sftp/open', { session: sessionID, path })
    runtimeFor(tab)?.sftpSyncedFiles.add(path)
  } catch (error) {
    ElMessage.error(t('terminal.sftpOpenFailed', { error: errorMessage(error) }))
  }
}

// Mirrors the SFTP page's row drag: Chromium turns a DownloadURL payload into a
// real OS file drag, so the row can be dropped on Explorer or the desktop.
function startSftpEntryDrag(entry: FileEntry, event: DragEvent) {
  const tab = activeTab.value
  if (!tab || !event.dataTransfer) return
  const path = entry.path || joinPath(tab.sftp.loadedPath || '.', entry.name, 'remote')
  event.dataTransfer.effectAllowed = 'copy'
  event.dataTransfer.setData('text/plain', path)
  const sessionID = runtimeFor(tab)?.sftpSessionID || ''
  if (!sessionID || entry.is_dir) return
  const url = `${window.location.origin}/api/sftp/download?session=${encodeURIComponent(sessionID)}&path=${encodeURIComponent(path)}`
  event.dataTransfer.setData(
    'DownloadURL',
    `${SFTP_EXTERNAL_DOWNLOAD_MIME}:${sanitizeSftpDownloadName(entry.name)}:${url}`
  )
}

// DownloadURL is colon-delimited, so ':' and path separators would corrupt it.
function sanitizeSftpDownloadName(name: string) {
  return name.replace(/[:\\/]/g, '_').replace(/[\u0000-\u001f]/g, '').trim() || 'download'
}

function handleSftpDragEnter(event: DragEvent) {
  if (!hasFileDrag(event)) return
  event.preventDefault()
  const tab = activeTab.value
  if (tab?.sftp.open) tab.sftp.dropActive = true
}

function handleSftpDragOver(event: DragEvent) {
  if (!hasFileDrag(event)) return
  event.preventDefault()
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy'
  const tab = activeTab.value
  if (tab?.sftp.open) tab.sftp.dropActive = true
}

function handleSftpDragLeave(event: DragEvent) {
  const tab = activeTab.value
  if (!tab?.sftp.dropActive) return
  const target = event.currentTarget instanceof HTMLElement ? event.currentTarget : null
  const related = event.relatedTarget instanceof Node ? event.relatedTarget : null
  if (target && related && target.contains(related)) return
  tab.sftp.dropActive = false
}

// Files dropped on the browser upload into the directory it is showing.
async function handleSftpDrop(event: DragEvent) {
  if (!hasFileDrag(event)) return
  event.preventDefault()
  const tab = activeTab.value
  if (!tab?.sftp.open) return
  tab.sftp.dropActive = false
  if (tab.status !== 'connected') {
    ElMessage.warning(t('terminal.sftpNeedsTerminal'))
    return
  }
  const dir = tab.sftp.loadedPath || tab.sftp.path || null
  try {
    const entries = await collectDropUploadEntries(event)
    await uploadEntriesToDir(tab, entries, dir)
    if (dir) void loadSftpPanel(tab, dir)
  } catch (error) {
    if (error === 'cancel' || error === 'close') return
    ElMessage.error(`${t('terminal.uploadFailed')}: ${errorMessage(error)}`)
  }
}

// The backend reports each edited-file upload here; refresh the directory that
// holds the file so sizes and timestamps stay truthful.
async function pollOpenSyncEvents() {
  if (openSyncPolling || viewDisposed) return
  // Without an SFTP channel there is nothing this page could have opened.
  const hasChannel = [...runtimes.values()].some(
    runtime => !runtime.disposed && (runtime.sftpSessionID !== '' || runtime.sftpSyncedFiles.size > 0)
  )
  if (!hasChannel) return
  openSyncPolling = true
  try {
    const result = await api<{ events: OpenSyncEvent[] }>(
      `/api/sftp/open-sync-events?after=${lastOpenSyncSeq}`
    )
    const events = Array.isArray(result.events) ? result.events : []
    for (const event of events) {
      lastOpenSyncSeq = Math.max(lastOpenSyncSeq, event.seq || 0)
      if (!event.session) continue
      const tab = tabs.value.find(item => {
        if (!item.sftp.open) return false
        return runtimeFor(item)?.sftpSessionID === event.session
      })
      if (!tab) continue
      if (event.status !== 'done') {
        ElMessage.error(t('sftp.openSyncFailed', { path: event.remote_path, error: event.error || '' }))
        continue
      }
      const parent = parentPath(event.remote_path, 'remote')
      const current = tab.sftp.loadedPath
      if (!current || current === parent) void loadSftpPanel(tab, current || tab.sftp.path)
    }
  } catch {
    // Polling stays quiet while the backend restarts or the session is gone.
  } finally {
    openSyncPolling = false
  }
}

// A user navigation in the sidebar browser drives the shell when syncing.
async function navigateSftpPanel(tab: TerminalTab, path: string, options: { recordHistory?: boolean } = {}) {
  const resolved = await loadSftpPanel(tab, path, options)
  if (!resolved || !tab.sftp.open || !tab.sftp.sync) return
  applySftpPathToTerminal(tab, resolved)
}

function applySftpPathToTerminal(tab: TerminalTab, path: string) {
  if (!path || tab.status !== 'connected') return
  if (tab.cwd === path) return
  const runtime = runtimeFor(tab)
  if (!runtime?.sessionID) return
  tab.cwd = path
  if (runtime.pendingCd) {
    runtime.pendingCd = path
    return
  }
  runtime.pendingInput = `cd ${quoteShellPath(path)}\r${runtime.pendingInput}`
  void flushInput(tab)
}

// A "connect this server" request means the user wants a session, not just the
// page: focus a live session, resume a tab that is still open, and only create a
// tab when the profile has none.
async function connectProfile(profileID: string) {
  if (!profileID || !remoteProfiles.value.some(profile => profile.id === profileID)) return
  const live = tabs.value.find(tab => tab.profileID === profileID && tab.status === 'connected')
  if (live) {
    await activateTab(live.id)
    return
  }
  const reusable = tabs.value.find(tab => tab.profileID === profileID && tab.status !== 'connecting')
  if (reusable) {
    await activateTab(reusable.id)
    await connect(reusable)
    return
  }
  const tab = await createTab(profileID)
  await connect(tab)
}

// The launcher reuses an already-open terminal page, whose mount hook never runs
// again, so it asks for the session over a message instead.
function handleHostMessage(event: MessageEvent) {
  if (window.parent === window || event.source !== window.parent) return
  const data = event.data || {}
  if (data.type !== 'chemssh-launcher:terminal-connect') return
  void connectProfile(String(data.profile || ''))
}

async function openProfileSession(profile: Profile) {
  selectProfile(profile)
  // A double-click always opens a fresh session, even when the same server
  // already has a connected tab: jumping to that tab looked like the click was
  // ignored. Only a blank, never-connected tab is filled in so the initial
  // placeholder does not linger next to a new session.
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
  if (!runtime) return { cols: 80, rows: 24 }
  // The rendered grid is the only trustworthy source: FitAddon reports a
  // placeholder size for a host that is not laid out (hidden tab), which
  // would resize the remote PTY to a few columns while the grid stays wide.
  return { cols: runtime.term.cols, rows: runtime.term.rows }
}

function writeNotice(tab: TerminalTab, text: string, freshLine = true) {
  runtimeFor(tab)?.term.write(`${freshLine ? '\r\n' : ''}\x1b[90m${text}\x1b[0m\r\n`)
}

// Modes a dropped remote program can leave behind: attributes, scroll region,
// origin mode, autowrap, hidden cursor, application cursor keys, mouse
// reporting, bracketed paste and insert mode. The new shell turns back on
// whatever it needs.
const RESUME_MODE_RESET =
  '\x1b[0m\x1b[r\x1b[?6l\x1b[?7h\x1b[?25h\x1b[?1l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[4l'

// xterm restores the DECSC cursor on `CSI ? 1049 l` unconditionally, and that
// saved position is the top row once the scrollback grew or nothing was saved
// yet. Sending it outside the alternate screen parked the cursor over the old
// output, so the new prompt overwrote it: only leave the alternate screen when
// the terminal is actually in it.
//
// Whatever the dead session left the cursor on, the resume path then parks it
// on a fresh line after the last line of history, so nothing can overwrite the
// log that a reconnect is supposed to preserve. `resume` queues the first
// notice, which must follow the parking step.
function prepareResumeTerminal(term: Terminal, resume: () => void) {
  const park = () => {
    term.write(RESUME_MODE_RESET)
    parkResumeCursor(term)
    resume()
  }
  // The buffer API still describes the alternate screen until the queued
  // sequence is parsed, so the parking step has to wait for it.
  if (term.buffer.active.type === 'alternate') term.write('\x1b[?1049l', park)
  else park()
}

function parkResumeCursor(term: Terminal) {
  if (term.rows < 1) return
  const buffer = term.buffer.active
  // Viewport scroll position has to match the buffer end first: `CSI H` is
  // relative to the viewport, not to the scrollback.
  term.scrollToBottom()
  let row = term.rows - 1
  while (row > 0) {
    const line = buffer.getLine(buffer.baseY + row)
    if (line && line.translateToString(true).trim() !== '') break
    row -= 1
  }
  // Column 1 of the last row that carries text, then a newline: it lands on an
  // empty row, or scrolls the buffer when that row is already the bottom one.
  term.write(`\x1b[${row + 1};1H\r\n`)
}

function resetStreamState(tab: TerminalTab) {
  // Remember where the shell was: a reconnect (and a failed connect retry)
  // continues in that directory instead of the remote home.
  if (tab.cwd) tab.resumePath = tab.cwd
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
  const changed = tab.cwd !== path
  tab.cwd = path
  if (!changed) return
  // Sync only ever runs terminal -> sidebar here; the sidebar sends its own
  // `cd` through applySftpPathToTerminal, so the two cannot ping-pong: each
  // side ignores a path it is already showing.
  if (!tab.sftp.open || !tab.sftp.sync) return
  if ((tab.sftp.loadedPath || tab.sftp.path) === path) return
  void loadSftpPanel(tab, path)
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

// `top`, `clear` and other screen-owning tools erase the display before they
// draw: `CSI 2 J` wipes the visible rows and `CSI 3 J` wipes the scrollback.
// xterm would honour both, dropping text this tab is supposed to keep, so the
// visible rows are moved into the scrollback first and a scrollback wipe is
// ignored. `CSI J` alone is left alone: readline emits it while editing a line.
const ERASE_DISPLAY = /\x1b\[(?:2J|3J)/g

function preserveHistory(tab: TerminalTab, text: string) {
  if (!text.includes('\x1b[')) return text
  const runtime = runtimeFor(tab)
  if (!runtime) return text
  let out = ''
  let index = 0
  let touched = false
  for (const match of text.matchAll(ERASE_DISPLAY)) {
    touched = true
    const at = match.index ?? 0
    out += text.slice(index, at)
    index = at + match[0].length
    // No remote program may throw away this tab's scrollback.
    if (match[0].endsWith('3J')) continue
    out += screenIntoScrollback(runtime.term)
    out += match[0]
  }
  return touched ? out + text.slice(index) : text
}

// Scrolls the visible screen into the scrollback and homes the cursor, which is
// where a program expects to be after erasing the display; the erase that the
// caller appends then only clears blank rows.
function screenIntoScrollback(term: Terminal) {
  const buffer = term.buffer.active
  if (buffer.type !== 'normal' || term.rows < 1) return ''
  term.scrollToBottom()
  let used = 0
  for (let row = term.rows - 1; row >= 0; row -= 1) {
    const line = buffer.getLine(buffer.baseY + row)
    if (line && line.translateToString(true).trim() !== '') {
      used = row + 1
      break
    }
  }
  if (used === 0) return ''
  return `\x1b[${term.rows};1H${'\r\n'.repeat(used)}\x1b[H`
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
  if (filtered) runtime.term.write(preserveHistory(tab, filtered))
}

function fitTerminal(tab: TerminalTab | null) {
  if (!props.active || !tab || tab.id !== activeTabID.value) return
  const runtime = runtimeFor(tab)
  const host = hosts.get(tab.id)
  if (!runtime || !host) return
  if (host.clientWidth <= 0 || host.clientHeight <= 0) return
  try {
    runtime.fitAddon.fit()
    // fit() only emits onResize when the grid changed, so a PTY that drifted
    // out of sync would otherwise stay wrong until the next window resize.
    scheduleResize(tab)
  } catch {
    // The terminal can be transiently unmeasurable while the view is switching.
  }
}

async function loadDisplaySettings() {
  try {
    const settings = await api<{ terminal_font_size?: number; terminal_scrollback?: number }>('/api/ui-settings')
    if (typeof settings.terminal_font_size === 'number') displaySettings.fontSize = settings.terminal_font_size
    if (typeof settings.terminal_scrollback === 'number') displaySettings.scrollback = settings.terminal_scrollback
  } catch {
    // Local defaults keep the terminal usable if the settings endpoint is unavailable.
  }
}

function persistDisplaySettings() {
  if (settingsSaveTimer !== null) window.clearTimeout(settingsSaveTimer)
  settingsSaveTimer = window.setTimeout(() => {
    settingsSaveTimer = null
    postJSON('/api/ui-settings', {
      terminal_font_size: displaySettings.fontSize,
      terminal_scrollback: displaySettings.scrollback
    }).catch(() => ElMessage.error(t('terminal.settingsSaveFailed')))
  }, 300)
}

function applyDisplaySettings() {
  for (const runtime of runtimes.values()) {
    if (runtime.disposed) continue
    runtime.term.options.fontSize = displaySettings.fontSize
    runtime.term.options.scrollback = displaySettings.scrollback
  }
  fitTerminal(activeTab.value)
  if (searchVisible.value) runSearch(false)
}

function adjustFontSize(delta: number) {
  const next = Math.min(TERMINAL_FONT_MAX, Math.max(TERMINAL_FONT_MIN, displaySettings.fontSize + delta))
  if (next === displaySettings.fontSize) return
  displaySettings.fontSize = next
  applyDisplaySettings()
  persistDisplaySettings()
}

function applyScrollback() {
  applyDisplaySettings()
  persistDisplaySettings()
}

function openSearch() {
  if (!activeTab.value) return
  searchVisible.value = true
  void nextTick(() => {
    searchInputRef.value?.focus()
    searchInputRef.value?.select()
    runSearch(false)
  })
}

function closeSearch(focusTerminal = true) {
  runtimeFor(activeTab.value)?.searchAddon.clearDecorations()
  searchVisible.value = false
  searchQuery.value = ''
  searchResultCount.value = 0
  searchResultIndex.value = -1
  if (focusTerminal) runtimeFor(activeTab.value)?.term.focus()
}

function toggleSearch() {
  if (searchVisible.value) closeSearch()
  else openSearch()
}

function runSearch(backwards: boolean) {
  const runtime = runtimeFor(activeTab.value)
  if (!runtime) return
  const query = searchQuery.value
  if (!query) {
    runtime.searchAddon.clearDecorations()
    searchResultCount.value = 0
    searchResultIndex.value = -1
    return
  }
  if (backwards) runtime.searchAddon.findPrevious(query, SEARCH_OPTIONS)
  else runtime.searchAddon.findNext(query, SEARCH_OPTIONS)
}

function handleTerminalShortcut(event: KeyboardEvent) {
  if (!props.active) return
  if (document.querySelector('.lumen-command-palette')) return
  // AltGr arrives as Ctrl+Alt on Windows, so it must not fire the SFTP shortcut.
  if ((event.ctrlKey || event.metaKey) && event.altKey && !event.shiftKey && !event.getModifierState?.('AltGraph') && event.key.toLowerCase() === 'f') {
    if (!activeTab.value) return
    event.preventDefault()
    event.stopPropagation()
    openTabInSftp(activeTab.value, true)
    return
  }
  if ((event.ctrlKey || event.metaKey) && !event.shiftKey && !event.altKey && event.key.toLowerCase() === 'f') {
    if (!activeTab.value) return
    event.preventDefault()
    event.stopPropagation()
    openSearch()
    return
  }
  if (event.key === 'Escape' && searchVisible.value) {
    event.preventDefault()
    event.stopPropagation()
    closeSearch()
  }
}

async function loadProfiles() {
  try {
    profiles.value = await api<Profile[]>('/api/profiles')
  } catch {
    profiles.value = []
  }
}

// Manual refresh for the server sidebar: profiles can be added, renamed or
// deleted on the launcher page while this view stays mounted.
async function refreshProfiles() {
  if (refreshingProfiles.value) return
  refreshingProfiles.value = true
  try {
    await loadProfiles()
    if (selectedProfileID.value && !profiles.value.some(profile => profile.id === selectedProfileID.value)) {
      selectedProfileID.value = ''
    }
  } finally {
    window.setTimeout(() => {
      refreshingProfiles.value = false
    }, 320)
  }
}

async function ensureRuntime(tab: TerminalTab) {
  const existing = runtimeFor(tab)
  if (existing) return existing
  await nextTick()
  const host = hosts.get(tab.id)
  if (!host) return null
  const term = new Terminal({
    allowProposedApi: true,
    cursorBlink: true,
    fontFamily: 'Consolas, "Cascadia Mono", "SFMono-Regular", Menlo, monospace',
    fontSize: displaySettings.fontSize,
    lineHeight: 1.2,
    scrollback: displaySettings.scrollback,
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
  const searchAddon = new SearchAddon()
  term.loadAddon(searchAddon)
  term.open(host)
  const runtime: TerminalRuntime = {
    term,
    fitAddon,
    searchAddon,
    sessionID: '',
    outputAbort: null,
    pendingInput: '',
    inputFlushing: false,
    resizeTimer: null,
    resizeRunning: false,
    resizePending: false,
    lastCols: 0,
    lastRows: 0,
    controlBuffer: '',
    zmodemProbe: '',
    zmodemIntercepted: false,
    sftpSessionID: '',
    sftpProfileID: '',
    sftpReleasePending: false,
    sftpConnectPromise: null,
    sftpSyncedFiles: new Set<string>(),
    pendingCd: '',
    disposed: false
  }
  runtimes.set(tab.id, runtime)
  term.onData(data => {
    if (tab.status !== 'connected' || !runtime.sessionID) return
    runtime.pendingInput += data
    // A queued cd has to reach the shell before anything typed while the
    // session was still starting, otherwise the keys prepend the command.
    if (runtime.pendingCd) return
    void flushInput(tab)
  })
  term.onResize(() => scheduleResize(tab))
  searchAddon.onDidChangeResults(result => {
    if (tab.id !== activeTabID.value) return
    searchResultCount.value = result.resultCount
    searchResultIndex.value = result.resultIndex
  })
  writeNotice(tab, t('terminal.welcome'))
  fitTerminal(tab)
  return runtime
}

async function createTab(profileID = lastProfileID || remoteProfiles.value[0]?.id || '') {
  if (profileID) {
    lastProfileID = profileID
    persistProfile(profileID)
    selectedProfileID.value = profileID
  }
  tabSeq += 1
  const tab = reactive<TerminalTab>({
    id: `terminal-tab-${Date.now()}-${tabSeq}`,
    profileID,
    status: 'idle',
    cwd: '',
    resumePath: '',
    wasConnected: false,
    uploading: false,
    uploadPercent: 0,
    sftp: emptySftpPanel()
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
  const tab = tabs.value.find(item => item.id === id)
  if (tab?.profileID) selectedProfileID.value = tab.profileID
  if (activeTabID.value === id) return
  if (searchVisible.value) closeSearch(false)
  activeTabID.value = id
  tabMenu.visible = false
  dropActive.value = false
  await nextTick()
  fitTerminal(activeTab.value)
  if (tab?.sftp.open && !tab.sftp.entries.length && tab.status === 'connected' && !tab.sftp.loading) {
    void loadSftpPanel(tab, tab.sftp.path || tab.cwd || '.')
  }
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
  if (activeTabID.value === id && searchVisible.value) closeSearch(false)
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
  if (searchVisible.value) closeSearch(false)
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
  const menuHeight = 168
  closeProfileMenu()
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
  if (action === 'sftp') {
    if (tab) openTabInSftp(tab)
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

function openProfileMenu(profile: Profile, event: MouseEvent) {
  const menuWidth = 190
  const menuHeight = 56
  selectProfile(profile)
  tabMenu.visible = false
  profileMenu.profileID = profile.id
  profileMenu.x = Math.min(event.clientX, Math.max(8, window.innerWidth - menuWidth - 8))
  profileMenu.y = Math.min(event.clientY, Math.max(8, window.innerHeight - menuHeight - 8))
  profileMenu.visible = true
}

function closeProfileMenu() {
  if (!profileMenu.visible) return
  profileMenu.visible = false
  profileMenu.profileID = ''
}

function handleProfileMenuDismiss(event: Event) {
  if (!profileMenu.visible) return
  if (event instanceof MouseEvent && event.button === 2) return
  const target = event.target
  if (target instanceof HTMLElement && target.closest('.profile-context-menu')) return
  closeProfileMenu()
}

function handleProfileMenuKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') closeProfileMenu()
}

function openProfileInSftp(profile: Profile, path = '', forceNew = false) {
  if (!profile.id || profile.kind === 'local') return
  const name = (profile.name || profile.ssh_host || '').trim()
  const targetPath = path.trim()
  if (window.parent && window.parent !== window) {
    window.parent.postMessage({ type: 'chemssh-launcher:open-sftp', profile: profile.id, name, path: targetPath, forceNew }, window.location.origin)
    return
  }
  const params = new URLSearchParams({ profile: profile.id })
  if (name) params.set('name', name)
  if (targetPath) params.set('path', targetPath)
  window.open(`/sftp?${params.toString()}`, '_blank')
}

// The server sidebar knows nothing about a terminal, so it opens the remote
// home directory. A terminal tab hands its tracked working directory over so
// the SFTP tab lands in exactly the directory the shell is sitting in.
// `forceNew` keeps the Ctrl+Alt+F shortcut additive: it appends a tab even
// when one already shows that directory, instead of focusing the existing one.
function openTabInSftp(tab: TerminalTab, forceNew = false) {
  const profile = profiles.value.find(item => item.id === tab.profileID)
  if (!profile) return
  openProfileInSftp(profile, tab.status === 'connected' ? tab.cwd : '', forceNew)
}

function openMenuProfileInSftp() {
  const profile = menuProfile.value
  closeProfileMenu()
  if (profile) openProfileInSftp(profile)
}

type ShellOpenResponse = { id: string; proxy_url?: string; proxy_error?: string }

async function openShell(tab: TerminalTab) {
  const size = terminalSize(tab)
  const payload = { profile_id: tab.profileID, cols: size.cols, rows: size.rows }
  let opened: ShellOpenResponse
  try {
    opened = await postJSON<ShellOpenResponse>('/api/session/shell/open', payload)
  } catch (error) {
    const hostKey = hostKeyFromError(error)
    if (!hostKey) throw error
    const trust = await confirmHostKey(hostKey)
    opened = await postJSON<ShellOpenResponse>('/api/session/shell/open', { ...payload, ...trust })
  }
  return { id: opened.id, proxyError: opened.proxy_error || '', ...size }
}

async function connectSftp(tab: TerminalTab) {
  const runtime = runtimeFor(tab)
  if (!runtime) throw new Error('terminal tab closed')
  if (runtime.sftpSessionID && runtime.sftpProfileID === tab.profileID) return runtime.sftpSessionID
  // The sidebar browser and the transfer helpers share one channel; coalescing
  // concurrent dials keeps a burst of clicks from opening a second session.
  if (runtime.sftpConnectPromise) return runtime.sftpConnectPromise
  const pending = (async () => {
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
    if (runtime.disposed) {
      // The tab closed while the channel was dialing; never orphan the session.
      await postJSON('/api/sftp/disconnect', { session: session.id }).catch(() => undefined)
      throw new Error('terminal tab closed')
    }
    runtime.sftpSessionID = session.id
    runtime.sftpProfileID = tab.profileID
    runtime.sftpReleasePending = false
    return runtime.sftpSessionID
  })()
  runtime.sftpConnectPromise = pending
  try {
    return await pending
  } finally {
    if (runtime.sftpConnectPromise === pending) runtime.sftpConnectPromise = null
  }
}

async function disconnectSftp(tab: TerminalTab) {
  const runtime = runtimeFor(tab)
  if (!runtime) return
  const id = runtime.sftpSessionID
  runtime.sftpSessionID = ''
  runtime.sftpProfileID = ''
  runtime.sftpSyncedFiles.clear()
  if (!id) return
  await postJSON('/api/sftp/disconnect', { session: id }).catch(() => undefined)
}

function releaseSftpIfIdle(tab: TerminalTab) {
  const runtime = runtimeFor(tab)
  // Locally opened files are watched through this channel; dropping it would
  // silently stop uploading their edits, so it lives until the tab closes.
  if (runtime && runtime.sftpSyncedFiles.size > 0) return
  if (tab.uploading) {
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
    // A release requested while uploading is honored here: either the shell
    // disconnected or the sidebar browser was collapsed mid-transfer.
    if (live?.sftpReleasePending && (tab.status !== 'connected' || !tab.sftp.open)) {
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

async function connect(tab: TerminalTab, cdPath = '', options: { resume?: boolean } = {}) {
  if (tab.status === 'connecting' || tab.status === 'connected') return
  if (!tab.profileID) {
    ElMessage.warning(t('terminal.selectProfile'))
    return
  }
  const runtime = await ensureRuntime(tab)
  if (!runtime) return
  const profile = profiles.value.find(item => item.id === tab.profileID)
  const label = profile?.name || profile?.ssh_host || ''
  const resume = options.resume === true
  tab.status = 'connecting'
  resetStreamState(tab)
  resetSftpPanelData(tab)
  if (resume) {
    // Reconnect keeps the scrollback: seeing what the session printed before
    // it dropped is the whole point.
    prepareResumeTerminal(runtime.term, () => {
      writeNotice(tab, t('terminal.reconnectingProfile', { name: label }), false)
    })
  } else {
    runtime.term.reset()
    writeNotice(tab, t('terminal.connectingProfile', { name: label }))
  }
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
    tab.wasConnected = true
    if (opened.proxyError) writeNotice(tab, t('terminal.proxyDegraded', { error: opened.proxyError }))
    runtime.lastCols = opened.cols
    runtime.lastRows = opened.rows
    if (cdPath.startsWith('/')) {
      tab.cwd = cdPath
      runtime.pendingCd = cdPath
    }
    void sendResize(tab)
    void streamOutput(tab, opened.id)
    if (tab.sftp.open) void loadSftpPanel(tab, tab.cwd || tab.sftp.path || '.')
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
          runtime.pendingInput = `cd ${quoteShellPath(dir)}\r${runtime.pendingInput}`
          void flushInput(tab)
        }
      }
      consumeTerminalOutput(tab, decoder.decode(value, { stream: true }))
    }
  } catch (error) {
    if (!runtime.disposed && (error as Error)?.name !== 'AbortError') {
      streamFailed = true
      writeNotice(tab, `${t('terminal.connectionLost')}: ${errorMessage(error)}`)
      writeNotice(tab, t('terminal.reconnectHint'))
    }
  } finally {
    if (!runtime.disposed && tab.status === 'connected' && runtime.sessionID === id) {
      tab.status = 'idle'
      runtime.sessionID = ''
      runtime.pendingInput = ''
      resetStreamState(tab)
      resetSftpPanelData(tab)
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
  resetSftpPanelData(tab)
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

// Reuses the same tab, terminal buffer and scrollback: the dropped session is
// replaced by a fresh SSH shell that continues in the last known directory.
async function reconnect(tab: TerminalTab) {
  if (tab.status === 'connecting' || tab.status === 'connected') return
  const dir = (tab.resumePath || tab.cwd).trim()
  await connect(tab, dir.startsWith('/') ? dir : '', { resume: true })
}

function reconnectActiveTab() {
  if (activeTab.value) void reconnect(activeTab.value)
}

function clearActiveTab() {
  const tab = activeTab.value
  if (!tab) return
  const runtime = runtimeFor(tab)
  runtime?.searchAddon.clearDecorations()
  runtime?.term.clear()
  searchResultCount.value = 0
  searchResultIndex.value = -1
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

async function sendResize(tab: TerminalTab) {
  const runtime = runtimeFor(tab)
  if (!runtime || tab.status !== 'connected' || !runtime.sessionID) return
  if (runtime.resizeRunning) {
    runtime.resizePending = true
    return
  }
  runtime.resizeRunning = true
  try {
    for (;;) {
      runtime.resizePending = false
      const id = runtime.sessionID
      const { cols, rows } = terminalSize(tab)
      if (cols === runtime.lastCols && rows === runtime.lastRows) return
      // Coalesce into one in-flight request so an older response can never
      // land after a newer size and leave the PTY stale.
      const ok = await postJSON('/api/session/shell/resize', { id, cols, rows })
        .then(() => true)
        .catch(() => false)
      if (!ok || runtime.sessionID !== id) return
      runtime.lastCols = cols
      runtime.lastRows = rows
      if (!runtime.resizePending) return
    }
  } finally {
    runtime.resizeRunning = false
    if (runtime.resizePending) void sendResize(tab)
  }
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
  const params = new URLSearchParams(window.location.search)
  const initialProfile = params.get('profile') || ''
  if (initialProfile) {
    lastProfileID = initialProfile
    persistProfile(initialProfile)
  }
  await loadDisplaySettings()
  await loadProfiles()
  await createTab(lastProfileID || remoteProfiles.value[0]?.id || '')
  if (stageRef.value) {
    resizeObserver = new ResizeObserver(() => fitTerminal(activeTab.value))
    resizeObserver.observe(stageRef.value)
  }
  document.addEventListener('click', handleTabMenuDismiss)
  window.addEventListener('keydown', handleTabMenuKeydown)
  window.addEventListener('scroll', handleTabMenuDismiss, true)
  document.addEventListener('click', handleProfileMenuDismiss)
  window.addEventListener('keydown', handleProfileMenuKeydown)
  window.addEventListener('scroll', handleProfileMenuDismiss, true)
  window.addEventListener('keydown', handleTerminalShortcut, true)
  window.addEventListener('message', handleHostMessage)
  openSyncPollTimer = window.setInterval(() => {
    void pollOpenSyncEvents()
  }, OPEN_SYNC_POLL_INTERVAL)
  activateCurrentTerminal()
  // The launcher's terminal button opens this page for one server, so connect
  // right away instead of landing on a tab the user still has to start.
  if (initialProfile) await connectProfile(initialProfile)
})

onBeforeUnmount(() => {
  viewDisposed = true
  if (openSyncPollTimer !== undefined) {
    window.clearInterval(openSyncPollTimer)
    openSyncPollTimer = undefined
  }
  resizeObserver?.disconnect()
  resizeObserver = null
  if (settingsSaveTimer !== null) window.clearTimeout(settingsSaveTimer)
  settingsSaveTimer = null
  document.removeEventListener('click', handleTabMenuDismiss)
  window.removeEventListener('keydown', handleTabMenuKeydown)
  window.removeEventListener('scroll', handleTabMenuDismiss, true)
  document.removeEventListener('click', handleProfileMenuDismiss)
  window.removeEventListener('keydown', handleProfileMenuKeydown)
  window.removeEventListener('scroll', handleProfileMenuDismiss, true)
  window.removeEventListener('keydown', handleTerminalShortcut, true)
  window.removeEventListener('message', handleHostMessage)
  for (const tab of [...tabs.value]) {
    void shutdownTab(tab)
  }
  tabs.value = []
})
</script>
