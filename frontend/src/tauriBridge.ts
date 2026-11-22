// Tauri v2 IPC adapter bridging the existing Vue frontend calls to Rust
// `#[tauri::command]` handlers and `tauri::ipc::Channel` streams.

import { Channel, invoke, isTauri } from '@tauri-apps/api/core'
import { listen } from '@tauri-apps/api/event'
import type { TransferProgressSnapshot } from './transferProgress'
import type { NativeDragAction, NativeDragJob } from './nativeDrag'
import type { TerminalSink } from './appEvents'

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
    if (typeof window === 'undefined') return false
    if ('__TAURI_INTERNALS__' in window || isTauri()) return true
    if (window.parent && window.parent !== window && '__TAURI_INTERNALS__' in window.parent) {
      ;(window as any).__TAURI_INTERNALS__ = (window.parent as any).__TAURI_INTERNALS__
      ;(window as any).isTauri = true
      if ('__TAURI_EVENT_PLUGIN_INTERNALS__' in window.parent) {
        ;(window as any).__TAURI_EVENT_PLUGIN_INTERNALS__ = (
          window.parent as any
        ).__TAURI_EVENT_PLUGIN_INTERNALS__
      }
      return true
    }
    return false
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

function resolveHostKeyPolicy(body: Record<string, any>): string | null {
  if (body.host_key_policy) return String(body.host_key_policy)
  if (body.replace_host_key) return 'replace_changed'
  if (body.accept_host_key) return 'accept_new'
  return null
}

function fileBaseName(p: string): string {
  const parts = String(p || '').replace(/\\/g, '/').split('/').filter(Boolean)
  return parts.at(-1) || String(p || '')
}

function parentDir(p: string, kind: 'local' | 'remote'): string {
  const normalized = String(p || '.').replace(/\\/g, '/')
  const parts = normalized.split('/').filter(Boolean)
  parts.pop()
  if (kind === 'local' && /^[A-Za-z]:/.test(normalized)) {
    return parts.length <= 1 ? `${parts[0]}/` : parts.join('\\')
  }
  if (normalized.startsWith('/')) return parts.length ? `/${parts.join('/')}` : '/'
  return parts.length ? parts.join('/') : '.'
}

function joinTargetPath(dir: string, rel: string, kind: 'local' | 'remote'): string {
  const cleanRel = String(rel || '').replace(/^[\\/]+/, '')
  if (!cleanRel) return dir || '.'
  if (!dir || dir === '.') return cleanRel
  const sep = kind === 'local' && (dir.includes('\\') || /^[A-Za-z]:/.test(dir)) ? '\\' : '/'
  const normalizedRel = sep === '\\' ? cleanRel.replace(/\//g, '\\') : cleanRel.replace(/\\/g, '/')
  if (dir.endsWith('/') || dir.endsWith('\\')) return `${dir}${normalizedRel}`
  return `${dir}${sep}${normalizedRel}`
}

function defaultArchiveName(entries: string[], format: string): string {
  const stem = entries.length === 1 && entries[0] ? fileBaseName(entries[0]) : 'archive'
  return `${stem}.${format}`
}

function computeArchiveFormats(tools: Record<string, any> | null | undefined): string[] {
  if (!tools) return []
  if (Array.isArray(tools.formats)) return tools.formats
  const formats: string[] = []
  if (tools.tar && tools.zstd) formats.push('tar.zst')
  if (tools.tar && tools.gzip) formats.push('tar.gz')
  if (tools.tar && tools.xz) formats.push('tar.xz')
  if (tools.tar) formats.push('tar')
  if (tools.zip) formats.push('zip')
  if (tools.seven_zip) formats.push('7z')
  return formats
}

function buildSaveProfileRequest(body: Record<string, any>, pathId?: string): Record<string, any> {
  if (!body.profile && !body.secrets) {
    return body
  }
  const profile = { ...(body.profile ?? {}) }
  if (pathId && !profile.id) {
    profile.id = pathId
  }
  const secrets = body.secrets ?? {}
  return {
    profile,
    password:
      secrets.password_action === 'replace' && secrets.password
        ? String(secrets.password)
        : (body.password ?? null),
    clear_password: secrets.password_action === 'clear' || Boolean(body.clear_password),
    private_key_passphrase:
      secrets.passphrase_action === 'replace' && secrets.passphrase
        ? String(secrets.passphrase)
        : (body.private_key_passphrase ?? null),
    clear_private_key_passphrase:
      secrets.passphrase_action === 'clear' || Boolean(body.clear_private_key_passphrase),
    security_token:
      secrets.security_token_action === 'replace' && secrets.security_token
        ? String(secrets.security_token)
        : (body.security_token ?? null),
    clear_security_key: false,
    clear_security_token:
      secrets.security_token_action === 'clear' || Boolean(body.clear_security_token),
  }
}

function decodeBase64Bytes(b64: string): Uint8Array {
  const binary = atob(b64)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i)
  }
  return bytes
}

interface TerminalStreamBuffer {
  sink: TerminalSink | null
  queued: TauriTerminalEvent[]
  exited: boolean
}

const terminalStreams = new Map<string, TerminalStreamBuffer>()

function dispatchTerminalEvent(buf: TerminalStreamBuffer, ev: TauriTerminalEvent) {
  if (!buf.sink) {
    buf.queued.push(ev)
    return
  }
  if (ev.kind === 'output' && ev.data_b64) {
    buf.sink.onData(decodeBase64Bytes(ev.data_b64))
  } else if (ev.kind === 'exit') {
    buf.exited = true
    buf.sink.onExit()
  }
}

export function attachTauriTerminalSink(sessionId: string, sink: TerminalSink): () => void {
  let buf = terminalStreams.get(sessionId)
  if (!buf) {
    buf = { sink: null, queued: [], exited: false }
    terminalStreams.set(sessionId, buf)
  }
  buf.sink = sink
  const pending = buf.queued.splice(0, buf.queued.length)
  for (const ev of pending) {
    dispatchTerminalEvent(buf, ev)
  }
  return () => {
    const current = terminalStreams.get(sessionId)
    if (current && current.sink === sink) {
      current.sink = null
    }
  }
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
    return { command: 'save_profile', args: { request: buildSaveProfileRequest(body) } }
  }
  if (pathname === '/api/profiles/reorder' && verb === 'POST') {
    return { command: 'reorder_profiles', args: { ids: body.ids ?? [] } }
  }
  if (pathname === '/api/profiles/duplicate' && verb === 'POST') {
    return {
      command: 'duplicate_profile',
      args: { idOrName: body.id ?? body.name ?? '', newName: body.name ?? null },
    }
  }
  if (pathname.startsWith('/api/profiles/')) {
    const idOrName = decodeURIComponent(pathname.slice('/api/profiles/'.length))
    if (verb === 'GET') return { command: 'get_profile', args: { idOrName } }
    if (verb === 'DELETE') return { command: 'delete_profile', args: { idOrName } }
    if (verb === 'PUT' || verb === 'POST') {
      return { command: 'save_profile', args: { request: buildSaveProfileRequest(body, idOrName) } }
    }
  }
  if (pathname === '/api/profile-test' && verb === 'POST') {
    return {
      command: 'test_profile_connection',
      args: {
        profileId: body.profile_id ?? body.id ?? '',
        hostKeyPolicy: resolveHostKeyPolicy(body),
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
  if (pathname === '/api/backend/open-config-dir' && verb === 'POST') {
    return { command: 'open_config_dir' }
  }
  if (pathname === '/api/backend/open-sftp-cache-dir' && verb === 'POST') {
    return { command: 'open_sftp_cache_dir' }
  }
  if (pathname === '/api/backend/clear-cache' && verb === 'POST') {
    return { command: 'clear_cache' }
  }

  // Sessions & logs
  if (pathname === '/api/session/start' && verb === 'POST') {
    return {
      command: 'start_session',
      args: {
        profileId: body.profile_id ?? body.id ?? '',
        hostKeyPolicy: resolveHostKeyPolicy(body),
      },
    }
  }
  if (pathname === '/api/session/stop' && verb === 'POST') {
    return { command: 'pause_forwarding', args: { profileId: body.profile_id ?? body.id ?? '' } }
  }
  if (pathname === '/api/session/stop-service' && verb === 'POST') {
    return { command: 'stop_session', args: { profileId: body.profile_id ?? body.id ?? '' } }
  }
  if (pathname === '/api/session/activate' && verb === 'POST') {
    return { command: 'activate_session', args: { profileId: body.profile_id ?? body.id ?? '' } }
  }
  if (pathname === '/api/session/status' && verb === 'GET') {
    return {
      command: 'get_session_status',
      args: { profileId: params.get('profile_id') || null },
    }
  }
  if (pathname === '/api/session/statuses' && verb === 'GET') {
    return { command: 'list_session_statuses' }
  }
  if (pathname === '/api/logs' && verb === 'GET') {
    return { command: 'get_session_logs', args: { profileId: params.get('profile_id') ?? '' } }
  }

  // Terminal control
  if (pathname === '/api/session/shell/open' && verb === 'POST') {
    return {
      command: 'open_terminal',
      args: {
        profileId: body.profile_id ?? body.id ?? '',
        cols: body.cols ?? 120,
        rows: body.rows ?? 32,
        hostKeyPolicy: resolveHostKeyPolicy(body),
      },
    }
  }
  if (pathname === '/api/session/shell/cwd' && verb === 'GET') {
    return {
      command: 'get_terminal_cwd',
      args: { sessionId: params.get('id') ?? params.get('session') ?? '' },
    }
  }
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
    return { command: 'local_list', args: { path: params.get('path') || '.' } }
  }
  if (pathname === '/api/local/mkdir' && verb === 'POST') {
    return { command: 'local_mkdir_cmd', args: { path: body.path ?? '' } }
  }
  if (pathname === '/api/local/create-file' && verb === 'POST') {
    return { command: 'local_create_file_cmd', args: { path: body.path ?? '' } }
  }
  if (pathname === '/api/local/rename' && verb === 'POST') {
    return {
      command: 'local_rename_cmd',
      args: { from: body.path ?? body.from ?? '', to: body.new_path ?? body.to ?? '' },
    }
  }
  if (pathname === '/api/local/delete' && verb === 'POST') {
    return { command: 'local_delete_cmd', args: { path: body.path ?? '' } }
  }
  if (pathname === '/api/local/copy' && verb === 'POST') {
    const src = body.src ?? body.source_path ?? ''
    const dst =
      body.dst ??
      joinTargetPath(
        body.target_path ?? '.',
        body.relative_path || fileBaseName(src),
        'local',
      )
    return {
      command: 'local_copy_cmd',
      args: { src, dst, transferId: body.transfer_id ?? null },
    }
  }
  if ((pathname === '/api/local/open' || pathname === '/api/local/open-text') && verb === 'POST') {
    return { command: 'local_open_cmd', args: { path: body.path ?? '' } }
  }
  if (pathname === '/api/local/archive-tools' && verb === 'GET') {
    return { command: 'local_archive_tools' }
  }
  if (pathname === '/api/local/compress' && verb === 'POST') {
    const cwd = body.cwd ?? body.target_directory ?? '.'
    const format = body.format ?? 'tar.gz'
    const entries: string[] =
      body.entries ?? (Array.isArray(body.paths) ? body.paths.map(fileBaseName) : [])
    const archiveName =
      body.archive_name ?? body.name ?? defaultArchiveName(entries, format)
    return {
      command: 'local_compress',
      args: { cwd, format, archiveName, entries },
    }
  }
  if (pathname === '/api/local/extract' && verb === 'POST') {
    const archiveName = body.archive_name ?? body.path ?? ''
    const cwd = body.cwd ?? body.target_directory ?? parentDir(archiveName, 'local')
    const destDir = body.dest_dir ?? body.target_directory ?? cwd
    return {
      command: 'local_extract',
      args: { cwd, archiveName, destDir },
    }
  }

  // SFTP
  if (pathname === '/api/sftp/connect' && verb === 'POST') {
    return {
      command: 'sftp_connect',
      args: {
        profileId: body.profile_id ?? body.id ?? '',
        hostKeyPolicy: resolveHostKeyPolicy(body),
      },
    }
  }
  if (pathname === '/api/sftp/reconnect' && verb === 'POST') {
    return {
      command: 'sftp_connect',
      args: {
        profileId: body.session ?? body.profile_id ?? body.id ?? '',
        hostKeyPolicy: resolveHostKeyPolicy(body),
      },
    }
  }
  if (pathname === '/api/sftp/disconnect' && verb === 'POST') {
    return {
      command: 'sftp_disconnect',
      args: { profileId: body.session ?? body.profile_id ?? body.id ?? '' },
    }
  }
  if (pathname === '/api/sftp/list' && verb === 'GET') {
    return {
      command: 'sftp_list',
      args: {
        profileId: params.get('session') ?? params.get('profile_id') ?? '',
        path: params.get('path') || '.',
      },
    }
  }
  if (pathname === '/api/sftp/realpath' && verb === 'GET') {
    return {
      command: 'sftp_realpath',
      args: {
        profileId: params.get('session') ?? params.get('profile_id') ?? '',
        path: params.get('path') || '.',
      },
    }
  }
  if (pathname === '/api/sftp/mkdir' && verb === 'POST') {
    return {
      command: 'sftp_mkdir',
      args: {
        profileId: body.session ?? body.profile_id ?? '',
        path: body.path ?? '',
      },
    }
  }
  if (pathname === '/api/sftp/mkdirs' && verb === 'POST') {
    return {
      command: 'sftp_mkdirs',
      args: {
        profileId: body.session ?? body.profile_id ?? '',
        paths: body.paths ?? [],
      },
    }
  }
  if (pathname === '/api/sftp/create-file' && verb === 'POST') {
    return {
      command: 'sftp_create_file',
      args: {
        profileId: body.session ?? body.profile_id ?? '',
        path: body.path ?? '',
      },
    }
  }
  if (pathname === '/api/sftp/rename' && verb === 'POST') {
    return {
      command: 'sftp_rename',
      args: {
        profileId: body.session ?? body.profile_id ?? '',
        from: body.path ?? body.from ?? '',
        to: body.new_path ?? body.to ?? '',
      },
    }
  }
  if (pathname === '/api/sftp/delete' && verb === 'POST') {
    return {
      command: 'sftp_delete',
      args: {
        profileId: body.session ?? body.profile_id ?? '',
        path: body.path ?? '',
        isDir: typeof body.is_dir === 'boolean' ? body.is_dir : null,
      },
    }
  }
  if (pathname === '/api/sftp/upload-local' && verb === 'POST') {
    const localPath = body.local_path ?? ''
    const remotePath =
      body.remote_path ??
      joinTargetPath(
        body.path ?? '.',
        body.relative_path || fileBaseName(localPath),
        'remote',
      )
    return {
      command: 'sftp_upload_local',
      args: {
        profileId: body.session ?? body.profile_id ?? '',
        localPath,
        remotePath,
        transferId: body.transfer_id ?? '',
        resume: Boolean(body.resume),
      },
    }
  }
  if (pathname === '/api/sftp/download-local' && verb === 'POST') {
    const remotePath = body.remote_path ?? ''
    const localPath = body.relative_path
      ? joinTargetPath(body.local_path ?? '.', body.relative_path, 'local')
      : (body.local_path ?? '')
    return {
      command: 'sftp_download_local',
      args: {
        profileId: body.session ?? body.profile_id ?? '',
        remotePath,
        localPath,
        transferId: body.transfer_id ?? '',
        resume: Boolean(body.resume),
      },
    }
  }
  if (pathname === '/api/sftp/copy-remote' && verb === 'POST') {
    const srcPath = body.src_path ?? body.source_path ?? ''
    const dstPath =
      body.dst_path ??
      joinTargetPath(
        body.target_path ?? '.',
        body.relative_path || fileBaseName(srcPath),
        'remote',
      )
    return {
      command: 'sftp_copy_remote',
      args: {
        srcProfileId: body.src_profile_id ?? body.source_session ?? '',
        srcPath,
        dstProfileId: body.dst_profile_id ?? body.target_session ?? '',
        dstPath,
        transferId: body.transfer_id ?? '',
      },
    }
  }
  if (pathname === '/api/sftp/archive-tools' && verb === 'GET') {
    return {
      command: 'sftp_archive_tools',
      args: { profileId: params.get('session') ?? params.get('profile_id') ?? '' },
    }
  }
  if (pathname === '/api/sftp/compress' && verb === 'POST') {
    const cwd = body.cwd ?? body.target_directory ?? '.'
    const format = body.format ?? 'tar.gz'
    const entries: string[] =
      body.entries ?? (Array.isArray(body.paths) ? body.paths.map(fileBaseName) : [])
    const archiveName =
      body.archive_name ?? body.name ?? defaultArchiveName(entries, format)
    return {
      command: 'sftp_compress',
      args: {
        profileId: body.session ?? body.profile_id ?? '',
        cwd,
        format,
        archiveName,
        entries,
      },
    }
  }
  if (pathname === '/api/sftp/extract' && verb === 'POST') {
    const archiveName = body.archive_name ?? body.path ?? ''
    const cwd = body.cwd ?? body.target_directory ?? parentDir(archiveName, 'remote')
    const destDir = body.dest_dir ?? body.target_directory ?? cwd
    return {
      command: 'sftp_extract',
      args: {
        profileId: body.session ?? body.profile_id ?? '',
        cwd,
        archiveName,
        destDir,
      },
    }
  }

  return null
}

export async function invokeApi<T>(path: string, options: RequestInit = {}): Promise<T> {
  const { pathname, params } = parseUrl(path)
  const verb = (options.method || 'GET').toUpperCase()
  const body = parseJsonBody(options.body)

  // Synthetic no-op / client-compat endpoints
  if (
    pathname === '/api/browser-client/heartbeat' ||
    pathname === '/api/browser-client/close' ||
    (pathname === '/api/transfer-count' && verb === 'POST')
  ) {
    return { ok: true } as unknown as T
  }
  if (pathname === '/api/launcher-logs' && verb === 'GET') {
    return { lines: [] } as unknown as T
  }
  if (pathname === '/api/sftp/server-copy-support' && verb === 'GET') {
    return { supported: false } as unknown as T
  }
  if (pathname === '/api/sftp/open-sync-events' && verb === 'GET') {
    return { events: [] } as unknown as T
  }
  if (pathname === '/api/transfer-control' && verb === 'POST' && body.action === 'reset') {
    return { ok: true } as unknown as T
  }

  // Raw JSON import from file text
  if (pathname === '/api/backend/import' && verb === 'POST') {
    const rawJson =
      typeof options.body === 'string' && !body.raw_json
        ? options.body
        : typeof body.raw_json === 'string'
          ? body.raw_json
          : JSON.stringify(body)
    return invoke<T>('import_profiles', { rawJson })
  }

  // Terminal open requires attaching a `Channel<TauriTerminalEvent>`
  if (pathname === '/api/session/shell/open' && verb === 'POST') {
    const channel = new Channel<TauriTerminalEvent>()
    const earlyQueue: TauriTerminalEvent[] = []
    let boundBuf: TerminalStreamBuffer | null = null
    channel.onmessage = ev => {
      if (boundBuf) {
        dispatchTerminalEvent(boundBuf, ev)
      } else {
        earlyQueue.push(ev)
      }
    }
    const res = await invoke<{
      id: string
      proxy_url?: string
      proxy_error?: string
      x11_enabled: boolean
      x11_error: string
    }>('open_terminal', {
      profileId: body.profile_id ?? body.id ?? '',
      cols: body.cols ?? 120,
      rows: body.rows ?? 32,
      hostKeyPolicy: resolveHostKeyPolicy(body),
      onEvent: channel,
    })
    let buf = terminalStreams.get(res.id)
    if (!buf) {
      buf = { sink: null, queued: [], exited: false }
      terminalStreams.set(res.id, buf)
    }
    boundBuf = buf
    for (const ev of earlyQueue) {
      dispatchTerminalEvent(buf, ev)
    }
    return res as unknown as T
  }

  if (pathname === '/api/session/shell/close' && verb === 'POST') {
    const sessionId = body.session ?? body.id ?? ''
    terminalStreams.delete(sessionId)
    await invoke('close_terminal', { sessionId })
    return { ok: true } as unknown as T
  }

  // SFTP remote file open (downloads to cache dir then opens locally)
  if ((pathname === '/api/sftp/open' || pathname === '/api/sftp/open-text') && verb === 'POST') {
    const profileId = body.session ?? body.profile_id ?? ''
    const remotePath = body.path ?? ''
    const backend = await invoke<{ sftp_open_cache_dir: string }>('get_backend_info')
    const localDir = joinTargetPath(backend.sftp_open_cache_dir, profileId || 'remote', 'local')
    await invoke('local_mkdir_cmd', { path: localDir })
    const localPath = joinTargetPath(localDir, fileBaseName(remotePath), 'local')
    await invoke('sftp_download_local', {
      profileId,
      remotePath,
      localPath,
      transferId: `open-${Date.now()}`,
      resume: false,
    })
    await invoke('local_open_cmd', { path: localPath })
    return { path: localPath } as unknown as T
  }

  // Local directory listing returns `{ path, entries }`
  if (pathname === '/api/local/list' && verb === 'GET') {
    const requestedPath = params.get('path') || ''
    const targetPath =
      !requestedPath || requestedPath === '.'
        ? await invoke<string>('get_local_home')
        : requestedPath
    const entries = await invoke<any[]>('local_list', { path: targetPath })
    return { path: targetPath, entries } as unknown as T
  }

  // SFTP directory listing returns `{ path, entries }`
  if (pathname === '/api/sftp/list' && verb === 'GET') {
    const profileId = params.get('session') ?? params.get('profile_id') ?? ''
    const requestedPath = params.get('path') || '.'
    const resolvedPath = await invoke<string>('sftp_realpath', {
      profileId,
      path: requestedPath,
    }).catch(() => requestedPath)
    const entries = await invoke<any[]>('sftp_list', {
      profileId,
      path: resolvedPath,
    })
    return { path: resolvedPath, entries } as unknown as T
  }

  const mapped = resolveTauriCommand(path, options.method, body)
  if (!mapped) {
    throw new Error(`Unmapped Tauri API route: ${options.method || 'GET'} ${path}`)
  }

  const rawResult = await invoke<any>(mapped.command, mapped.args)

  // Post-process response shapes to match existing Vue frontend expectations
  if (mapped.command === 'save_profile') {
    return (rawResult?.profile ?? rawResult) as T
  }
  if (mapped.command === 'profile_defaults') {
    return (rawResult?.remote ?? rawResult) as T
  }
  if (mapped.command === 'get_version') {
    return (typeof rawResult === 'string' ? { version: rawResult } : rawResult) as T
  }
  if (mapped.command === 'get_local_home' || mapped.command === 'sftp_realpath') {
    return (typeof rawResult === 'string' ? { path: rawResult } : rawResult) as T
  }
  if (mapped.command === 'get_session_status') {
    return (rawResult ?? {
      running: false,
      forwarding: false,
      phase: 'stopped',
      url: '',
      open_seq: 0,
    }) as T
  }
  if (mapped.command === 'list_session_statuses') {
    const list = Array.isArray(rawResult) ? rawResult : []
    const statuses: Record<string, any> = {}
    for (const item of list) {
      if (item && item.profile_id) {
        statuses[item.profile_id] = {
          ...item,
          last_failure: item.last_error ? { message: item.last_error, at: '' } : null,
        }
      }
    }
    return { statuses } as unknown as T
  }
  if (mapped.command === 'local_archive_tools' || mapped.command === 'sftp_archive_tools') {
    return {
      ...(rawResult || {}),
      formats: computeArchiveFormats(rawResult),
    } as unknown as T
  }
  if (mapped.command === 'local_compress' || mapped.command === 'sftp_compress') {
    return { archive: mapped.args?.archiveName ?? '' } as unknown as T
  }
  if (mapped.command === 'control_transfer') {
    return { ok: true, snapshot: rawResult } as unknown as T
  }

  return rawResult as T
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
  const res = await invoke<{ id: string }>('open_terminal', {
    profileId,
    cols,
    rows,
    hostKeyPolicy: hostKeyPolicy ?? null,
    onEvent: channel,
  })
  return res.id
}

export function sendTauriTerminalInput(sessionId: string, dataB64: string) {
  return invoke<{ accepted: boolean; flow_paused: boolean }>('send_terminal_input', {
    sessionId,
    dataB64,
  })
}

export async function subscribeTauriSessionLogs(
  profileId: string,
  onBatch: (lines: string[], isInitialReplay: boolean) => void,
): Promise<void> {
  let first = true
  const channel = new Channel<string[]>()
  channel.onmessage = lines => {
    const isInitial = first
    first = false
    onBatch(Array.isArray(lines) ? lines : [], isInitial)
  }
  await invoke('stream_session_logs', { profileId, onEvent: channel })
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
