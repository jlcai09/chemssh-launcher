<template>
  <div class="cwait" role="status" aria-live="polite">
    <div v-if="!failed" class="cwait-body">
      <span class="cwait-spinner" aria-hidden="true"></span>
      <div class="cwait-text">
        <strong>{{ name || 'ChemSSH' }}</strong>
        <span>{{ t('wait.starting') }}</span>
      </div>
    </div>
    <div v-else class="cwait-body cwait-failed">
      <div class="cwait-text">
        <strong>{{ t('wait.failedTitle') }}</strong>
        <span class="cwait-error">{{ errorText }}</span>
        <div class="cwait-actions">
          <button type="button" @click="retry">{{ t('wait.retry') }}</button>
          <button type="button" class="ghost" @click="backToLauncher">{{ t('wait.backToLauncher') }}</button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { api, postJSON } from '../api'
import { t } from '../i18n'

interface WaitStatus {
  phase?: string
  running?: boolean
  last_error?: string | null
}

const query = new URLSearchParams(window.location.search)
const profileID = query.get('profile_id') || ''
const name = ref(query.get('name') || '')
const target = query.get('target') || ''

const failed = ref(false)
const errorText = ref('')
let timer: number | undefined
let disposed = false
let seen = false
let misses = 0

function handoff() {
  if (!target) return
  if (window.parent !== window) {
    window.parent.postMessage({ type: 'chemssh-launcher:wait-ready', target, name: name.value }, window.location.origin)
  } else {
    window.location.replace(target)
  }
}

function fail(message: string) {
  failed.value = true
  errorText.value = message || t('wait.failedUnknown')
}

async function poll() {
  if (disposed) return
  try {
    const status = await api<WaitStatus | null>(`/api/session/status?profile_id=${encodeURIComponent(profileID)}`)
    if (disposed) return
    if (!status) {
      misses += 1
      if (seen || misses > 10) return fail('')
    } else {
      seen = true
      if (status.phase === 'ready') return handoff()
      if (status.phase === 'failed' || status.phase === 'stopped' || status.running === false) {
        return fail(status.last_error || '')
      }
    }
  } catch {
    // Keep waiting while the backend is busy.
  }
  timer = window.setTimeout(poll, 600)
}

async function retry() {
  failed.value = false
  seen = false
  misses = 0
  try {
    await postJSON('/api/session/start', { id: profileID })
  } catch (error) {
    return fail((error as Error).message)
  }
  window.parent?.postMessage({ type: 'chemssh-launcher:session-updated' }, window.location.origin)
  void poll()
}

function backToLauncher() {
  window.parent?.postMessage({ type: 'chemssh-launcher:focus-launcher' }, window.location.origin)
}

onMounted(() => {
  void poll()
})

onBeforeUnmount(() => {
  disposed = true
  if (timer !== undefined) window.clearTimeout(timer)
})
</script>

<style scoped>
.cwait {
  position: fixed;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  background: #f6f6f7;
  color: #1d1d1f;
  font-family: -apple-system, BlinkMacSystemFont, 'SF Pro Text', 'Segoe UI', 'PingFang SC', 'Microsoft YaHei', system-ui, sans-serif;
  user-select: none;
}

.cwait-body {
  display: inline-flex;
  align-items: center;
  gap: 12px;
  max-width: min(520px, 86vw);
}

.cwait-spinner {
  flex: none;
  width: 16px;
  height: 16px;
  border-radius: 50%;
  border: 1.5px solid rgba(29, 29, 31, 0.12);
  border-top-color: #6e6e73;
  animation: cwait-spin 0.75s linear infinite;
}

.cwait-text {
  display: flex;
  flex-direction: column;
  gap: 3px;
  font-size: 12.5px;
  color: #86868b;
}

.cwait-text strong {
  font-size: 13px;
  font-weight: 600;
  color: #1d1d1f;
}

.cwait-failed {
  align-items: flex-start;
}

.cwait-error {
  white-space: pre-wrap;
  word-break: break-word;
  user-select: text;
}

.cwait-actions {
  display: flex;
  gap: 8px;
  margin-top: 10px;
}

.cwait-actions button {
  height: 28px;
  padding: 0 12px;
  border: 1px solid transparent;
  border-radius: 6px;
  background: #1d1d1f;
  color: #fff;
  font: inherit;
  font-size: 12.5px;
  cursor: pointer;
}

.cwait-actions button.ghost {
  background: transparent;
  border-color: rgba(29, 29, 31, 0.18);
  color: #1d1d1f;
}

@keyframes cwait-spin {
  to {
    transform: rotate(360deg);
  }
}

@media (prefers-reduced-motion: reduce) {
  .cwait-spinner {
    animation: none;
    border-color: rgba(29, 29, 31, 0.28);
  }
}
</style>
