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
          <el-button :icon="CaretRight" type="primary" :disabled="!current.id" :loading="phaseStarting" :title="t('launcher.start')" @click.stop="startSession">
            <span>{{ phaseStarting ? t('launcher.phaseStarting') : t('launcher.start') }}</span>
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
            <p v-if="localBindWarning" class="field-warning">
              <el-icon><WarningFilled /></el-icon>
              <span>{{ localBindWarning }}</span>
            </p>
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
              <div class="session-badge" :class="{ 'is-live': currentPhase === 'ready' }">
                <el-icon><Check v-if="currentPhase === 'ready'" /><CaretRight v-else /></el-icon>
              </div>
              <div class="session-copy">
                <div class="session-name">
                  {{ sessionHeadline }}
                </div>
                <div class="session-sub">
                  <span>{{ kindLabel }}</span><i />
                  <span>{{ authLabel }}</span><i />
                  <span>{{ current.local_url_path || '/' }}</span>
                </div>
              </div>
              <span class="session-pill" :class="{ 'is-live': currentPhase === 'ready', 'is-paused': currentPhase === 'paused', 'is-failed': currentPhase === 'failed' }"><i />{{ sessionPill }}</span>
            </div>

            <div v-if="currentFailure" class="session-failure">
              <div class="session-failure-head">
                <strong>{{ t('launcher.lastFailure') }}</strong>
                <button type="button" class="session-failure-link" @click="focusLogs">{{ t('launcher.viewLogs') }}</button>
              </div>
              <p>{{ currentFailure }}</p>
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
                <label v-if="logs.length" class="log-filter">
                  <el-icon><Search /></el-icon>
                  <input
                    v-model="logQuery"
                    type="text"
                    spellcheck="false"
                    autocomplete="off"
                    :placeholder="t('launcher.logFilterPlaceholder')"
                    :aria-label="t('launcher.logFilter')"
                  />
                  <button
                    v-if="logQuery"
                    type="button"
                    class="log-filter-clear"
                    :title="t('launcher.logFilterClear')"
                    @mousedown.prevent
                    @click="logQuery = ''"
                  >
                    <el-icon><Close /></el-icon>
                  </button>
                </label>
                <span v-if="logQuery" class="log-filter-count">{{ t('launcher.logFilterCount', { shown: visibleLogs.length, total: logs.length }) }}</span>
                <button type="button" class="card-icon-btn" :disabled="!logs.length" :title="t('launcher.copyLogs')" @click="copyLogs">
                  <el-icon><DocumentCopy /></el-icon>
                </button>
                <button type="button" class="card-icon-btn" :disabled="!logs.length" :title="t('launcher.exportLogs')" @click="exportLogs">
                  <el-icon><Download /></el-icon>
                </button>
                <button type="button" class="card-icon-btn" :title="t('launcher.refreshLogs')" @click="refreshLogs(true)">
                  <el-icon><Refresh /></el-icon>
                </button>
              </span>
            </div>
            <pre v-if="visibleLogs.length" ref="logRef" @scroll="logScroller.onScroll">{{ visibleLogs.join('\n') }}</pre>
            <p v-else-if="logs.length" class="logs-empty">{{ t('launcher.logsNoMatch', { query: logQuery }) }}</p>
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

    <el-dialog
      v-model="discardDialog.visible"
      :title="t('launcher.confirmDiscardTitle')"
      width="420px"
      align-center
      :close-on-click-modal="false"
      class="discard-changes-dialog"
      @closed="resolveDiscard('cancel')"
    >
      <p class="discard-changes-body">{{ t('launcher.confirmDiscardBody') }}</p>
      <template #footer>
        <div class="discard-changes-actions">
          <button type="button" class="discard-changes-cancel" @click="resolveDiscard('cancel')">{{ t('launcher.cancel') }}</button>
          <el-button @click="resolveDiscard('discard')">{{ t('launcher.confirmDiscardLeave') }}</el-button>
          <el-button type="primary" @click="resolveDiscard('save')">{{ t('launcher.confirmDiscardSave') }}</el-button>
        </div>
      </template>
    </el-dialog>
  </section>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { ArrowRight, CaretRight, Check, Clock, Close, CloseBold, Connection, Cpu, Delete, DocumentCopy, Download, FolderOpened, Lock, Monitor, MoreFilled, Plus, Refresh, Search, Setting, SwitchButton, WarningFilled } from '@element-plus/icons-vue'
import { api, postJSON, putJSON, type Profile } from '../api'
import { createAutoScroll } from '../autoScroll'
import { confirmHostKey, hostKeyFromError } from '../hostKey'
import { t } from '../i18n'

type SessionPhase = 'stopped' | 'starting' | 'ready' | 'paused' | 'failed'

interface SessionStatus {
  running: boolean
  open_seq?: number
  phase: SessionPhase
  last_failure?: { message: string; at: string } | null
}

const defaults = ref<Profile | null>(null)
const profiles = ref<Profile[]>([])
const searchQuery = ref('')
const current = reactive<Profile>({} as Profile)
const draftOpen = ref(false)
const draftSnapshot = ref<Partial<Profile>>({})
const draftSecrets = ref({ password: '', passphrase: '', securityToken: '' })
const activeProfileId = ref('')
const runningProfileIds = ref(new Set<string>())
const statusByProfile = ref<Record<string, SessionStatus>>({})
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
const logQuery = ref('')
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
const currentStatus = computed<SessionStatus | null>(() => (current.id ? statusByProfile.value[current.id] || null : null))
const currentPhase = computed<SessionPhase>(() => currentStatus.value?.phase || 'stopped')
const phaseStarting = computed(() => currentPhase.value === 'starting')
const phaseLabel = computed(() => {
  switch (currentPhase.value) {
    case 'starting': return t('launcher.phaseStarting')
    case 'ready': return t('launcher.phaseReady')
    case 'paused': return t('launcher.phasePaused')
    case 'failed': return t('launcher.phaseFailed')
    default: return t('launcher.phaseStopped')
  }
})
const phaseShortLabel = computed(() => {
  switch (currentPhase.value) {
    case 'starting': return t('launcher.phaseStartingShort')
    case 'ready': return t('launcher.phaseReadyShort')
    case 'paused': return t('launcher.phasePausedShort')
    case 'failed': return t('launcher.phaseFailedShort')
    default: return t('launcher.phaseStoppedShort')
  }
})
const currentFailure = computed(() => (currentPhase.value === 'failed' ? currentStatus.value?.last_failure?.message || '' : ''))
const sessionHeadline = computed(() => {
  if (!current.id) return t('launcher.sessionIdle')
  if (currentPhase.value === 'ready') return t('launcher.sessionRunning')
  if (currentPhase.value === 'stopped') return t('launcher.sessionIdle')
  return phaseLabel.value
})
const sessionPill = computed(() => (current.id ? phaseShortLabel.value : t('launcher.idle')))
const visibleLogs = computed(() => {
  const query = logQuery.value.trim().toLowerCase()
  if (!query) return logs.value
  return logs.value.filter(line => line.toLowerCase().includes(query))
})
// Mirrors config.Profile.UsesNonLoopbackLocalHost; only the tunnel profile has a
// bind address that can expose ChemSSH to other machines.
const localBindWarning = computed(() => {
  if (isLocalProfile.value) return ''
  const host = (current.local_host || '').trim()
  if (!host || host === '127.0.0.1' || host === 'localhost' || host === '::1') return ''
  return t('launcher.warnNonLoopbackBind', { host })
})
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

type DiscardChoice = 'save' | 'discard' | 'cancel'

// Dropping unsaved edits has three possible outcomes, which the two-button
// message box cannot express, so the confirm is a real dialog.
const discardDialog = reactive<{ visible: boolean; resolve: ((choice: DiscardChoice) => void) | null }>({
  visible: false,
  resolve: null
})

function resolveDiscard(choice: DiscardChoice) {
  const resolve = discardDialog.resolve
  discardDialog.resolve = null
  discardDialog.visible = false
  resolve?.(choice)
}

async function confirmDiscardChanges(): Promise<boolean> {
  if (!isDirty.value) return true
  // Never stack two prompts: the second would silently strand the first promise.
  if (discardDialog.resolve) return false
  const choice = await new Promise<DiscardChoice>(resolve => {
    discardDialog.resolve = resolve
    discardDialog.visible = true
  })
  if (choice === 'cancel') return false
  if (choice === 'discard') return true
  return await saveProfile()
}

async function selectDraft() {
  if (!draftOpen.value || draftActive.value) return
  if (!(await confirmDiscardChanges())) return
  restoreDraft()
}

async function newProfile() {
  if (draftOpen.value) {
    await selectDraft()
    return
  }
  if (!(await confirmDiscardChanges())) return
  draftOpen.value = true
  draftSnapshot.value = {}
  draftSecrets.value = { password: '', passphrase: '', securityToken: '' }
  applyProfile({})
  logs.value = []
}

async function selectProfile(profile: Profile) {
  if (Date.now() < suppressProfileClickUntil) return
  if (profile.id === current.id) return
  if (!(await confirmDiscardChanges())) return
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

// An empty input means "leave the stored secret untouched". Sending "replace"
// with an empty value would wipe the saved secret and leave the profile with
// has_password=true but nothing to authenticate with.
function effectiveSecretAction(action: 'keep' | 'replace' | 'clear', value: string) {
  return action === 'replace' && !value ? 'keep' : action
}

async function saveProfile() {
  const wasDraft = !current.id
  const payload = {
    profile: current,
    secrets: {
      password_action: effectiveSecretAction(passwordAction.value, passwordInput.value),
      password: passwordInput.value,
      passphrase_action: effectiveSecretAction(passphraseAction.value, passphraseInput.value),
      passphrase: passphraseInput.value,
      security_token_action: effectiveSecretAction(securityTokenAction.value, securityTokenInput.value),
      security_token: securityTokenInput.value
    }
  }
  let saved: Profile
  try {
    saved = current.id
      ? await putJSON<Profile>(`/api/profiles/${current.id}`, payload)
      : await postJSON<Profile>('/api/profiles', payload)
  } catch (error) {
    ElMessage.error((error as Error).message)
    return false
  }
  if (wasDraft) {
    draftOpen.value = false
    draftSnapshot.value = {}
    draftSecrets.value = { password: '', passphrase: '', securityToken: '' }
  }
  applyProfile(saved)
  await loadProfiles()
  await refreshLogs(true)
  ElMessage.success(t('launcher.profileSaved'))
  return true
}

async function deleteProfile() {
  const profile = profiles.value.find(item => item.id === current.id)
  if (profile) await deleteProfileByID(profile)
}

async function deleteProfileByID(profile: Profile) {
  if (!profile.id) return
  const name = profile.name || t('launcher.unnamed')
  const running = Boolean(statusByProfile.value[profile.id]?.running)
  try {
    await ElMessageBox.confirm(
      running ? t('launcher.confirmDeleteRunning', { name }) : t('launcher.confirmDelete', { name }),
      t('launcher.confirmTitle'),
      { type: 'warning', confirmButtonText: t('launcher.actions.delete'), cancelButtonText: t('launcher.cancel') }
    )
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
  try {
    await withHostKey('/api/profile-test', { id: current.id })
    ElMessage.success(t('launcher.sshTestPassed'))
  } catch (error) {
    // ElMessageBox rejects with 'cancel'/'close' when the host key dialog is dismissed
    if (error === 'cancel' || error === 'close') return
    ElMessage.error((error as Error).message)
  }
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
  try {
    await withHostKey('/api/session/start', { id: current.id })
    await refreshStatus()
    ElMessage.success(t('launcher.requestedStart'))
  } catch (error) {
    // ElMessageBox rejects with 'cancel'/'close' when the host key dialog is dismissed
    if (error === 'cancel' || error === 'close') return
    ElMessage.error((error as Error).message)
  }
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

let activatedSessionKey = ''
// A single failed poll is often just a dropped request; only after repeated
// failures do we treat the cached status as stale and clear it.
let statusPollFailures = 0

async function refreshStatus() {
  if (!profiles.value.length) {
    statusPollFailures = 0
    statusByProfile.value = {}
    runningProfileIds.value = new Set()
    activeProfileId.value = ''
    return
  }
  try {
    const res = await api<{ statuses: Record<string, SessionStatus> }>('/api/session/statuses')
    statusByProfile.value = res.statuses || {}
    statusPollFailures = 0
  } catch {
    statusPollFailures += 1
    if (statusPollFailures >= 2) {
      statusByProfile.value = {}
      runningProfileIds.value = new Set()
      activeProfileId.value = ''
      // Promotion is keyed off the status we just discarded, so re-validate it
      // on the next successful poll instead of trusting a stale marker.
      activatedSessionKey = ''
    }
    return
  }
  const running = new Set<string>()
  Object.entries(statusByProfile.value).forEach(([id, status]) => {
    if (status.running) running.add(id)
  })
  runningProfileIds.value = running
  activeProfileId.value = current.id && running.has(current.id) ? current.id : ''
  await syncActiveProxy()
}

// Promotion changes which session the proxy serves, so it runs once per
// session instead of on every two-second poll.
async function syncActiveProxy() {
  const status = currentStatus.value
  if (!current.id || !status?.running) return
  const key = `${current.id}:${status.open_seq || 0}`
  if (key === activatedSessionKey) return
  activatedSessionKey = key
  try {
    await postJSON('/api/session/activate', { id: current.id })
  } catch {
    activatedSessionKey = ''
  }
}

async function refreshLogs(forceScroll = false) {
  // Reconnect log stream to get fresh logs
  connectLogStream()
  if (forceScroll) {
    await nextTick()
    logScroller.scrollToBottom(true)
  }
}

function focusLogs() {
  logRef.value?.scrollIntoView({ block: 'center', behavior: 'smooth' })
}

async function copyLogs() {
  if (!logs.value.length) return
  try {
    await navigator.clipboard.writeText(logs.value.join('\n'))
    ElMessage.success(t('launcher.logsCopied'))
  } catch {
    ElMessage.error(t('launcher.logsCopyFailed'))
  }
}

function exportLogs() {
  if (!logs.value.length) return
  const name = (current.name || 'chemssh').replace(/[\\/:*?"<>|]/g, '-')
  const blob = new Blob([logs.value.join('\n')], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = `${name}-logs.txt`
  document.body.appendChild(link)
  link.click()
  link.remove()
  URL.revokeObjectURL(url)
}

function syncActionFromInput(value: string, action: { value: 'keep' | 'replace' | 'clear' }) {
  if (value) {
    action.value = 'replace'
    return
  }
  if (action.value === 'replace') action.value = 'keep'
}

watch(passwordInput, value => syncActionFromInput(value, passwordAction))
watch(passphraseInput, value => syncActionFromInput(value, passphraseAction))
watch(securityTokenInput, value => syncActionFromInput(value, securityTokenAction))

// SSE for real-time log streaming
let logEventSource: EventSource | null = null
let replayLines: string[] | null = null

function appendLogLine(line: string) {
  logs.value.push(line)
  // Keep only last 500 lines
  if (logs.value.length > 500) {
    logs.value = logs.value.slice(-500)
  }
}

function connectLogStream() {
  if (logEventSource) {
    logEventSource.close()
    logEventSource = null
  }

  if (!current.id) {
    replayLines = null
    logs.value = []
    return
  }

  const url = `/api/logs/stream?profile_id=${encodeURIComponent(current.id)}`
  const source = new EventSource(url)
  logEventSource = source
  // Buffer the replayed history and swap it in when the server marks the end,
  // so a reconnect neither blanks the panel nor duplicates every line.
  replayLines = []

  source.onmessage = (event) => {
    if (logEventSource !== source) return
    if (replayLines) {
      replayLines.push(event.data)
      return
    }
    appendLogLine(event.data)
    nextTick(() => {
      logScroller.scrollToBottom(false)
    })
  }

  source.addEventListener('replay-end', () => {
    if (logEventSource !== source) return
    const replayed = replayLines || []
    replayLines = null
    logs.value = replayed.slice(-500)
    nextTick(() => {
      logScroller.scrollToBottom(false)
    })
  })

  source.onerror = () => {
    // Ignore errors from streams that were already replaced, otherwise an old
    // stream would close the new one and schedule an extra reconnect.
    if (logEventSource !== source) return
    logEventSource.close()
    logEventSource = null
    replayLines = null
    // Reconnect after 2 seconds
    setTimeout(() => {
      if (current.id && !logEventSource) connectLogStream()
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
watch(() => current.id, (newId, oldId) => {
  if (newId === oldId) return
  logs.value = []
  // A filter left over from the previous profile would hide the new profile's
  // output and look like an empty log.
  logQuery.value = ''
  if (newId) {
    connectLogStream()
  } else {
    disconnectLogStream()
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

const handleBeforeUnload = (event: BeforeUnloadEvent) => {
  if (!isDirty.value) return
  event.preventDefault()
  event.returnValue = ''
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
  window.addEventListener('beforeunload', handleBeforeUnload)
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
  window.removeEventListener('beforeunload', handleBeforeUnload)
  document.removeEventListener('click', handleProfileMenuDocumentClick)
  window.removeEventListener('keydown', handleProfileMenuKeydown)
  window.removeEventListener('scroll', closeProfileMenu, true)
  window.removeEventListener('resize', closeProfileMenu)
})
</script>
