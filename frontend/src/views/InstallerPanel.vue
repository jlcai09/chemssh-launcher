<template>
  <component :is="embedded ? 'div' : 'section'" :class="embedded ? 'installer-panel is-embedded' : 'form-section installer-panel'">
    <h3 v-if="!embedded">{{ t('launcher.installer') }}</h3>
    <p class="field-hint">{{ t('launcher.installerHint') }}</p>
    <p v-if="!profileId" class="field-hint">{{ t('launcher.installerSaveFirst') }}</p>
    <template v-else>
      <div class="form-grid">
        <el-form-item :label="t('launcher.installerDir')">
          <el-input v-model="options.install_dir" :disabled="busy" placeholder="~/.chemssh" @change="persist" />
        </el-form-item>
        <el-form-item :label="t('launcher.installerPython')">
          <el-input v-model="options.python" :disabled="busy" :placeholder="t('launcher.installerPythonHint')" @change="persist" />
        </el-form-item>
        <el-form-item :label="t('launcher.installerPipIndex')">
          <el-select
            v-model="options.pip_index"
            :disabled="busy"
            filterable
            allow-create
            default-first-option
            style="width: 100%"
            :placeholder="t('launcher.installerPipIndexHint')"
            @change="persist"
          >
            <el-option
              v-for="item in pipIndexPresets"
              :key="item.value"
              :label="item.label"
              :value="item.value"
            />
          </el-select>
        </el-form-item>
        <el-form-item :label="t('launcher.installerProxyPrefix')">
          <el-select
            v-model="options.proxy_prefix"
            :disabled="busy"
            filterable
            allow-create
            default-first-option
            style="width: 100%"
            :placeholder="t('launcher.installerProxyPrefixHint')"
            @change="persist"
          >
            <el-option
              v-for="item in proxyPrefixPresets"
              :key="item.value"
              :label="item.label"
              :value="item.value"
            />
          </el-select>
        </el-form-item>
      </div>
      <el-checkbox v-model="options.ignore_pre_start" :disabled="busy" @change="persist">
        {{ t('launcher.installerIgnorePreStart') }}
      </el-checkbox>

      <div class="installer-actions">
        <el-button :disabled="busy" :loading="checking" @click="check">{{ t('launcher.installerCheck') }}</el-button>
        <el-button type="primary" :disabled="busy || !canRun" :loading="busy" @click="run('auto')">
          {{ primaryLabel }}
        </el-button>
        <el-button :disabled="busy || !status?.installed" @click="repair">{{ t('launcher.installerRepair') }}</el-button>
        <el-button v-if="phase === 'done' || (status?.installed && status.action === 'up_to_date')" :disabled="busy" @click="apply">
          {{ t('launcher.installerApply') }}
        </el-button>
      </div>

      <p v-if="summary" class="field-hint installer-summary">{{ summary }}</p>
      <p v-if="phase === 'failed'" class="field-hint installer-error">{{ t('launcher.installerFailed', { message: resultMessage }) }}</p>
      <p v-else-if="phase === 'done'" class="field-hint">{{ t('launcher.installerDone', { message: resultMessage }) }}</p>

      <details v-if="logLines.length" class="installer-log-box" open>
        <summary>{{ t('launcher.installerLogs') }}</summary>
        <pre ref="logEl" class="installer-log">{{ logLines.join('\n') }}</pre>
      </details>
    </template>
  </component>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, postJSON, type Profile } from '../api'
import { confirmHostKey, hostKeyFromError } from '../hostKey'
import { t } from '../i18n'

interface InstallerOptions {
  install_dir: string
  python: string
  pip_index: string
  proxy_prefix: string
  ignore_pre_start: boolean
}

interface InstallerCheck {
  install_dir: string
  installed: boolean
  installed_version: string
  latest_version: string
  action: 'install' | 'repair' | 'update_fast' | 'update_full' | 'up_to_date'
  deps_changed: boolean
  venv_ok: boolean
}

interface InstallerProgress {
  phase: 'idle' | 'running' | 'done' | 'failed'
  message: string
  lines: string[]
  next: number
}

const props = defineProps<{ profileId: string; embedded?: boolean }>()
const emit = defineEmits<{ (e: 'applied', profile: Profile): void }>()

const STORAGE_PREFIX = 'chemssh-launcher:installer:'
const DEFAULT_PIP_INDEX = 'https://pypi.tuna.tsinghua.edu.cn/simple'
const DEFAULT_PROXY_PREFIX = 'https://gh-proxy.com'

const pipIndexPresets = computed(() => [
  { label: t('launcher.pipPresetTuna'), value: 'https://pypi.tuna.tsinghua.edu.cn/simple' },
  { label: t('launcher.pipPresetAliyun'), value: 'https://mirrors.aliyun.com/pypi/simple' },
  { label: t('launcher.pipPresetUstc'), value: 'https://pypi.mirrors.ustc.edu.cn/simple' },
  { label: t('launcher.pipPresetTencent'), value: 'https://mirrors.cloud.tencent.com/pypi/simple' },
  { label: t('launcher.pipPresetOfficial'), value: '' }
])

const proxyPrefixPresets = computed(() => [
  { label: t('launcher.proxyPresetGhProxyCom'), value: 'https://gh-proxy.com' },
  { label: t('launcher.proxyPresetGhProxyNet'), value: 'https://ghproxy.net' },
  { label: t('launcher.proxyPresetAkams'), value: 'https://github.akams.cn' },
  { label: t('launcher.proxyPresetDirect'), value: '' }
])

const emptyOptions = (): InstallerOptions => ({
  install_dir: '~/.chemssh',
  python: '',
  pip_index: DEFAULT_PIP_INDEX,
  proxy_prefix: DEFAULT_PROXY_PREFIX,
  ignore_pre_start: true
})

const options = reactive<InstallerOptions>(emptyOptions())
const status = ref<InstallerCheck | null>(null)
const checking = ref(false)
const starting = ref(false)
const phase = ref<InstallerProgress['phase']>('idle')
const resultMessage = ref('')
const logLines = ref<string[]>([])
const logEl = ref<HTMLElement | null>(null)
let pollTimer: ReturnType<typeof setTimeout> | null = null
let nextLine = 0
let requestGen = 0

const busy = computed(() => starting.value || phase.value === 'running')
const canRun = computed(() => !!status.value && status.value.action !== 'up_to_date')

const primaryLabel = computed(() => {
  if (busy.value) return t('launcher.installerRunning')
  const action = status.value?.action
  if (action === 'install') return t('launcher.installerInstall')
  if (action === 'repair') return t('launcher.installerRepair')
  if (action === 'update_fast' || action === 'update_full') return t('launcher.installerUpdate')
  return t('launcher.installerInstall')
})

const summary = computed(() => {
  const s = status.value
  if (!s) return ''
  const params = { installed: s.installed_version || '-', latest: s.latest_version }
  switch (s.action) {
    case 'install': return t('launcher.installerNotInstalled', params)
    case 'update_fast': return t('launcher.installerUpdateFast', params)
    case 'update_full': return t('launcher.installerUpdateFull', params)
    case 'repair': return t('launcher.installerRepairNeeded', params)
    default: return t('launcher.installerUpToDate', params)
  }
})

function persist() {
  if (!props.profileId) return
  try {
    localStorage.setItem(STORAGE_PREFIX + props.profileId, JSON.stringify({ ...options }))
  } catch { /* storage unavailable */ }
}

function restore() {
  Object.assign(options, emptyOptions())
  try {
    const raw = localStorage.getItem(STORAGE_PREFIX + props.profileId)
    if (raw) Object.assign(options, JSON.parse(raw))
  } catch { /* ignore corrupt entries */ }
}

function payload(extra: Record<string, unknown> = {}) {
  return { profile_id: props.profileId, options: { ...options }, ...extra }
}

async function withHostKey<T>(path: string, body: Record<string, unknown>): Promise<T> {
  try {
    return await postJSON<T>(path, body)
  } catch (error) {
    const hostKey = hostKeyFromError(error)
    if (!hostKey) throw error
    const trust = await confirmHostKey(hostKey)
    return await postJSON<T>(path, { ...body, ...trust })
  }
}

function reportError(error: unknown) {
  // ElMessageBox rejects with 'cancel'/'close' when a dialog is dismissed
  if (error === 'cancel' || error === 'close') return
  ElMessage.error((error as Error).message || String(error))
}

async function check() {
  const reqProfileId = props.profileId
  const gen = requestGen
  checking.value = true
  try {
    const res = await withHostKey<InstallerCheck>('/api/installer/check', payload())
    if (gen !== requestGen || props.profileId !== reqProfileId) return
    status.value = res
  } catch (error) {
    if (gen !== requestGen || props.profileId !== reqProfileId) return
    reportError(error)
  } finally {
    if (gen === requestGen && props.profileId === reqProfileId) {
      checking.value = false
    }
  }
}

function stopPolling() {
  if (pollTimer) clearTimeout(pollTimer)
  pollTimer = null
}

async function poll() {
  stopPolling()
  const reqProfileId = props.profileId
  const gen = requestGen
  if (!reqProfileId) return
  const wasRunning = phase.value === 'running'
  try {
    const res = await api<InstallerProgress>(
      `/api/installer/status?profile_id=${encodeURIComponent(reqProfileId)}&since=${nextLine}`
    )
    if (gen !== requestGen || props.profileId !== reqProfileId) return
    if (res.lines.length) {
      logLines.value = [...logLines.value, ...res.lines].slice(-1500)
      nextLine = res.next
      await nextTick()
      if (gen !== requestGen || props.profileId !== reqProfileId) return
      if (logEl.value) logEl.value.scrollTop = logEl.value.scrollHeight
    }
    phase.value = res.phase === 'idle' ? 'idle' : res.phase
    resultMessage.value = res.message
    if (res.phase === 'running') {
      pollTimer = setTimeout(poll, 700)
    } else if (res.phase === 'done' && wasRunning) {
      await check()
    }
  } catch {
    if (gen !== requestGen || props.profileId !== reqProfileId) return
    pollTimer = setTimeout(poll, 2000)
  }
}

async function run(mode: 'auto' | 'repair') {
  const reqProfileId = props.profileId
  const gen = requestGen
  starting.value = true
  logLines.value = []
  nextLine = 0
  resultMessage.value = ''
  try {
    const result = await withHostKey<InstallerCheck>('/api/installer/run', payload({ mode }))
    if (gen !== requestGen || props.profileId !== reqProfileId) return
    status.value = result
    if (result.action === 'up_to_date') {
      phase.value = 'idle'
      return
    }
    phase.value = 'running'
    await poll()
  } catch (error) {
    if (gen !== requestGen || props.profileId !== reqProfileId) return
    phase.value = 'idle'
    reportError(error)
  } finally {
    if (gen === requestGen && props.profileId === reqProfileId) {
      starting.value = false
    }
  }
}

async function repair() {
  try {
    await ElMessageBox.confirm(t('launcher.installerRepairConfirm'), t('launcher.installerRepair'), { type: 'warning' })
  } catch {
    return
  }
  await run('repair')
}

async function apply() {
  try {
    await ElMessageBox.confirm(t('launcher.installerApplyConfirm'), t('launcher.installerApply'), { type: 'warning' })
    const profile = await postJSON<Profile>('/api/installer/apply', payload())
    ElMessage.success(t('launcher.installerApplied'))
    emit('applied', profile)
  } catch (error) {
    reportError(error)
  }
}

watch(
  () => props.profileId,
  async id => {
    requestGen += 1
    stopPolling()
    checking.value = false
    starting.value = false
    status.value = null
    phase.value = 'idle'
    resultMessage.value = ''
    logLines.value = []
    nextLine = 0
    restore()
    if (id) await poll() // resumes an install that is still running
  },
  { immediate: true }
)

onBeforeUnmount(stopPolling)
</script>

<style scoped>
.installer-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 12px;
}
.installer-summary {
  margin-top: 8px;
}
.installer-error {
  color: var(--el-color-danger);
}
.installer-log-box {
  margin-top: 8px;
}
.installer-log {
  max-height: 260px;
  overflow: auto;
  margin: 6px 0 0;
  padding: 8px;
  font-size: 12px;
  line-height: 1.45;
  white-space: pre-wrap;
  word-break: break-all;
  border-radius: 6px;
  background: var(--el-fill-color-light);
}
</style>
