<template>
  <div v-if="jobs.length" class="native-download-list" :aria-label="t('download.title')">
    <div v-for="job in jobs" :key="job.id" class="native-download-row">
      <span class="native-download-name" :title="`${job.sourceName}: ${job.remotePath} → ${job.localPath}`">
        {{ job.localPath.split(/[/\\]/).pop() }}
      </span>
      <span class="native-download-state" :title="job.error || undefined">
        {{ t(`download.${job.state}`) }}<template v-if="job.error && job.state === 'failed'">: {{ job.error }}</template>
      </span>
      <progress v-if="downloadActive(job)" :value="job.total ? job.loaded : undefined" :max="job.total || 1" :aria-label="t('sftp.progress')" />
      <span v-if="downloadActive(job)" class="native-download-bytes">
        {{ formatBytes(job.loaded) || '0 B' }} / {{ job.total ? formatBytes(job.total) : t('sftp.unknownSize') }}
        <template v-if="speed(job)"> · {{ speed(job) }}/s</template>
      </span>
      <el-button v-if="downloadActive(job)" size="small" :disabled="!canCancelDownload(job)" @click="cancel(job.id)">
        {{ t('sftp.cancel') }}
      </el-button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ElMessage } from 'element-plus'
import { formatBytes } from './api'
import { t } from './i18n'
import { canCancelDownload, downloadActive, nativeDownloads, type NativeDownloadJob } from './nativeDownloads'

defineProps<{ jobs: readonly NativeDownloadJob[] }>()

function speed(job: NativeDownloadJob) {
  if (!job.startedAt || !job.loaded || job.state !== 'running') return ''
  return formatBytes(job.loaded / Math.max(1, (Date.now() - job.startedAt) / 1000))
}

function cancel(id: string) {
  void nativeDownloads.cancel(id).catch(error => ElMessage.error(error instanceof Error ? error.message : String(error)))
}
</script>

<style scoped>
.native-download-list {
  max-height: 10em;
  overflow: auto;
}
.native-download-row {
  display: flex;
  align-items: center;
  gap: 8px;
  min-height: 28px;
  padding: 2px 8px;
}
.native-download-name, .native-download-state {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.native-download-name { min-width: 4em; }
.native-download-row progress { width: min(8em, 20%); }
.native-download-bytes { flex-shrink: 0; }
</style>
