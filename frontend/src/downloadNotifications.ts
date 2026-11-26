import { ElMessage } from 'element-plus'
import { t } from './i18n'
import type { DownloadOutcome } from './nativeDownloads'
import type { TauriDownloadTarget } from './tauriBridge'

export function notifyDownloadBatch(
  { count, done }: { count: number; done: Promise<DownloadOutcome[]> },
  targets: readonly TauriDownloadTarget[],
) {
  if (!count) return
  ElMessage.info(t('download.queuedCount', { count }))
  const renamed = targets.filter(target => target.renamed).length
  if (renamed) ElMessage.info(t('download.renamedCount', { count: renamed }))
  void done.then(outcomes => {
    const counts = { saved: 0, cancelled: 0, failed: 0 }
    for (const outcome of outcomes) counts[outcome]++
    const message = t('download.summary', counts)
    if (counts.failed) ElMessage.warning(message)
    else if (counts.saved) ElMessage.success(message)
    else ElMessage.info(message)
  })
}
