<template>
  <section class="terminal-view">
    <div class="terminal-card">
      <header class="terminal-toolbar">
        <div class="terminal-toolbar-main">
          <el-select
            v-model="selectedProfileID"
            class="terminal-profile-select"
            :placeholder="t('terminal.selectProfile')"
            filterable
            :disabled="connected || connecting"
          >
            <el-option
              v-for="profile in remoteProfiles"
              :key="profile.id"
              :label="profileLabel(profile)"
              :value="profile.id"
            />
          </el-select>
          <el-button
            v-if="!connected"
            type="primary"
            :icon="CaretRight"
            :loading="connecting"
            :disabled="!selectedProfileID"
            @click="connect"
          >
            {{ t('terminal.connect') }}
          </el-button>
          <el-button v-else type="danger" :icon="SwitchButton" @click="disconnect(false)">
            {{ t('terminal.disconnect') }}
          </el-button>
          <el-tooltip :content="t('terminal.clear')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button :icon="Delete" circle @click="clearTerminal" />
          </el-tooltip>
          <span
            class="terminal-upload-ring"
            :class="{ 'is-active': uploading }"
            :style="{ '--terminal-upload-progress': uploadPercent }"
          >
            <el-tooltip :content="t('terminal.upload')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
              <el-button :icon="Upload" circle :disabled="!connected || uploading" @click="pickAndUploadManually" />
            </el-tooltip>
            <span v-if="uploading" class="terminal-upload-percent">{{ uploadPercent }}%</span>
          </span>
        </div>
        <div class="terminal-status" :class="`is-${statusKind}`">
          <span class="state-dot" :class="{ running: statusKind === 'connected' }" />
          <span>{{ statusText }}</span>
        </div>
      </header>
      <div
        class="terminal-stage"
        @dragenter="handleFileDragEnter"
        @dragover="handleFileDragOver"
        @dragleave="handleFileDragLeave"
        @drop="handleFileDrop"
      >
        <div ref="terminalHost" class="terminal-host" />
        <div v-if="!remoteProfiles.length" class="terminal-placeholder">
          <strong>{{ t('terminal.noProfiles') }}</strong>
          <span>{{ t('terminal.noProfilesHint') }}</span>
        </div>
        <div v-if="dropActive" class="terminal-drop-overlay">
          <el-icon><UploadFilled /></el-icon>
          <strong>{{ !connected ? t('terminal.connectFirst') : uploading ? t('terminal.uploadBusy') : t('terminal.dropToUpload') }}</strong>
          <span v-if="connected && !uploading">{{ t('terminal.uploadTarget', { path: uploadTargetLabel }) }}</span>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { CaretRight, Delete, SwitchButton, Upload, UploadFilled } from '@element-plus/icons-vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { api, APIError, postJSON, withLauncherToken, type HostKeyInfo, type Profile } from '../api'
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

const profiles = ref<Profile[]>([])
const selectedProfileID = ref(localStorage.getItem(TERMINAL_PROFILE_KEY) || '')
const connecting = ref(false)
const connected = ref(false)
const statusKind = ref<'idle' | 'connecting' | 'connected' | 'error'>('idle')
const terminalHost = ref<HTMLElement | null>(null)
const currentDir = ref('')
const uploading = ref(false)
const uploadPercent = ref(0)
const dropActive = ref(false)

let sftpSessionID = ''
let sftpProfileID = ''
let sftpReleasePending = false

const remoteProfiles = computed(() => profiles.value.filter(profile => profile.kind !== 'local'))
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

let term: Terminal | null = null
const fitAddon = new FitAddon()
let resizeObserver: ResizeObserver | null = null
let outputAbort: AbortController | null = null
let sessionID = ''
let disposed = false
let pendingInput = ''
let inputFlushing = false
let resizeTimer: number | null = null
let lastCols = 0
let lastRows = 0
let controlBuffer = ''
let zmodemProbe = ''
let zmodemIntercepted = false

const ZMODEM_SIGNATURES = [
  'rz waiting to receive',
  '**b00000000000000',
  '**b0100',
  '**\x18b00000000000000',
  '**\x18b0100'
]

function profileLabel(profile: Profile) {
  const name = profile.name || profile.ssh_host || profile.id
  return `${name}  (${profile.ssh_user || '?'}@${profile.ssh_host || '?'})`
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

function terminalSize() {
  const proposed = fitAddon.proposeDimensions()
  return {
    cols: proposed?.cols || term?.cols || 80,
    rows: proposed?.rows || term?.rows || 24
  }
}

function writeNotice(text: string) {
  term?.write(`\r\n\x1b[90m${text}\x1b[0m\r\n`)
}

function resetStreamState() {
  currentDir.value = ''
  controlBuffer = ''
  zmodemProbe = ''
  zmodemIntercepted = false
}

function setCurrentDir(value: string) {
  const path = value.replace(/\r/g, '').trim()
  if (!path || /[\r\n\0]/.test(path)) return
  currentDir.value = path
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

function handleOsc(body: string) {
  if (body.startsWith('633;P;Cwd=')) {
    setCurrentDir(body.slice('633;P;Cwd='.length))
    return ''
  }
  if (body.startsWith('7;file://')) {
    const path = parseOsc7Cwd(body)
    if (path) {
      setCurrentDir(path)
      return ''
    }
  }
  if (body.startsWith('777;chemssh-transfer;')) {
    handleTransferMarker(body.slice('777;chemssh-transfer;'.length))
    return ''
  }
  return `\x1b]${body}\x07`
}

function filterZmodem(text: string) {
  if (zmodemIntercepted) return text
  const probe = (zmodemProbe + text).toLowerCase()
  zmodemProbe = probe.slice(-96)
  const signature = ZMODEM_SIGNATURES.find(item => probe.includes(item))
  if (!signature) return text
  zmodemIntercepted = true
  void interceptNativeZmodem(signature)
  const index = text.toLowerCase().indexOf(signature)
  return index < 0 ? '' : text.slice(0, index)
}

function consumeTerminalOutput(data: string) {
  const text = controlBuffer + data
  controlBuffer = ''
  let visible = ''
  let index = 0
  while (index < text.length) {
    const start = text.indexOf('\x1b]', index)
    if (start < 0) {
      const tail = text.slice(index)
      if (tail.endsWith('\x1b')) {
        visible += tail.slice(0, -1)
        controlBuffer = '\x1b'
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
      controlBuffer = rest
      break
    }
    visible += handleOsc(rest.slice(2, end))
    index = start + end + terminatorLength
  }
  if (controlBuffer.length > 65536) {
    visible += controlBuffer
    controlBuffer = ''
  }
  if (!visible) return
  const filtered = filterZmodem(visible)
  if (filtered) term?.write(filtered)
}

function fitTerminal() {
  if (!props.active || !terminalHost.value) return
  if (terminalHost.value.clientWidth <= 0 || terminalHost.value.clientHeight <= 0) return
  try {
    fitAddon.fit()
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
  if (!remoteProfiles.value.some(profile => profile.id === selectedProfileID.value)) {
    selectedProfileID.value = remoteProfiles.value[0]?.id || ''
  }
}

async function openShell(profileID: string) {
  const size = terminalSize()
  const payload = { profile_id: profileID, cols: size.cols, rows: size.rows }
  try {
    return await postJSON<{ id: string }>('/api/session/shell/open', payload)
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
    return await postJSON<{ id: string }>('/api/session/shell/open', { ...payload, accept_host_key: true })
  }
}

async function connectSftp(profileID: string) {
  if (sftpSessionID && sftpProfileID === profileID) return sftpSessionID
  await disconnectSftp()
  const request = (acceptHostKey: boolean) =>
    postJSON<{ id: string }>('/api/sftp/connect', { id: profileID, accept_host_key: acceptHostKey })
  let session: { id: string }
  try {
    session = await request(false)
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
    session = await request(true)
  }
  sftpSessionID = session.id
  sftpProfileID = profileID
  sftpReleasePending = false
  return sftpSessionID
}

async function disconnectSftp() {
  const id = sftpSessionID
  sftpSessionID = ''
  sftpProfileID = ''
  if (!id) return
  await postJSON('/api/sftp/disconnect', { session: id }).catch(() => undefined)
}

function releaseSftpIfIdle() {
  if (uploading.value) {
    sftpReleasePending = true
    return
  }
  void disconnectSftp()
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

async function resolveUploadDir(sessionID: string, target?: string | null) {
  const raw = (target ?? currentDir.value).trim().replace(/[\r\n\0]/g, '')
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

async function uploadEntriesToDir(entries: UploadEntry[], target: string | null) {
  if (uploading.value) {
    ElMessage.info(t('terminal.uploadBusy'))
    return
  }
  uploading.value = true
  uploadPercent.value = 0
  sftpReleasePending = false
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
    const sessionID = await connectSftp(selectedProfileID.value)
    const targetDir = await resolveUploadDir(sessionID, target)
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
        uploadPercent.value = Math.floor((done / totalBytes) * 100)
      })
      sentBytes = entryStart + entry.file.size
      uploadPercent.value = Math.floor((sentBytes / totalBytes) * 100)
      uploadedCount += 1
    }
    ElMessage.success(t('terminal.uploadDone', { count: uploadedCount, dir: targetDir }))
  } catch (error) {
    if (error === 'cancel' || error === 'close') return
    ElMessage.error(`${t('terminal.uploadFailed')}: ${errorMessage(error)}`)
  } finally {
    uploading.value = false
    uploadPercent.value = 0
    if (sftpReleasePending && !connected.value) {
      sftpReleasePending = false
      await disconnectSftp()
    }
  }
}

async function uploadDroppedFiles(event: DragEvent) {
  try {
    const entries = await collectDropUploadEntries(event)
    await uploadEntriesToDir(entries, null)
  } catch (error) {
    if (error === 'cancel' || error === 'close') return
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

function handleTransferMarker(raw: string) {
  const transfer = parseTransferMarker(raw)
  if (!transfer) return
  if (transfer.direction === 'upload') {
    void pickAndUploadFromShell(transfer.cwd)
    return
  }
  void downloadFromShell(transfer.cwd, transfer.argv)
}

async function pickAndUploadManually() {
  if (!connected.value) {
    ElMessage.warning(t('terminal.connectFirst'))
    return
  }
  if (uploading.value) {
    ElMessage.info(t('terminal.uploadBusy'))
    return
  }
  const files = await pickTransferFiles()
  if (!files.length) {
    ElMessage.info(t('terminal.transferCancelled'))
    return
  }
  await uploadEntriesToDir(filesToUploadEntries(files), null)
}

async function pickAndUploadFromShell(cwd: string) {
  if (uploading.value) {
    ElMessage.info(t('terminal.uploadBusy'))
    return
  }
  const files = await pickTransferFiles()
  if (!files.length) {
    ElMessage.info(t('terminal.transferCancelled'))
    return
  }
  await uploadEntriesToDir(filesToUploadEntries(files), cwd || null)
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

async function downloadFromShell(cwd: string, argv: string[]) {
  const paths = downloadPathsFromArgv(cwd, argv)
  if (!paths.length) {
    ElMessage.warning(t('terminal.transferNoFiles'))
    return
  }
  try {
    const sessionID = await connectSftp(selectedProfileID.value)
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

async function sendRawInput(data: string) {
  if (!sessionID) return
  const res = await fetch(`/api/session/shell/input?id=${encodeURIComponent(sessionID)}`, {
    method: 'POST',
    headers: withLauncherToken({ 'Content-Type': 'text/plain; charset=utf-8' }),
    body: data
  })
  if (!res.ok) throw new Error(await responseError(res))
}

async function interceptNativeZmodem(signature: string) {
  await sendRawInput('\x03').catch(() => undefined)
  if (signature.includes('rz waiting')) {
    ElMessage.warning(t('terminal.nativeRzIntercepted'))
    window.setTimeout(() => {
      void pickAndUploadFromShell(currentDir.value)
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
  if (!connected.value) {
    ElMessage.warning(t('terminal.connectFirst'))
    return
  }
  if (uploading.value) {
    ElMessage.info(t('terminal.uploadBusy'))
    return
  }
  await uploadDroppedFiles(event)
}

async function connect() {
  if (connecting.value || connected.value) return
  if (!selectedProfileID.value) {
    ElMessage.warning(t('terminal.noProfiles'))
    return
  }
  const profile = remoteProfiles.value.find(item => item.id === selectedProfileID.value)
  connecting.value = true
  statusKind.value = 'connecting'
  resetStreamState()
  term?.reset()
  writeNotice(t('terminal.connectingProfile', { name: profile?.name || profile?.ssh_host || '' }))
  await nextTick()
  fitTerminal()
  try {
    const opened = await openShell(selectedProfileID.value)
    if (disposed) {
      await postJSON('/api/session/shell/close', { id: opened.id }).catch(() => undefined)
      return
    }
    sessionID = opened.id
    connected.value = true
    connecting.value = false
    statusKind.value = 'connected'
    lastCols = 0
    lastRows = 0
    void sendResize(true)
    void streamOutput(opened.id)
    term?.focus()
  } catch (error) {
    connecting.value = false
    connected.value = false
    if (error === 'cancel' || error === 'close') {
      statusKind.value = 'idle'
      writeNotice(t('terminal.hostKeyCancelled'))
      return
    }
    statusKind.value = 'error'
    writeNotice(`${t('terminal.connectFailed')}: ${errorMessage(error)}`)
  }
}

async function streamOutput(id: string) {
  outputAbort = new AbortController()
  let streamFailed = false
  try {
    const res = await fetch(`/api/session/shell/output?id=${encodeURIComponent(id)}`, {
      method: 'GET',
      headers: withLauncherToken(),
      cache: 'no-store',
      signal: outputAbort.signal
    })
    if (!res.ok || !res.body) {
      throw new Error(await responseError(res))
    }
    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      consumeTerminalOutput(decoder.decode(value, { stream: true }))
    }
  } catch (error) {
    if (!disposed && (error as Error)?.name !== 'AbortError') {
      streamFailed = true
      writeNotice(`${t('terminal.connectionLost')}: ${errorMessage(error)}`)
    }
  } finally {
    if (!disposed && connected.value && sessionID === id) {
      connected.value = false
      statusKind.value = 'idle'
      sessionID = ''
      pendingInput = ''
      dropActive.value = false
      resetStreamState()
      if (!streamFailed) writeNotice(t('terminal.sessionEnded'))
      releaseSftpIfIdle()
    }
  }
}

async function disconnect(quiet: boolean) {
  const id = sessionID
  sessionID = ''
  connected.value = false
  connecting.value = false
  statusKind.value = 'idle'
  pendingInput = ''
  dropActive.value = false
  resetStreamState()
  if (resizeTimer !== null) {
    window.clearTimeout(resizeTimer)
    resizeTimer = null
  }
  if (outputAbort) {
    outputAbort.abort()
    outputAbort = null
  }
  if (id) {
    await postJSON('/api/session/shell/close', { id }).catch(() => undefined)
  }
  releaseSftpIfIdle()
  if (!quiet) writeNotice(t('terminal.disconnected'))
}

function clearTerminal() {
  term?.clear()
}

async function flushInput() {
  if (inputFlushing) return
  inputFlushing = true
  try {
    while (pendingInput && sessionID) {
      const chunk = pendingInput
      const id = sessionID
      pendingInput = ''
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
    inputFlushing = false
  }
}

async function sendResize(force = false) {
  if (!connected.value || !sessionID) return
  const { cols, rows } = terminalSize()
  if (!force && cols === lastCols && rows === lastRows) return
  lastCols = cols
  lastRows = rows
  await postJSON('/api/session/shell/resize', { id: sessionID, cols, rows }).catch(() => undefined)
}

function scheduleResize() {
  if (resizeTimer !== null) window.clearTimeout(resizeTimer)
  resizeTimer = window.setTimeout(() => {
    resizeTimer = null
    void sendResize()
  }, 150)
}

watch(selectedProfileID, value => {
  try {
    localStorage.setItem(TERMINAL_PROFILE_KEY, value)
  } catch {
    // Selection persistence is best-effort.
  }
})

watch(() => props.active, async active => {
  if (!active) return
  await loadProfiles()
  await nextTick()
  fitTerminal()
  term?.focus()
})

onMounted(async () => {
  const initialProfile = new URLSearchParams(window.location.search).get('profile')
  if (initialProfile) selectedProfileID.value = initialProfile
  term = new Terminal({
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
  term.loadAddon(fitAddon)
  if (terminalHost.value) term.open(terminalHost.value)
  term.onData(data => {
    if (!connected.value || !sessionID) return
    pendingInput += data
    void flushInput()
  })
  term.onResize(() => scheduleResize())
  writeNotice(t('terminal.welcome'))
  await loadProfiles()
  await nextTick()
  fitTerminal()
  if (terminalHost.value) {
    resizeObserver = new ResizeObserver(() => fitTerminal())
    resizeObserver.observe(terminalHost.value)
  }
  if (props.active) term.focus()
})

onBeforeUnmount(() => {
  disposed = true
  resizeObserver?.disconnect()
  resizeObserver = null
  void disconnect(true)
  void disconnectSftp()
  term?.dispose()
  term = null
})
</script>
