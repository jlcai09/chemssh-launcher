<template>
  <section class="launcher-view">
    <aside class="profile-sidebar">
      <div class="sidebar-search">
        <el-icon><Search /></el-icon>
        <input v-model="searchQuery" type="text" :placeholder="t('launcher.searchServers')" />
        <button v-if="searchQuery" class="sidebar-search-clear" type="button" :aria-label="t('launcher.searchServers')" @click="searchQuery = ''">
          <el-icon><Close /></el-icon>
        </button>
      </div>

      <div class="sidebar-label">
        <span>{{ t('launcher.servers') }}</span>
        <div class="sidebar-label-actions">
          <button type="button" :title="t('launcher.refresh')" :aria-label="t('launcher.refresh')" @click="loadProfiles"><el-icon><Refresh /></el-icon></button>
          <button type="button" :title="t('launcher.newProfile')" :aria-label="t('launcher.newProfile')" @click="newProfile"><el-icon><Plus /></el-icon></button>
        </div>
      </div>

      <div class="profile-list">
        <button
          v-if="draftOpen"
          class="profile-card is-draft"
          :class="{ 'is-active': draftActive }"
          type="button"
          @click="selectDraft"
        >
          <span class="profile-glyph is-draft">+</span>
          <span class="profile-meta">
            <span class="profile-card-heading">
              <strong>{{ draftName }}</strong>
              <em class="draft-tag">{{ t('launcher.draft') }}</em>
            </span>
            <small>{{ draftSummary }}</small>
          </span>
          <span class="state-dot" />
        </button>
        <button
          v-for="profile in filteredProfiles"
          :key="profile.id"
          class="profile-card"
          :class="{
            'is-active': current.id === profile.id,
            'is-dragging': draggingProfileID === profile.id,
            'is-drop-before': profileDropTarget.id === profile.id && profileDropTarget.position === 'before',
            'is-drop-after': profileDropTarget.id === profile.id && profileDropTarget.position === 'after'
          }"
          type="button"
          draggable="true"
          @click="selectProfile(profile)"
          @contextmenu.prevent.stop="openProfileMenu(profile, $event)"
          @dragstart="startProfileDrag(profile, $event)"
          @dragover="handleProfileDragOver(profile, $event)"
          @dragleave="handleProfileDragLeave(profile, $event)"
          @drop.prevent.stop="dropProfile(profile)"
          @dragend="finishProfileDrag"
        >
          <span class="profile-glyph" :class="`tone-${profileTone(profile)}`">{{ profileInitials(profile) }}</span>
          <span class="profile-meta">
            <strong>{{ profile.name || t('launcher.unnamed') }}</strong>
            <small>{{ profileSummary(profile) }}</small>
          </span>
          <span class="state-dot" :class="{ running: runningProfileIds.has(profile.id) }" />
        </button>
        <p v-if="!profiles.length" class="empty">{{ t('launcher.emptyProfiles') }}</p>
        <p v-else-if="!filteredProfiles.length && !draftOpen" class="empty">{{ t('launcher.noMatchingProfiles') }}</p>
      </div>

      <button class="sidebar-new" type="button" @click="newProfile">
        <el-icon><Plus /></el-icon>
        <span>{{ t('launcher.newProfile') }}…</span>
      </button>
      <div class="sidebar-foot">
        <el-icon><Lock /></el-icon>
        <span>{{ t('launcher.encryptedCredentials') }}</span>
        <span class="sidebar-count">{{ profiles.length }}</span>
      </div>
    </aside>

    <section class="launcher-main">
      <div class="launcher-toolbar">
        <div class="tool-title">
          <h2>
            {{ current.name || t('launcher.unsaved') }}
            <span v-if="draftActive" class="title-badge">{{ t('launcher.draft') }}</span>
          </h2>
          <p>{{ toolbarSubtitle }}</p>
        </div>
        <div class="toolbar-actions">
          <el-button :icon="Connection" :disabled="!current.id" :title="t('launcher.sshTest')" @click.stop="testProfile">
            <span>{{ t('launcher.sshTest') }}</span>
          </el-button>
          <el-button :icon="Monitor" :disabled="!current.id || isLocalProfile" :title="t('launcher.sshTerminal')" @click.stop="openTerminal">
            <span>{{ t('launcher.terminalShort') }}</span>
          </el-button>
          <el-button :icon="CaretRight" type="primary" :disabled="!current.id" :title="t('launcher.start')" @click.stop="startSession">
            <span>{{ t('launcher.start') }}</span>
          </el-button>
          <el-dropdown trigger="click" placement="bottom-end" @command="handleToolbarCommand">
            <el-button :icon="MoreFilled" :title="t('launcher.moreActions')" />
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="stop-forwarding" :icon="SwitchButton" :disabled="!current.id">{{ t('launcher.stopForwarding') }}</el-dropdown-item>
                <el-dropdown-item command="stop-service" :icon="CloseBold" :disabled="!current.id" divided>{{ t('launcher.stopService') }}</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </div>
      </div>

      <div class="launcher-grid">
        <div class="content-col main-col">
          <section class="lumen-card config-card">
            <div class="card-h">
              <h2><el-icon><Setting /></el-icon>{{ t('launcher.profileSettings') }}</h2>
              <span class="aside" :class="{ 'is-dirty': isDirty }">{{ isDirty ? t('launcher.unsavedChanges') : t('launcher.savedState') }}</span>
            </div>
            <el-form id="profile-editor-form" class="profile-editor" label-position="top" :model="current" @submit.prevent="saveProfile">
          <section class="form-section">
            <h3>{{ t('launcher.server') }}</h3>
            <div class="form-grid">
              <el-form-item :label="t('launcher.profileKind')">
                <el-select :model-value="current.kind || 'remote'" @update:model-value="setProfileKind">
                  <el-option :label="t('launcher.kindRemote')" value="remote" />
                  <el-option :label="t('launcher.kindLocal')" value="local" />
                </el-select>
              </el-form-item>
              <el-form-item :label="t('launcher.name')"><el-input v-model="current.name" :placeholder="t('launcher.placeholder.name')" /></el-form-item>
              <el-form-item v-if="!isLocalProfile" :label="t('launcher.sshHost')"><el-input v-model="current.ssh_host" :placeholder="t('launcher.placeholder.sshHost')" /></el-form-item>
              <el-form-item v-if="!isLocalProfile" :label="t('launcher.sshPort')"><el-input-number v-model="current.ssh_port" :min="1" :max="65535" /></el-form-item>
              <el-form-item v-if="!isLocalProfile" :label="t('launcher.sshUser')"><el-input v-model="current.ssh_user" :placeholder="t('launcher.placeholder.sshUser')" /></el-form-item>
              <el-form-item v-if="!isLocalProfile" :label="t('launcher.authMethod')">
                <el-select v-model="current.auth_method">
                  <el-option :label="t('launcher.authPassword')" value="password" />
                  <el-option :label="t('launcher.authPrivateKey')" value="private_key" />
                </el-select>
              </el-form-item>
              <el-form-item v-if="!isLocalProfile && current.auth_method === 'private_key'" :label="t('launcher.authPrivateKey')">
                <el-input v-model="current.private_key_path" :placeholder="t('launcher.placeholder.privateKeyPath')" />
              </el-form-item>
            </div>
          </section>

          <section v-if="!isLocalProfile" class="form-section">
            <h3>{{ t('launcher.credentials') }}</h3>
            <div class="form-grid">
              <el-form-item v-if="current.auth_method === 'password'" :label="t('launcher.authPassword')">
                <div class="secret-edit">
                  <el-input v-model="passwordInput" :placeholder="current.has_password ? t('launcher.savedKeepEmpty') : t('launcher.passwordNew')" show-password />
                  <el-select v-model="passwordAction">
                    <el-option :label="t('launcher.secretKeep')" value="keep" />
                    <el-option :label="t('launcher.secretReplace')" value="replace" />
                    <el-option :label="t('launcher.secretClear')" value="clear" />
                  </el-select>
                </div>
              </el-form-item>
              <el-form-item v-if="current.auth_method === 'private_key'" :label="t('launcher.passphrase')">
                <div class="secret-edit">
                  <el-input v-model="passphraseInput" :placeholder="current.has_private_key_passphrase ? t('launcher.savedKeepEmpty') : t('launcher.passphraseNew')" show-password />
                  <el-select v-model="passphraseAction">
                    <el-option :label="t('launcher.secretKeep')" value="keep" />
                    <el-option :label="t('launcher.secretReplace')" value="replace" />
                    <el-option :label="t('launcher.secretClear')" value="clear" />
                  </el-select>
                </div>
              </el-form-item>
            </div>
          </section>

          <section class="form-section">
            <h3>{{ isLocalProfile ? t('launcher.localAccess') : t('launcher.tunnel') }}</h3>
            <div class="form-grid">
              <el-form-item v-if="!isLocalProfile" :label="t('launcher.remoteHost')"><el-input v-model="current.remote_host" :placeholder="t('launcher.placeholder.remoteHost')" /></el-form-item>
              <el-form-item v-if="!isLocalProfile" :label="t('launcher.remotePort')"><el-input-number v-model="current.remote_port" :min="1" :max="65535" /></el-form-item>
              <el-form-item :label="isLocalProfile ? t('launcher.localTargetHost') : t('launcher.localHost')"><el-input v-model="current.local_host" :placeholder="t('launcher.placeholder.localHost')" /></el-form-item>
              <el-form-item :label="isLocalProfile ? t('launcher.localTargetPort') : t('launcher.localPort')"><el-input-number v-model="current.local_port" :min="1" :max="65535" /></el-form-item>
            </div>
            <el-checkbox v-model="current.open_browser">{{ t('launcher.openBrowser') }}</el-checkbox>
          </section>

          <section class="form-section">
            <h3>{{ isLocalProfile ? t('launcher.localCommands') : t('launcher.remoteCommands') }}</h3>
            <el-form-item :label="t('launcher.preStartCommands')">
              <el-input v-model="current.pre_start_commands" type="textarea" :rows="5" :placeholder="isLocalProfile ? t('launcher.placeholder.localPreStartCommands') : t('launcher.placeholder.preStartCommands')" />
            </el-form-item>
            <el-form-item :label="t('launcher.startCommand')">
              <el-input v-model="current.start_command" type="textarea" :rows="5" :placeholder="isLocalProfile ? t('launcher.placeholder.localStartCommand') : t('launcher.placeholder.startCommand')" />
            </el-form-item>
          </section>

          <details class="form-section form-section-collapsible">
            <summary class="form-section-summary">
              <span class="form-section-title">{{ t('launcher.otherSettings') }}</span>
              <span class="form-section-hint">{{ t('launcher.otherSettingsHint') }}</span>
              <el-icon class="form-section-chevron"><ArrowRight /></el-icon>
            </summary>
            <div class="form-grid form-grid-other">
              <el-form-item :label="t('launcher.localURLPath')"><el-input v-model="current.local_url_path" :placeholder="t('launcher.placeholder.localURLPath')" /></el-form-item>
              <el-form-item :label="t('launcher.healthURL')"><el-input v-model="current.health_check_url" :placeholder="t('launcher.placeholder.healthURL')" /></el-form-item>
              <el-form-item class="span-2" :label="t('launcher.securityToken')">
                <div class="secret-edit">
                  <el-input v-model="securityTokenInput" :placeholder="current.has_security_token ? t('launcher.savedKeepEmpty') : t('launcher.securityTokenNew')" show-password />
                  <el-select v-model="securityTokenAction">
                    <el-option :label="t('launcher.secretKeep')" value="keep" />
                    <el-option :label="t('launcher.secretReplace')" value="replace" />
                    <el-option :label="t('launcher.secretClear')" value="clear" />
                  </el-select>
                </div>
              </el-form-item>
            </div>
          </details>
        </el-form>
            <div class="editor-actions">
              <span v-if="isDirty" class="dirty-label">{{ t('launcher.unsavedChanges') }}</span>
              <el-button :icon="Delete" type="danger" :disabled="!current.id" @click="deleteProfile">{{ t('launcher.actions.delete') }}</el-button>
              <el-button :icon="Check" type="primary" native-type="submit" form="profile-editor-form">{{ t('launcher.actions.save') }}</el-button>
            </div>
          </section>
        </div>

        <div class="content-col side-col">
          <section class="lumen-card session-card">
            <div class="card-h">
              <h2><el-icon><Monitor /></el-icon>{{ t('launcher.sessionOverview') }}</h2>
            </div>
            <div class="session-top">
              <div class="session-badge" :class="{ 'is-live': isRunning }">
                <el-icon><Check v-if="isRunning" /><CaretRight v-else /></el-icon>
              </div>
              <div class="session-copy">
                <div class="session-name">
                  {{ isRunning ? t('launcher.sessionRunning') : t('launcher.sessionIdle') }}
                </div>
                <div class="session-sub">
                  <span>{{ kindLabel }}</span><i />
                  <span>{{ authLabel }}</span><i />
                  <span>{{ current.local_url_path || '/' }}</span>
                </div>
              </div>
              <span class="session-pill" :class="{ 'is-live': isRunning }"><i />{{ isRunning ? t('launcher.active') : t('launcher.idle') }}</span>
            </div>

            <div class="routebar">
              <div class="route-endpoint">
                <span class="route-icon"><el-icon><Monitor /></el-icon></span>
                <span><b>{{ localAddress }}</b><small>{{ t('launcher.localBind') }}</small></span>
              </div>
              <span class="route-wire"><i /></span>
              <div class="route-endpoint">
                <span class="route-icon is-green"><el-icon><Connection /></el-icon></span>
                <span><b>{{ isLocalProfile ? t('launcher.localService') : t('launcher.sshTunnel') }}</b><small>{{ t('launcher.encryptedForward') }}</small></span>
              </div>
              <span class="route-wire" />
              <div class="route-endpoint">
                <span class="route-icon"><el-icon><Cpu /></el-icon></span>
                <span><b>{{ remoteAddress }}</b><small>{{ t('launcher.chemsshService') }}</small></span>
              </div>
            </div>

            <div class="session-metrics">
              <div><small>{{ t('launcher.authLabel') }}</small><b :title="authLabel">{{ authLabel }}</b></div>
              <div><small>{{ t('launcher.localAddress') }}</small><b :title="localAddress">{{ localAddress }}</b></div>
              <div><small>{{ t('launcher.remoteAddress') }}</small><b :title="remoteAddress">{{ remoteAddress }}</b></div>
              <div><small>{{ t('launcher.healthCheck') }}</small><b class="is-ok">{{ t('launcher.healthAuto') }}</b></div>
            </div>
          </section>

          <section class="lumen-card logs-card">
            <div class="card-h">
              <h2><el-icon><Clock /></el-icon>{{ t('launcher.logs') }}</h2>
              <span class="aside">
                <b v-if="current.id" class="is-live">{{ t('launcher.logsLive') }}</b>
                <span v-else>{{ t('launcher.logsIdle') }}</span>
                <button type="button" class="card-icon-btn" :title="t('launcher.refreshLogs')" @click="refreshLogs(true)">
                  <el-icon><Refresh /></el-icon>
                </button>
              </span>
            </div>
            <pre v-if="logs.length" ref="logRef" @scroll="logScroller.onScroll">{{ logs.join('\n') }}</pre>
            <p v-else class="logs-empty">{{ t('launcher.logsEmpty') }}</p>
          </section>

          <section class="lumen-card quick-card">
            <div class="card-h"><h2>{{ t('launcher.quickActions') }}</h2></div>
            <div class="quick-list">
              <button type="button" :disabled="!current.id" @click="testProfile">
                <span class="quick-icon tone-blue"><el-icon><Connection /></el-icon></span>
                <span>{{ t('launcher.sshTest') }}</span>
                <el-icon class="quick-chevron"><ArrowRight /></el-icon>
              </button>
              <button type="button" :disabled="!current.id || isLocalProfile" @click="openTerminal">
                <span class="quick-icon tone-blue"><el-icon><Monitor /></el-icon></span>
                <span>{{ t('launcher.terminalShort') }}</span>
                <el-icon class="quick-chevron"><ArrowRight /></el-icon>
              </button>
              <button type="button" :disabled="!current.id || isLocalProfile" @click="openCurrentInSftp">
                <span class="quick-icon tone-blue"><el-icon><FolderOpened /></el-icon></span>
                <span>{{ t('launcher.browseSftp') }}</span>
                <el-icon class="quick-chevron"><ArrowRight /></el-icon>
              </button>
              <button type="button" :disabled="!current.id" @click="stopForwarding">
                <span class="quick-icon tone-orange"><el-icon><SwitchButton /></el-icon></span>
                <span>{{ t('launcher.stopForwarding') }}</span>
                <el-icon class="quick-chevron"><ArrowRight /></el-icon>
              </button>
              <button type="button" class="is-danger" :disabled="!current.id" @click="stopService">
                <span class="quick-icon tone-red"><el-icon><CloseBold /></el-icon></span>
                <span>{{ t('launcher.stopService') }}</span>
                <el-icon class="quick-chevron"><ArrowRight /></el-icon>
              </button>
            </div>
          </section>
        </div>
      </div>
    </section>

    <Teleport to="body">
      <div
        v-if="profileMenu.visible"
        class="sftp-context-menu profile-context-menu"
        :style="{ left: `${profileMenu.x}px`, top: `${profileMenu.y}px` }"
        role="menu"
      >
        <button type="button" role="menuitem" @click="duplicateProfileFromMenu">{{ t('launcher.duplicateProfile') }}</button>
        <button type="button" role="menuitem" class="is-danger" @click="deleteProfileFromMenu">{{ t('launcher.actions.delete') }}</button>
        <span class="sftp-context-separator" />
        <button type="button" role="menuitem" :disabled="menuProfileIsLocal" @click="openMenuProfileInSftp">{{ t('launcher.openSftp') }}</button>
      </div>
    </Teleport>
  </section>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { ArrowRight, CaretRight, Check, Clock, Close, CloseBold, Connection, Cpu, Delete, FolderOpened, Lock, Monitor, MoreFilled, Plus, Refresh, Search, Setting, SwitchButton } from '@element-plus/icons-vue'
import { api, postJSON, putJSON, type Profile } from '../api'
import { createAutoScroll } from '../autoScroll'
import { confirmHostKey, hostKeyFromError } from '../hostKey'
import { t } from '../i18n'

const defaults = ref<Profile | null>(null)
const profiles = ref<Profile[]>([])
const searchQuery = ref('')
const current = reactive<Profile>({} as Profile)
const draftOpen = ref(false)
const draftSnapshot = ref<Partial<Profile>>({})
const draftSecrets = ref({ password: '', passphrase: '', securityToken: '' })
const activeProfileId = ref('')
const runningProfileIds = ref(new Set<string>())
const draggingProfileID = ref('')
const profileDropTarget = reactive<{ id: string; position: 'before' | 'after' }>({ id: '', position: 'before' })
const profileMenu = reactive<{ visible: boolean; x: number; y: number; profileID: string }>({
  visible: false,
  x: 0,
  y: 0,
  profileID: ''
})
let suppressProfileClickUntil = 0
const logs = ref<string[]>([])
const logRef = ref<HTMLElement | null>(null)
const logScroller = createAutoScroll(() => logRef.value)
const passwordInput = ref('')
const passphraseInput = ref('')
const securityTokenInput = ref('')
const passwordAction = ref<'keep' | 'replace' | 'clear'>('keep')
const passphraseAction = ref<'keep' | 'replace' | 'clear'>('keep')
const securityTokenAction = ref<'keep' | 'replace' | 'clear'>('keep')
const isLocalProfile = computed(() => current.kind === 'local')
const menuProfile = computed(() => profiles.value.find(profile => profile.id === profileMenu.profileID))
const menuProfileIsLocal = computed(() => menuProfile.value?.kind === 'local')
const draftActive = computed(() => draftOpen.value && !current.id)
const draftName = computed(() => (draftActive.value ? current.name : draftSnapshot.value.name) || t('launcher.unsaved'))
const draftSummary = computed(() => {
  const source = draftActive.value ? current : draftSnapshot.value
  if (source.kind === 'local') {
    if (!source.local_host && !source.local_port) return t('launcher.newProfileHint')
    return profileSummary(source)
  }
  if (!source.ssh_host) return t('launcher.newProfileHint')
  return profileSummary(source)
})

const filteredProfiles = computed(() => {
  const query = searchQuery.value.trim().toLowerCase()
  if (!query) return profiles.value
  return profiles.value.filter(profile =>
    [profile.name, profile.ssh_host, profile.ssh_user, profile.kind]
      .some(value => String(value || '').toLowerCase().includes(query))
  )
})
const isRunning = computed(() => Boolean(current.id && runningProfileIds.value.has(current.id)))
const kindLabel = computed(() => (isLocalProfile.value ? t('launcher.kindLocal') : t('launcher.kindRemote')))
const authLabel = computed(() => {
  if (isLocalProfile.value) return current.has_security_token ? t('launcher.securityToken') : t('launcher.noAuth')
  return current.auth_method === 'private_key' ? t('launcher.authPrivateKey') : t('launcher.authPassword')
})
const localAddress = computed(() => `${current.local_host || '127.0.0.1'}:${current.local_port || 8888}`)
const remoteAddress = computed(() => {
  if (!isLocalProfile.value) return `${current.remote_host || '127.0.0.1'}:${current.remote_port || 8888}`
  const path = current.local_url_path || '/'
  return current.health_check_url || `http://${localAddress.value}${path.startsWith('/') ? path : `/${path}`}`
})
const hostSummary = computed(() => {
  if (isLocalProfile.value) return localAddress.value
  return `${current.ssh_user || '?'}@${current.ssh_host || '?'}:${current.ssh_port || 22}`
})
const toolbarSubtitle = computed(() => {
  if (!current.id && !draftActive.value) return t('launcher.noProfileHint')
  if (isLocalProfile.value) return `${kindLabel.value} · ${localAddress.value} · ${current.local_url_path || '/'}`
  return `${hostSummary.value} · ${authLabel.value} · ${current.local_url_path || '/'}`
})
const profileTones = ['blue', 'orange', 'violet', 'green'] as const

function profileTone(profile: Partial<Profile>) {
  let hash = 0
  for (const char of String(profile.id || profile.name || profile.ssh_host || '')) {
    hash = (hash * 31 + char.charCodeAt(0)) >>> 0
  }
  return profileTones[hash % profileTones.length]
}

function profileInitials(profile: Partial<Profile>) {
  if (profile.kind === 'local') return 'L'
  const text = String(profile.name || profile.ssh_host || '').trim()
  const words = text.match(/[A-Za-z0-9]+/g)
  if (words?.length) return words.map(word => word[0]).join('').slice(0, 2).toUpperCase()
  return text.slice(0, 1) || '?'
}

function handleToolbarCommand(command: string | number | object) {
  const action = String(command)
  if (action === 'stop-forwarding') void stopForwarding()
  else if (action === 'stop-service') void stopService()
}

const isDirty = computed(() => {
  if (!defaults.value) return false
  const source: Partial<Profile> = current.id
    ? profiles.value.find(profile => profile.id === current.id) || {}
    : {}
  const target = current as Partial<Profile>
  const keys: (keyof Profile)[] = [
    'name', 'kind', 'ssh_host', 'ssh_port', 'ssh_user', 'auth_method',
    'private_key_path', 'remote_host', 'remote_port', 'local_host', 'local_port',
    'local_url_path', 'health_check_url', 'open_browser',
    'pre_start_commands', 'start_command'
  ]
  for (const key of keys) {
    const left = source[key] ?? defaults.value[key] ?? ''
    const right = target[key] ?? defaults.value[key] ?? ''
    if (left !== right) return true
  }
  if (passwordInput.value) return true
  if (passphraseInput.value) return true
  if (securityTokenInput.value) return true
  return false
})

function applyProfile(profile: Partial<Profile>) {
  Object.assign(current, defaults.value, profile)
  current.kind = current.kind || 'remote'
  passwordInput.value = ''
  passphraseInput.value = ''
  securityTokenInput.value = ''
  passwordAction.value = 'keep'
  passphraseAction.value = 'keep'
  securityTokenAction.value = 'keep'
}

function setProfileKind(kind: string | number | boolean) {
  const nextKind = kind === 'local' ? 'local' : 'remote'
  if (current.kind === nextKind) return
  current.kind = nextKind
  if (nextKind === 'local') {
    current.ssh_host = ''
    current.ssh_port = 0
    current.ssh_user = ''
    current.auth_method = ''
    current.has_password = false
    current.private_key_path = ''
    current.has_private_key_passphrase = false
    current.remote_host = ''
    current.remote_port = 0
    current.pre_start_commands = ''
    current.start_command = ''
    if (!current.name) current.name = t('launcher.kindLocal')
  } else if (defaults.value) {
    current.ssh_port = current.ssh_port || defaults.value.ssh_port
    current.auth_method = current.auth_method || defaults.value.auth_method
    current.remote_host = current.remote_host || defaults.value.remote_host
    current.remote_port = current.remote_port || defaults.value.remote_port
    current.start_command = current.start_command || defaults.value.start_command
  }
}

function profileSummary(profile: Partial<Profile>) {
  if (profile.kind === 'local') return `${t('launcher.kindLocal')} · ${profile.local_host || '127.0.0.1'}:${profile.local_port || 8888}`
  return `${profile.ssh_user || '?'}@${profile.ssh_host || '?'}`
}

function snapshotDraft() {
  if (!draftOpen.value || current.id) return
  draftSnapshot.value = { ...current }
  draftSecrets.value = {
    password: passwordInput.value,
    passphrase: passphraseInput.value,
    securityToken: securityTokenInput.value
  }
}

function restoreDraft() {
  applyProfile(draftSnapshot.value)
  passwordInput.value = draftSecrets.value.password
  passphraseInput.value = draftSecrets.value.passphrase
  securityTokenInput.value = draftSecrets.value.securityToken
  passwordAction.value = passwordInput.value ? 'replace' : 'keep'
  passphraseAction.value = passphraseInput.value ? 'replace' : 'keep'
  securityTokenAction.value = securityTokenInput.value ? 'replace' : 'keep'
  logs.value = []
}

function selectDraft() {
  if (!draftOpen.value || draftActive.value) return
  restoreDraft()
}

function newProfile() {
  if (draftOpen.value) {
    selectDraft()
    return
  }
  draftOpen.value = true
  draftSnapshot.value = {}
  draftSecrets.value = { password: '', passphrase: '', securityToken: '' }
  applyProfile({})
  logs.value = []
}

function selectProfile(profile: Profile) {
  if (Date.now() < suppressProfileClickUntil) return
  if (profile.id === current.id) return
  snapshotDraft()
  applyProfile(profile)
  // Log stream will auto-reconnect via watch on current.id
}

function startProfileDrag(profile: Profile, event: DragEvent) {
  draggingProfileID.value = profile.id
  profileDropTarget.id = ''
  profileDropTarget.position = 'before'
  suppressProfileClickUntil = Date.now() + 300
  if (event.dataTransfer) {
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('text/plain', profile.id)
  }
}

function handleProfileDragOver(profile: Profile, event: DragEvent) {
  if (!draggingProfileID.value || draggingProfileID.value === profile.id) return
  event.preventDefault()
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'
  const rect = (event.currentTarget as HTMLElement).getBoundingClientRect()
  profileDropTarget.id = profile.id
  profileDropTarget.position = event.clientY < rect.top + rect.height / 2 ? 'before' : 'after'
}

function handleProfileDragLeave(profile: Profile, event: DragEvent) {
  const currentTarget = event.currentTarget as HTMLElement
  if (event.relatedTarget instanceof Node && currentTarget.contains(event.relatedTarget)) return
  if (profileDropTarget.id === profile.id) profileDropTarget.id = ''
}

function finishProfileDrag() {
  draggingProfileID.value = ''
  profileDropTarget.id = ''
}

async function dropProfile(target: Profile) {
  const sourceID = draggingProfileID.value
  const targetID = profileDropTarget.id === target.id ? target.id : ''
  const position = profileDropTarget.position
  finishProfileDrag()
  if (!sourceID || !targetID || sourceID === targetID) return

  const ids = profiles.value.map(profile => profile.id)
  const from = ids.indexOf(sourceID)
  if (from < 0) return
  ids.splice(from, 1)
  let to = ids.indexOf(targetID)
  if (to < 0) return
  if (position === 'after') to += 1
  ids.splice(to, 0, sourceID)

  const previous = profiles.value.map(profile => profile.id)
  if (ids.every((id, index) => id === previous[index])) return

  const byID = new Map(profiles.value.map(profile => [profile.id, profile]))
  profiles.value = ids
    .map(id => byID.get(id))
    .filter((profile): profile is Profile => Boolean(profile))
  try {
    await postJSON('/api/profiles/reorder', { ids })
  } catch (error) {
    await loadProfiles()
    const message = (error as Error).message
    ElMessage.error(message ? `${t('launcher.reorderFailed')}: ${message}` : t('launcher.reorderFailed'))
  }
}

function openProfileMenu(profile: Profile, event: MouseEvent) {
  suppressProfileClickUntil = 0
  selectProfile(profile)
  const menuWidth = 190
  const menuHeight = 122
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

function handleProfileMenuDocumentClick(event: MouseEvent) {
  const target = event.target
  if (target instanceof HTMLElement && target.closest('.profile-context-menu')) return
  closeProfileMenu()
}

function handleProfileMenuKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') closeProfileMenu()
}

async function duplicateProfileFromMenu() {
  const profile = menuProfile.value
  closeProfileMenu()
  if (!profile) return
  try {
    const name = t('launcher.copyName', { name: profile.name || t('launcher.unnamed') })
    const created = await postJSON<Profile>('/api/profiles/duplicate', { id: profile.id, name })
    await loadProfiles()
    applyProfile(created)
    await refreshStatus()
    ElMessage.success(t('launcher.profileDuplicated'))
  } catch (error) {
    ElMessage.error((error as Error).message)
  }
}

async function deleteProfileFromMenu() {
  const profile = menuProfile.value
  closeProfileMenu()
  if (profile) await deleteProfileByID(profile)
}

function openProfileInSftp(profile: Partial<Profile>) {
  if (!profile.id || profile.kind === 'local') return
  const name = (profile.name || profile.ssh_host || '').trim()
  if (window.parent && window.parent !== window) {
    window.parent.postMessage({ type: 'chemssh-launcher:open-sftp', profile: profile.id, name }, window.location.origin)
    return
  }
  const params = new URLSearchParams({ profile: profile.id })
  if (name) params.set('name', name)
  window.open(`/sftp?${params.toString()}`, '_blank')
}

function openCurrentInSftp() {
  if (!current.id || isLocalProfile.value) return
  openProfileInSftp(current)
}

function openMenuProfileInSftp() {
  const profile = menuProfile.value
  closeProfileMenu()
  if (profile) openProfileInSftp(profile)
}

async function loadProfiles() {
  profiles.value = await api<Profile[]>('/api/profiles')
  if (!current.id && !draftOpen.value && profiles.value[0]) applyProfile(profiles.value[0])
}

async function saveProfile() {
  const wasDraft = !current.id
  const payload = {
    profile: current,
    secrets: {
      password_action: passwordAction.value,
      password: passwordInput.value,
      passphrase_action: passphraseAction.value,
      passphrase: passphraseInput.value,
      security_token_action: securityTokenAction.value,
      security_token: securityTokenInput.value
    }
  }
  const saved = current.id
    ? await putJSON<Profile>(`/api/profiles/${current.id}`, payload)
    : await postJSON<Profile>('/api/profiles', payload)
  if (wasDraft) {
    draftOpen.value = false
    draftSnapshot.value = {}
    draftSecrets.value = { password: '', passphrase: '', securityToken: '' }
  }
  applyProfile(saved)
  await loadProfiles()
  await refreshLogs(true)
  ElMessage.success(t('launcher.profileSaved'))
}

async function deleteProfile() {
  const profile = profiles.value.find(item => item.id === current.id)
  if (profile) await deleteProfileByID(profile)
}

async function deleteProfileByID(profile: Profile) {
  if (!profile.id) return
  try {
    await ElMessageBox.confirm(t('launcher.confirmDelete', { name: profile.name || t('launcher.unnamed') }), t('launcher.confirmTitle'), { type: 'warning' })
  } catch {
    return
  }
  try {
    await api(`/api/profiles/${profile.id}`, { method: 'DELETE' })
  } catch (error) {
    ElMessage.error((error as Error).message)
    return
  }
  if (current.id === profile.id) applyProfile({})
  await loadProfiles()
  if (draftOpen.value) restoreDraft()
  await refreshStatus()
  ElMessage.success(t('launcher.profileDeleted'))
}

async function withHostKey<T>(path: string, payload: Record<string, unknown>) {
  try {
    return await postJSON<T>(path, payload)
  } catch (error) {
    const hostKey = hostKeyFromError(error)
    if (!hostKey) throw error
    const trust = await confirmHostKey(hostKey)
    return await postJSON<T>(path, { ...payload, ...trust })
  }
}

async function testProfile() {
  if (!current.id) return ElMessage.warning(t('launcher.warnSaveFirst'))
  await withHostKey('/api/profile-test', { id: current.id })
  ElMessage.success(t('launcher.sshTestPassed'))
}

function openTerminal() {
  if (!current.id || isLocalProfile.value) return
  const url = `/terminal?profile=${encodeURIComponent(current.id)}`
  if (window.parent && window.parent !== window) {
    window.parent.postMessage({ type: 'chemssh-launcher:new-tab', url }, window.location.origin)
    return
  }
  window.open(url, '_blank')
}

async function startSession() {
  if (!current.id) return ElMessage.warning(t('launcher.warnSaveFirst'))
  await withHostKey('/api/session/start', { id: current.id })
  await refreshStatus()
  ElMessage.success(t('launcher.requestedStart'))
}

function suppressSessionAutoOpen() {
  window.parent?.postMessage({ type: 'chemssh-launcher:suppress-auto-open' }, window.location.origin)
}

async function stopForwarding() {
  if (!current.id) return
  suppressSessionAutoOpen()
  await postJSON('/api/session/stop', { id: current.id })
  await refreshStatus()
}

async function stopService() {
  if (!current.id) return
  try {
    await ElMessageBox.confirm(t('launcher.confirmStop'), t('launcher.confirmTitle'), { type: 'warning' })
  } catch {
    return
  }
  suppressSessionAutoOpen()
  await postJSON('/api/session/stop-service', { id: current.id })
  await refreshStatus()
}

async function refreshStatus() {
  if (!profiles.value.length) {
    runningProfileIds.value = new Set()
    activeProfileId.value = ''
    return
  }
  const statuses = await Promise.all(profiles.value.map(profile =>
    api<{ running: boolean; id: string }>(`/api/session/status?profile_id=${encodeURIComponent(profile.id)}${profile.id === current.id ? '&activate=1' : ''}`)
      .catch(() => ({ running: false, id: profile.id }))
  ))
  const running = new Set<string>()
  statuses.forEach(status => {
    if (status.running && status.id) running.add(status.id)
  })
  runningProfileIds.value = running
  activeProfileId.value = current.id && running.has(current.id) ? current.id : ''
}

async function refreshLogs(forceScroll = false) {
  // Reconnect log stream to get fresh logs
  connectLogStream()
  if (forceScroll) {
    await nextTick()
    logScroller.scrollToBottom(true)
  }
}

watch(passwordInput, value => {
  if (value) passwordAction.value = 'replace'
})

watch(passphraseInput, value => {
  if (value) passphraseAction.value = 'replace'
})

watch(securityTokenInput, value => {
  if (value) securityTokenAction.value = 'replace'
})

// SSE for real-time log streaming
let logEventSource: EventSource | null = null

function connectLogStream() {
  if (logEventSource) {
    logEventSource.close()
  }

  if (!current.id) {
    logs.value = []
    return
  }

  const url = `/api/logs/stream?profile_id=${encodeURIComponent(current.id)}`
  logEventSource = new EventSource(url)

  // Clear logs and prepare for new stream
  logs.value = []

  logEventSource.onmessage = (event) => {
    logs.value.push(event.data)
    // Keep only last 500 lines
    if (logs.value.length > 500) {
      logs.value = logs.value.slice(-500)
    }
    nextTick(() => {
      logScroller.scrollToBottom(false)
    })
  }

  logEventSource.onerror = () => {
    // Auto-reconnect on error
    if (logEventSource) {
      logEventSource.close()
      logEventSource = null
    }
    // Reconnect after 2 seconds
    setTimeout(() => {
      if (current.id) connectLogStream()
    }, 2000)
  }
}

function disconnectLogStream() {
  if (logEventSource) {
    logEventSource.close()
    logEventSource = null
  }
}

// Watch for profile changes to reconnect log stream
watch(() => current.id, (newId) => {
  if (newId) {
    connectLogStream()
  } else {
    disconnectLogStream()
    logs.value = []
  }
})

// Status polling (still needed for session status)
let statusTimerId: number | null = null
const pollStatus = () => {
  const doPoll = async () => {
    if (!document.hidden) {
      await refreshStatus()
    }
    const interval = runningProfileIds.value.size > 0 ? 2000 : 10000
    statusTimerId = window.setTimeout(doPoll, interval)
  }
  doPoll()
}

// 页面可见性控制
const handleVisibilityChange = () => {
  if (document.hidden) {
    if (statusTimerId) {
      clearTimeout(statusTimerId)
      statusTimerId = null
    }
    disconnectLogStream()
  } else {
    pollStatus()
    if (current.id) connectLogStream()
  }
}

onMounted(async () => {
  defaults.value = await api<Profile>('/api/defaults')
  applyProfile({})
  await loadProfiles()
  await refreshStatus()

  // 启动状态轮询和日志流
  pollStatus()
  if (current.id) connectLogStream()

  // 监听页面可见性变化
  document.addEventListener('visibilitychange', handleVisibilityChange)
  document.addEventListener('click', handleProfileMenuDocumentClick)
  window.addEventListener('keydown', handleProfileMenuKeydown)
  window.addEventListener('scroll', closeProfileMenu, true)
  window.addEventListener('resize', closeProfileMenu)
})

// 清理定时器和连接
onBeforeUnmount(() => {
  if (statusTimerId) {
    clearTimeout(statusTimerId)
  }
  disconnectLogStream()
  document.removeEventListener('visibilitychange', handleVisibilityChange)
  document.removeEventListener('click', handleProfileMenuDocumentClick)
  window.removeEventListener('keydown', handleProfileMenuKeydown)
  window.removeEventListener('scroll', closeProfileMenu, true)
  window.removeEventListener('resize', closeProfileMenu)
})
</script>
