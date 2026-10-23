import { ElMessageBox } from 'element-plus'
import { APIError, type HostKeyInfo } from './api'
import { t } from './i18n'

export interface HostKeyTrustPayload {
  accept_host_key?: boolean
  replace_host_key?: boolean
}

export function hostKeyFromError(error: unknown) {
  if (!(error instanceof APIError)) return null
  const info = error.data?.host_key
  if (!info || typeof info !== 'object') return null
  return info as HostKeyInfo
}

export async function confirmHostKey(info: HostKeyInfo): Promise<HostKeyTrustPayload> {
  if (!info.mismatch) {
    await ElMessageBox.confirm(
      t('launcher.hostKeyConfirm', {
        address: info.address,
        keyType: info.key_type,
        fingerprint: info.fingerprint,
        path: info.known_hosts_path
      }),
      t('launcher.hostKeyTitle'),
      { type: 'warning', customClass: 'host-key-confirm-dialog' }
    )
    return { accept_host_key: true }
  }

  const previous = (info.previous_fingerprints || []).join('\n') || t('launcher.hostKeyPreviousUnknown')
  await ElMessageBox.confirm(
    t('launcher.hostKeyMismatchConfirm', {
      address: info.address,
      keyType: info.key_type,
      fingerprint: info.fingerprint,
      previous,
      path: info.known_hosts_path
    }),
    t('launcher.hostKeyMismatchTitle'),
    {
      type: 'error',
      confirmButtonText: t('launcher.hostKeyMismatchAction'),
      cancelButtonText: t('launcher.cancel'),
      confirmButtonClass: 'el-button--danger',
      customClass: 'host-key-mismatch-dialog',
      closeOnClickModal: false,
      closeOnPressEscape: false
    }
  )
  return { replace_host_key: true }
}
