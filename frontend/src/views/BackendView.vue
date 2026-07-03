<template>
  <section class="backend-view">
    <div class="backend-grid">
      <section class="settings-section">
        <div class="panel-header">
          <div>
            <strong>{{ t('backend.title') }}</strong>
            <p>{{ t('backend.runningState') }}</p>
          </div>
          <div class="header-actions">
            <el-tooltip :content="t('backend.refresh')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
              <el-button :icon="Refresh" circle @click="refreshAll" />
            </el-tooltip>
          </div>
        </div>
        <div class="backend-info">
          <div class="backend-row">
            <span>{{ t('backend.configDir') }}</span>
            <code :title="info.config_dir || '-'">{{ info.config_dir || '-' }}</code>
            <el-tooltip :content="t('backend.copyPath')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
              <el-button :icon="CopyDocument" circle @click="copyPath(info.config_dir)" />
            </el-tooltip>
            <el-tooltip :content="t('backend.openDir')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
              <el-button :icon="FolderOpened" circle @click="openConfigDir" />
            </el-tooltip>
          </div>
          <div class="backend-row">
            <span>{{ t('backend.fileCacheDir') }}</span>
            <code :title="info.sftp_open_cache_dir || '-'">{{ info.sftp_open_cache_dir || '-' }}</code>
            <el-tooltip :content="t('backend.copyPath')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
              <el-button :icon="CopyDocument" circle @click="copyPath(info.sftp_open_cache_dir)" />
            </el-tooltip>
            <el-tooltip :content="t('backend.openDir')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
              <el-button :icon="FolderOpened" circle @click="openSftpCacheDir" />
            </el-tooltip>
          </div>
          <div class="backend-row">
            <span>{{ t('backend.clearPending') }}</span>
            <el-tag :type="info.clear_pending ? 'warning' : 'success'">{{ info.clear_pending ? t('backend.statusContinue') : t('backend.normal') }}</el-tag>
          </div>
        </div>
        <div class="cache-list">
          <div v-for="entry in info.cache_entries || []" :key="entry.path" class="cache-entry">
            <div class="cache-entry-head">
              <strong>{{ cacheEntryLabel(entry.kind) }}</strong>
              <el-tag size="small" :type="entry.exists ? 'success' : 'info'">{{ entry.exists ? t('backend.saved') : t('backend.notFound') }}</el-tag>
            </div>
            <button class="path-button" type="button" @click="copyPath(entry.path)">{{ entry.path || '-' }}</button>
          </div>
          <p v-if="!(info.cache_entries || []).length" class="empty">{{ t('backend.noCacheEntries') }}</p>
        </div>
        <div class="editor-actions backend-actions">
          <div class="action-group">
            <el-button :icon="Download" @click="exportProfiles">{{ t('backend.export') }}</el-button>
            <el-button :icon="Upload" @click="pickImport">{{ t('backend.import') }}</el-button>
          </div>
          <div class="action-group danger">
            <el-button :icon="Delete" type="danger" @click="clearCache">{{ t('backend.clearCache') }}</el-button>
          </div>
          <input ref="importInput" class="hidden-input" type="file" accept="application/json,.json" @change="importProfiles" />
        </div>
        <el-alert v-if="notice" class="backend-notice" :title="notice" :type="noticeType" show-icon :closable="false" />
      </section>

      <section class="log-panel backend-log">
        <div class="logs-title">
          <h2>{{ t('backend.logTitle') }}</h2>
          <el-tooltip :content="t('backend.refreshLogs')" placement="bottom" popper-class="chemssh-passive-tooltip" :enterable="false" :show-after="500">
            <el-button :icon="Refresh" circle @click="refreshLogs" />
          </el-tooltip>
        </div>
        <pre ref="logRef" @scroll="logScroller.onScroll">{{ logs.join('\n') }}</pre>
      </section>
    </div>
  </section>
</template>

<script setup lang="ts">
import { nextTick, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { CopyDocument, Delete, Download, FolderOpened, Refresh, Upload } from '@element-plus/icons-vue'
import { api, postJSON } from '../api'
import { createAutoScroll } from '../autoScroll'
import { t } from '../i18n'

interface CacheEntry {
  kind: string
  path: string
  exists: boolean
}

interface BackendInfo {
  config_dir: string
  sftp_open_cache_dir: string
  clear_pending: boolean
  cache_entries: CacheEntry[]
}

const info = reactive<BackendInfo>({ config_dir: '', sftp_open_cache_dir: '', clear_pending: false, cache_entries: [] })
const logs = ref<string[]>([])
const logRef = ref<HTMLElement | null>(null)
const logScroller = createAutoScroll(() => logRef.value)
const notice = ref('')
const noticeType = ref<'success' | 'info' | 'warning' | 'error'>('info')
const importInput = ref<HTMLInputElement | null>(null)

function setNotice(message: string, type: typeof noticeType.value = 'info') {
  notice.value = message
  noticeType.value = type
}

async function refreshBackendInfo() {
  Object.assign(info, await api<BackendInfo>('/api/backend'))
}

async function refreshLogs() {
  const data = await api<{ lines: string[] }>('/api/launcher-logs')
  logs.value = data.lines || []
  await nextTick()
  logScroller.scrollToBottom()
}

async function refreshAll() {
  await Promise.all([refreshBackendInfo(), refreshLogs()])
}

async function openConfigDir() {
  await postJSON('/api/backend/open-config-dir', {})
  setNotice(t('backend.openedConfigDir'), 'success')
}

async function openSftpCacheDir() {
  await postJSON('/api/backend/open-sftp-cache-dir', {})
  setNotice(t('backend.openedSftpCacheDir'), 'success')
}

async function copyPath(path: string) {
  if (!path || !navigator.clipboard) {
    setNotice(t('backend.clipboardUnsupported'), 'error')
    return
  }
  await navigator.clipboard.writeText(path)
  setNotice(t('backend.pathCopied'), 'success')
}

function cacheEntryLabel(kind: string) {
  if (kind === 'sftp-open') return t('backend.cacheEntry.sftpOpen')
  if (kind === 'managed') return t('backend.cacheEntry.currentWebview')
  return t('backend.cacheEntry.legacyWebview')
}

async function clearCache() {
  await ElMessageBox.confirm(t('backend.clearCacheConfirm'), t('backend.clearCache'), { type: 'warning' })
  const result = await postJSON<{ info: BackendInfo; messages: string[] }>('/api/backend/clear-cache', {})
  Object.assign(info, result.info || {})
  setNotice(t('backend.clearCacheDone'), 'success')
  await refreshLogs()
}

async function exportProfiles() {
  const res = await fetch('/api/backend/export')
  if (!res.ok) {
    const data = await res.json()
    throw new Error(data.error || '导出失败')
  }
  const blob = await res.blob()
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = 'chemssh-launcher-profiles.json'
  document.body.appendChild(link)
  link.click()
  link.remove()
  URL.revokeObjectURL(url)
  setNotice(t('backend.configExported'), 'success')
}

function pickImport() {
  importInput.value?.click()
}

async function importProfiles(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  const text = await file.text()
  await api('/api/backend/import', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: text
  })
  setNotice(t('backend.configImported'), 'success')
  ElMessage.success(t('backend.configImported'))
  await refreshAll()
}

onMounted(async () => {
  await refreshAll()
  window.setInterval(refreshAll, 2500)
})
</script>
