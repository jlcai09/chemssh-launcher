// Tauri v2 IPC adapter bridging the existing Vue frontend calls to Rust
// `#[tauri::command]` handlers and `tauri::ipc::Channel` streams.

import { Channel, invoke, isTauri } from '@tauri-apps/api/core'
import { listen } from '@tauri-apps/api/event'
import type { TransferProgressSnapshot } from './transferProgress'
import type { NativeDragAction, NativeDragJob } from './nativeDrag'

export interface TauriTerminalEvent {
  kind: 'output' | 'flow_control' | 'exit'
  seq?: number
  data_b64?: string
  paused?: boolean
  exit_code?: number | null
}

export interface ActiveWorkSummary {
  active_transfers: number
  active_drag_jobs: number
}

export function isTauriRuntime(): boolean {
  try {
    return typeof window !== 'undefined' && isTauri()
  } catch {
    return false
  }
}

function parseJsonBody(body: BodyInit | null | undefined): Record<string, any> {
  if (!body) return {}
  if (typeof body === 'string') {
    try {
      return JSON.parse(body)
    } catch {
      return {}
    }
  }
  return {}
}

function parseUrl(rawPath: string): { pathname: string; params: URLSearchParams } {
  const url = new URL(rawPath, 'http://tauri.local')
  return { pathname: url.pathname, params: url.searchParams }
}

/**
 * Pure route mapper exported for unit testing and used by `invokeApi`.
 */
export function resolveTauriCommand(
  rawPath: string,
  method = 'GET',
  body: Record<string, any> = {},
): { command: string; args?: Record<string, any> } | null {
  const { pathname, params } = parseUrl(rawPath)
  const verb = method.toUpperCase()

  // Profiles & defaults
  if (pathname === '/api/profiles' && verb === 'GET') {
    return { command: 'list_profiles' }
  }
  if (pathname === '/api/profiles' && (verb === 'POST' || verb === 'PUT')) {
    return { command: 'save_profile', args: { request: body } }
  }
  if (pathname === '/api/profiles/reorder' && verb === 'POST') {
    return { command: 'reorder_profiles', args: { ids: body.ids ?? [] } }
  }
  if (pathname === '/api/profiles/duplicate' && verb === 'POST') {
    return { command: 'duplicate_profile', args: { idOrName: body.id ?? body.name ?? '' } }
  }
  if (pathname.startsWith('/api/profiles/')) {
    const idOrName = decodeURIComponent(pathname.slice('/api/profiles/'.length))
    if (verb === 'GET') return { command: 'get_profile', args: { idOrName } }
    if (verb === 'DELETE') return { command: 'delete_profile', args: { idOrName } }
    if (verb === 'PUT' || verb === 'POST') {
      return { command: 'save_profile', args: { request: body } }
    }
  }
  if (pathname === '/api/profile-test' && verb === 'POST') {
    return {
      command: 'test_profile_connection',
      args: {
        profileId: body.profile_id ?? body.id ?? '',
        hostKeyPolicy: body.host_key_policy ?? null,
      },
    }
  }
  if (pathname === '/api/defaults' && verb === 'GET') {
    return { command: 'profile_defaults' }
  }
  if (pathname === '/api/version' && verb === 'GET') {
    return { command: 'get_version' }
  }

  // Backend & UI settings
  if (pathname === '/api/ui-settings' && verb === 'GET') {
    return { command: 'get_ui_settings' }
  }
  if (pathname === '/api/ui-settings' && (verb === 'PUT' || verb === 'POST')) {
    return { command: 'save_ui_settings', args: { settings: body } }
  }
  if (pathname === '/api/backend' && verb === 'GET') {
    return { command: 'get_backend_info' }
  }
  if (pathname === '/api/backend/export' && verb === 'GET') {
    return { command: 'export_profiles' }
  }
  if (pathname === '/api/backend/import' && verb === 'POST') {
    return {
      command: 'import_profiles',
      args: { rawJson: typeof body.raw_json === 'string' ? body.raw_json : JSON.stringify(body) },
    }
  }

  // Sessions & logs
  if (pathname === '/api/session/start' && verb === 'POST') {
    return {
      command: 'start_session',
      args: {
        profileId: body.profile_id ?? body.id ?? '',
        hostKeyPolicy: body.host_key_policy ?? null,
      },
    }
  }
  if (pathname === '/api/session/stop' && verb === 'POST') {
    return { command: 'stop_session', args: { profileId: body.profile_id ?? body.id ?? '' } }
  }
  if (pathname === '/api/session/stop-service' && verb === 'POST') {
    return { command: 'pause_forwarding', args: { profileId: body.profile_id ?? body.id ?? '' } }
  }
  if (pathname === '/api/session/status' && verb === 'GET') {
    return { command: 'get_session_status', args: { profileId: params.get('profile_id') ?? '' } }
  }
  if (pathname === '/api/session/statuses' && verb === 'GET') {
    return { command: 'list_session_statuses' }
  }
  if (pathname === '/api/logs' && verb === 'GET') {
    return { command: 'get_session_logs', args: { profileId: params.get('profile_id') ?? '' } }
  }

  // Terminal control
  if (pathname === '/api/session/shell/resize' && verb === 'POST') {
    return {
      command: 'resize_terminal',
      args: { sessionId: body.session ?? body.id ?? '', cols: body.cols ?? 120, rows: body.rows ?? 32 },
    }
  }
  if (pathname === '/api/session/shell/close' && verb === 'POST') {
    return { command: 'close_terminal', args: { sessionId: body.session ?? body.id ?? '' } }
  }

  // Transfers
  if (pathname === '/api/transfer-progress' && verb === 'GET') {
    const id = params.get('id')
    return id ? { command: 'get_transfer_progress', args: { id } } : { command: 'list_transfers' }
  }
  if (pathname === '/api/transfer-count' && verb === 'GET') {
    return { command: 'get_transfer_count' }
  }
  if (pathname === '/api/transfer-control' && verb === 'POST') {
    return { command: 'control_transfer', args: { id: body.id ?? '', action: body.action ?? '' } }
  }

  // Local FS
  if (pathname === '/api/local/home' && verb === 'GET') {
    return { command: 'get_local_home' }
  }
  if (pathname === '/api/local/list' && verb === 'GET') {
    return { command: 'local_list', args: { path: params.get('path') ?? '.' } }
  }
  if (pathname === '/api/local/mkdir' && verb === 'POST') {
    return { command: 'local_mkdir_cmd', args: { path: body.path ?? '' } }
  }
  if (pathname === '/api/local/create-file' && verb === 'POST') {
    return { command: 'local_create_file_cmd', args: { path: body.path ?? '' } }
  }
  if (pathname === '/api/local/rename' && verb === 'POST') {
    return { command: 'local_rename_cmd', args: { from: body.from ?? '', to: body.to ?? '' } }
  }
  if (pathname === '/api/local/delete' && verb === 'POST') {
    return { command: 'local_delete_cmd', args: { path: body.path ?? '' } }
  }
  if (pathname === '/api/local/copy' && verb === 'POST') {
    return { command: 'local_copy_cmd', args: { src: body.src ?? '', dst: body.dst ?? '' } }
  }
  if (pathname === '/api/local/archive-tools' && verb === 'GET') {
    return { command: 'local_archive_tools' }
  }
  if (pathname === '/api/local/compress' && verb === 'POST') {
    return {
      command: 'local_compress',
      args: {
        cwd: body.cwd ?? '.',
        format: body.format ?? 'tar.gz',
        archiveName: body.archive_name ?? body.name ?? 'archive.tar.gz',
        entries: body.entries ?? body.paths ?? [],
      },
    }
  }
  if (pathname === '/api/local/extract' && verb === 'POST') {
    return {
      command: 'local_extract',
      args: {
        cwd: body.cwd ?? '.',
        archiveName: body.archive_name ?? body.path ?? '',
        destDir: body.dest_dir ?? '.',
      },
    }
  }

  // SFTP
  if (pathname === '/api/sftp/connect' && verb === 'POST') {
    return {
      command: 'sftp_connect',
      args: {
        profileId: body.profile_id ?? body.id ?? '',
        hostKeyPolicy: body.host_key_policy ?? null,
      },
    }
  }
  if (pathname === '/api/sftp/disconnect' && verb === 'POST') {
    return { command: 'sftp_disconnect', args: { profileId: body.profile_id ?? body.id ?? '' } }
  }
  if (pathname === '/api/sftp/list' && verb === 'GET') {
    return {
      command: 'sftp_list',
      args: { profileId: params.get('profile_id') ?? '', path: params.get('path') ?? '.' },
    }
  }
  if (pathname === '/api/sftp/realpath' && verb === 'GET') {
    return {
      command: 'sftp_realpath',
      args: { profileId: params.get('profile_id') ?? '', path: params.get('path') ?? '.' },
    }
  }
  if (pathname === '/api/sftp/mkdir' && verb === 'POST') {
    return { command: 'sftp_mkdir', args: { profileId: body.profile_id ?? '', path: body.path ?? '' } }
  }
  if (pathname === '/api/sftp/create-file' && verb === 'POST') {
    return {
      command: 'sftp_create_file',
      args: { profileId: body.profile_id ?? '', path: body.path ?? '' },
    }
  }
  if (pathname === '/api/sftp/rename' && verb === 'POST') {
    return {
      command: 'sftp_rename',
      args: { profileId: body.profile_id ?? '', from: body.from ?? '', to: body.to ?? '' },
    }
  }
  if (pathname === '/api/sftp/delete' && verb === 'POST') {
    return {
      command: 'sftp_delete',
      args: {
        profileId: body.profile_id ?? '',
        path: body.path ?? '',
        isDir: Boolean(body.is_dir),
      },
    }
  }
  if (pathname === '/api/sftp/upload-local' && verb === 'POST') {
    return {
      command: 'sftp_upload_local',
      args: {
        profileId: body.profile_id ?? '',
        localPath: body.local_path ?? '',
        remotePath: body.remote_path ?? '',
        transferId: body.transfer_id ?? '',
        resume: Boolean(body.resume),
      },
    }
  }
  if (pathname === '/api/sftp/download-local' && verb === 'POST') {
    return {
      command: 'sftp_download_local',
      args: {
        profileId: body.profile_id ?? '',
        remotePath: body.remote_path ?? '',
        localPath: body.local_path ?? '',
        transferId: body.transfer_id ?? '',
        resume: Boolean(body.resume),
      },
    }
  }
  if (pathname === '/api/sftp/copy-remote' && verb === 'POST') {
    return {
      command: 'sftp_copy_remote',
      args: {
        srcProfileId: body.src_profile_id ?? '',
        srcPath: body.src_path ?? '',
        dstProfileId: body.dst_profile_id ?? '',
        dstPath: body.dst_path ?? '',
        transferId: body.transfer_id ?? '',
      },
    }
  }
  if (pathname === '/api/sftp/archive-tools' && verb === 'GET') {
    return { command: 'sftp_archive_tools', args: { profileId: params.get('profile_id') ?? '' } }
  }
  if (pathname === '/api/sftp/compress' && verb === 'POST') {
    return {
      command: 'sftp_compress',
      args: {
        profileId: body.profile_id ?? '',
        cwd: body.cwd ?? '.',
        format: body.format ?? 'tar.gz',
        archiveName: body.archive_name ?? 'archive.tar.gz',
        entries: body.entries ?? [],
      },
    }
  }
  if (pathname === '/api/sftp/extract' && verb === 'POST') {
    return {
      command: 'sftp_extract',
      args: {
        profileId: body.profile_id ?? '',
        cwd: body.cwd ?? '.',
        archiveName: body.archive_name ?? '',
        destDir: body.dest_dir ?? '.',
      },
    }
  }

  return null
}

export async function invokeApi<T>(path: string, options: RequestInit = {}): Promise<T> {
  const body = parseJsonBody(options.body)
  const mapped = resolveTauriCommand(path, options.method, body)
  if (!mapped) {
    throw new Error(`Unmapped Tauri API route: ${options.method || 'GET'} ${path}`)
  }
  return invoke<T>(mapped.command, mapped.args)
}

export async function openTauriTerminalChannel(
  profileId: string,
  onEvent: (event: TauriTerminalEvent) => void,
  cols = 120,
  rows = 32,
  hostKeyPolicy?: string,
): Promise<string> {
  const channel = new Channel<TauriTerminalEvent>()
  channel.onmessage = onEvent
  return invoke<string>('open_terminal', {
    profileId,
    cols,
    rows,
    hostKeyPolicy: hostKeyPolicy ?? null,
    onEvent: channel,
  })
}

export function sendTauriTerminalInput(sessionId: string, dataB64: string) {
  return invoke<{ accepted: boolean; flow_paused: boolean }>('send_terminal_input', {
    sessionId,
    dataB64,
  })
}

export type TauriTransferProgressItem = TransferProgressSnapshot & { id: string }

export async function subscribeTauriTransferProgress(
  onSnapshot: (items: TauriTransferProgressItem[]) => void,
): Promise<void> {
  const channel = new Channel<TauriTransferProgressItem[]>()
  channel.onmessage = onSnapshot
  await invoke('stream_transfer_progress', { onEvent: channel })
}

export async function listenCloseRequested(
  handler: (summary: ActiveWorkSummary) => void,
): Promise<() => void> {
  return listen<ActiveWorkSummary>('app://close-requested', event => {
    handler(event.payload)
  })
}

/**
 * Installs window-level native drag bindings backed by Tauri v2 commands so
 * `nativeDrag.ts` and existing Vue views work without changing their call sites.
 */
export function installTauriNativeDragBridge(): void {
  if (!isTauriRuntime()) return
  window.chemsshNativeDragJobs = () => invoke<NativeDragJob[]>('list_native_drag_jobs')
  window.chemsshControlNativeDragJob = async (id: string, action: NativeDragAction) => {
    try {
      await invoke('control_native_drag_job', { id, action })
      return true
    } catch {
      return false
    }
  }
  window.chemsshCancelNativeDragJob = (id: string) =>
    window.chemsshControlNativeDragJob!(id, 'cancel')
  window.chemsshDragOutFiles = async (payloadJson: string) => {
    const parsed = JSON.parse(payloadJson) as {
      id: string
      session: string
      paths: string[]
      staged_local_paths?: string[]
    }
    const channel = new Channel<NativeDragJob>()
    channel.onmessage = job => {
      window.dispatchEvent(new CustomEvent('chemssh-native-drag-job', { detail: job }))
    }
    try {
      const outcome = await invoke<string>('start_native_drag_out', {
        request: {
          id: parsed.id,
          session: parsed.session,
          paths: parsed.paths,
          staged_local_paths: parsed.staged_local_paths ?? [],
        },
        onEvent: channel,
      })
      window.dispatchEvent(
        new CustomEvent('chemssh-native-drag-finished', {
          detail: { id: parsed.id, status: outcome, ok: outcome === 'dropped' },
        }),
      )
    } catch (err) {
      window.dispatchEvent(
        new CustomEvent('chemssh-native-drag-finished', {
          detail: { id: parsed.id, status: String(err), ok: false },
        }),
      )
    }
  }
}
