<template>
  <section class="launcher-view">
    <aside class="profile-sidebar">
      <div class="panel-header">
        <div>
          <strong>{{ t('launcher.serverConfig') }}</strong>
          <p>{{ t('launcher.profileCount', { count: profiles.length }) }}</p>
        </div>
        <div class="header-actions">
          <el-tooltip :content="t('launcher.newProfile')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false">
            <el-button :icon="Plus" circle type="primary" @click="newProfile" />
          </el-tooltip>
          <el-tooltip :content="t('launcher.refresh')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false">
            <el-button :icon="Refresh" circle @click="loadProfiles" />
          </el-tooltip>
        </div>
      </div>
      <div class="profile-list">
        <button
          v-for="profile in profiles"
          :key="profile.id"
          class="profile-card"
          :class="{ 'is-active': current.id === profile.id }"
          type="button"
          @click="selectProfile(profile)"
        >
          <span class="state-dot" :class="{ running: runningProfileIds.has(profile.id) }" />
          <span>
            <strong>{{ profile.name || t('launcher.unnamed') }}</strong>
            <small>{{ profileSummary(profile) }}</small>
          </span>
        </button>
        <p v-if="!profiles.length" class="empty">{{ t('launcher.emptyProfiles') }}</p>
      </div>
    </aside>

    <section class="launcher-main">
      <div class="launcher-toolbar">
        <div>
          <h2>{{ current.name || t('launcher.unsaved') }}</h2>
          <p>{{ current.id ? profileSummary(current) : t('launcher.noProfileHint') }}</p>
        </div>
        <div class="toolbar-actions">
          <el-tooltip :content="t('launcher.sshTest')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false">
            <el-button :icon="Connection" circle :disabled="!current.id" @click="testProfile" />
          </el-tooltip>
          <el-tooltip :content="t('launcher.start')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false">
            <el-button :icon="VideoPlay" circle type="primary" :disabled="!current.id" @click="startSession" />
          </el-tooltip>
          <el-tooltip :content="t('launcher.stopForwarding')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false">
            <el-button :icon="SwitchButton" circle @click="stopForwarding" />
          </el-tooltip>
          <el-tooltip :content="t('launcher.stopService')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false">
            <el-button :icon="CircleClose" circle type="danger" @click="stopService" />
          </el-tooltip>
        </div>
      </div>

      <div class="launcher-grid">
        <el-form class="profile-editor" label-position="top" :model="current" @submit.prevent="saveProfile">
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
              <el-form-item :label="t('launcher.localURLPath')"><el-input v-model="current.local_url_path" :placeholder="t('launcher.placeholder.localURLPath')" /></el-form-item>
              <el-form-item :label="t('launcher.healthURL')"><el-input v-model="current.health_check_url" :placeholder="t('launcher.placeholder.healthURL')" /></el-form-item>
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

          <div class="editor-actions">
            <el-button :icon="Delete" type="danger" :disabled="!current.id" @click="deleteProfile">{{ t('launcher.actions.delete') }}</el-button>
            <el-button :icon="Check" type="primary" native-type="submit">{{ t('launcher.actions.save') }}</el-button>
          </div>
        </el-form>

        <section class="log-panel">
          <div class="logs-title">
            <h2>{{ t('launcher.logs') }}</h2>
            <el-tooltip :content="t('launcher.refreshLogs')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false">
              <el-button :icon="Refresh" circle @click="refreshLogs" />
            </el-tooltip>
          </div>
          <pre ref="logRef" @scroll="logScroller.onScroll">{{ logs.join('\n') }}</pre>
        </section>
      </div>
    </section>
  </section>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Check, CircleClose, Connection, Delete, Plus, Refresh, SwitchButton, VideoPlay } from '@element-plus/icons-vue'
import { api, APIError, postJSON, putJSON, type HostKeyInfo, type Profile } from '../api'
import { createAutoScroll } from '../autoScroll'
import { t } from '../i18n'

const defaults = ref<Profile | null>(null)
const profiles = ref<Profile[]>([])
const current = reactive<Profile>({} as Profile)
const activeProfileId = ref('')
const runningProfileIds = ref(new Set<string>())
const logs = ref<string[]>([])
const logRef = ref<HTMLElement | null>(null)
const logScroller = createAutoScroll(() => logRef.value)
const passwordInput = ref('')
const passphraseInput = ref('')
const passwordAction = ref<'keep' | 'replace' | 'clear'>('keep')
const passphraseAction = ref<'keep' | 'replace' | 'clear'>('keep')
const isLocalProfile = computed(() => current.kind === 'local')

function applyProfile(profile: Partial<Profile>) {
  Object.assign(current, defaults.value, profile)
  current.kind = current.kind || 'remote'
  passwordInput.value = ''
  passphraseInput.value = ''
  passwordAction.value = 'keep'
  passphraseAction.value = 'keep'
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
  if (profile.kind === 'local') return `local ${profile.local_host || '127.0.0.1'}:${profile.local_port || 8888}`
  return `${profile.ssh_user || '?'}@${profile.ssh_host || '?'}`
}

function newProfile() {
  applyProfile({})
  logs.value = []
}

function selectProfile(profile: Profile) {
  applyProfile(profile)
  void refreshLogs(true)
}

async function loadProfiles() {
  profiles.value = await api<Profile[]>('/api/profiles')
  if (!current.id && profiles.value[0]) applyProfile(profiles.value[0])
}

async function saveProfile() {
  const payload = {
    profile: current,
    secrets: {
      password_action: passwordAction.value,
      password: passwordInput.value,
      passphrase_action: passphraseAction.value,
      passphrase: passphraseInput.value
    }
  }
  const saved = current.id
    ? await putJSON<Profile>(`/api/profiles/${current.id}`, payload)
    : await postJSON<Profile>('/api/profiles', payload)
  applyProfile(saved)
  await loadProfiles()
  await refreshLogs(true)
  ElMessage.success(t('launcher.profileSaved'))
}

async function deleteProfile() {
  if (!current.id) return
  await ElMessageBox.confirm(t('launcher.confirmDelete', { name: current.name || t('launcher.unnamed') }), t('launcher.confirmTitle'), { type: 'warning' })
  await api(`/api/profiles/${current.id}`, { method: 'DELETE' })
  applyProfile({})
  await loadProfiles()
  ElMessage.success(t('launcher.profileDeleted'))
}

async function withHostKey<T>(path: string, payload: Record<string, unknown>) {
  try {
    return await postJSON<T>(path, payload)
  } catch (error) {
    const err = error as APIError
    const hostKey = err.data?.host_key as HostKeyInfo | undefined
    if (!hostKey || hostKey.mismatch) throw error
    await ElMessageBox.confirm(
      t('launcher.hostKeyConfirm', {
        address: hostKey.address,
        keyType: hostKey.key_type,
        fingerprint: hostKey.fingerprint,
        path: hostKey.known_hosts_path
      }),
      t('launcher.hostKeyTitle'),
      { type: 'warning' }
    )
    return await postJSON<T>(path, { ...payload, accept_host_key: true })
  }
}

async function testProfile() {
  if (!current.id) return ElMessage.warning(t('launcher.warnSaveFirst'))
  await withHostKey('/api/profile-test', { id: current.id })
  await refreshLogs()
  ElMessage.success(t('launcher.sshTestPassed'))
}

async function startSession() {
  if (!current.id) return ElMessage.warning(t('launcher.warnSaveFirst'))
  await withHostKey('/api/session/start', { id: current.id })
  await refreshStatus()
  await refreshLogs()
  ElMessage.success(t('launcher.requestedStart'))
}

async function stopForwarding() {
  await postJSON('/api/session/stop', { id: current.id })
  await refreshStatus()
  await refreshLogs()
}

async function stopService() {
  await ElMessageBox.confirm(t('launcher.confirmStop'), t('launcher.confirmTitle'), { type: 'warning' })
  await postJSON('/api/session/stop-service', { id: current.id })
  await refreshStatus()
  await refreshLogs()
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
  if (!current.id) {
    logs.value = []
    return
  }
  const data = await api<{ lines: string[] }>(`/api/logs?profile_id=${encodeURIComponent(current.id)}`)
  logs.value = data.lines || []
  await nextTick()
  logScroller.scrollToBottom(forceScroll)
}

watch(passwordInput, value => {
  if (value) passwordAction.value = 'replace'
})

watch(passphraseInput, value => {
  if (value) passphraseAction.value = 'replace'
})

onMounted(async () => {
  defaults.value = await api<Profile>('/api/defaults')
  applyProfile({})
  await loadProfiles()
  await refreshStatus()
  await refreshLogs()
  window.setInterval(refreshStatus, 2000)
  window.setInterval(refreshLogs, 2000)
})
</script>
