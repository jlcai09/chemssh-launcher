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

export interface ParsedTauriInvokeError {
  message: string
  status: number
  data: Record<string, any>
  transportLost: boolean
}

export function parseTauriInvokeError(raw: unknown): ParsedTauriInvokeError {
  const rawMsg = raw instanceof Error ? raw.message : String(raw)
  let parsedData: Record<string, any> = { error: rawMsg }
  let displayMsg = rawMsg || 'Request failed'
  const trimmed = rawMsg.trim()
  if (trimmed.startsWith('{') && trimmed.endsWith('}')) {
    try {
      const obj = JSON.parse(trimmed)
      if (obj && typeof obj === 'object') {
        parsedData = obj
        if (typeof obj.error === 'string' && obj.error) {
          displayMsg = obj.error
        }
      }
    } catch {
      // keep plain text error
    }
  }
  return {
    message: displayMsg,
    status: 500,
    data: parsedData,
    transportLost: Boolean(parsedData.transport_lost),
  }
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

function encodeBase64Bytes(bytes: Uint8Array): string {
  let binary = ''
  const chunkSize = 8192
  for (let offset = 0; offset < bytes.length; offset += chunkSize) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + chunkSize))
  }
  return btoa(binary)
}

type QueuedTerminalFrame =
  | { kind: 'output'; bytes: Uint8Array }
  | { kind: 'exit' }

interface TerminalStreamBuffer {
  sink: TerminalSink | null
  queued: QueuedTerminalFrame[]
  exited: boolean
  lastSeq: number
  channelDelivered: boolean
  ws: WebSocket | null
}

const terminalStreams = new Map<string, TerminalStreamBuffer>()
let terminalPollTimer: number | null = null
let terminalPollInFlight = false
const sharedTextEncoder = new TextEncoder()

export interface TerminalWsDecodedFrame {
  kind: 'output' | 'exit'
  seq: number
  bytes?: Uint8Array
}

export function decodeTerminalWsFrame(data: ArrayBuffer): TerminalWsDecodedFrame | null {
  if (!(data instanceof ArrayBuffer) || data.byteLength < 9) return null
  const view = new Uint8Array(data)
  const dv = new DataView(data)
  const tag = view[0]
  const seq = Number(dv.getBigUint64(1, false))
  if (tag === 0) {
    return { kind: 'output', seq, bytes: view.subarray(9) }
  }
  if (tag === 1) {
    return { kind: 'exit', seq }
  }
  return null
}

export interface TerminalInputQueue {
  enqueue(sessionId: string, text: string): void
  hasInFlight(sessionId: string): boolean
  clear(sessionId: string): void
}

/**
 * Serializes terminal input per session cleanly with zero artificial delay:
 * the first keystroke invokes immediately, while any keys arriving while that
 * IPC call is still in flight coalesce into one ordered batch.
 */
export function createTerminalInputQueue(
  sender: (sessionId: string, data: string) => Promise<unknown>,
): TerminalInputQueue {
  const pending = new Map<string, string>()
  const inFlight = new Set<string>()

  const pump = (sessionId: string) => {
    if (inFlight.has(sessionId)) return
    const batch = pending.get(sessionId)
    if (!batch) {
      pending.delete(sessionId)
      return
    }
    pending.delete(sessionId)
    inFlight.add(sessionId)
    Promise.resolve(sender(sessionId, batch))
      .catch(err => {
        console.error('[tauriBridge] send_terminal_input failed:', err)
      })
      .finally(() => {
        inFlight.delete(sessionId)
        if (pending.has(sessionId)) pump(sessionId)
      })
  }

  return {
    enqueue(sessionId: string, text: string) {
      if (!sessionId || !text) return
      pending.set(sessionId, (pending.get(sessionId) ?? '') + text)
      pump(sessionId)
    },
    hasInFlight(sessionId: string) {
      return Boolean(sessionId && inFlight.has(sessionId))
    },
    clear(sessionId: string) {
      if (!sessionId) return
      pending.delete(sessionId)
    },
  }
}

const terminalInputQueue = createTerminalInputQueue(async (sessionId, data) => {
  try {
    const res = await invoke<unknown>('send_terminal_input', { sessionId, data })
    if (
      res === 'Closed' ||
      (typeof res === 'object' && res !== null && (res as { accepted?: boolean }).accepted === false)
    ) {
      const buf = terminalStreams.get(sessionId)
      if (buf && !buf.exited) {
        deliverTerminalFrame(buf, 'exit', undefined, () => new Uint8Array(0), false)
      }
    }
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    if (msg.includes('terminal session not found')) {
      const buf = terminalStreams.get(sessionId)
      if (buf && !buf.exited) {
        deliverTerminalFrame(buf, 'exit', undefined, () => new Uint8Array(0), false)
      }
      return
    }
    throw err
  }
})

export function sendTauriTerminalText(sessionId: string, text: string): void {
  if (!sessionId || !text) return
  const buf = terminalStreams.get(sessionId)
  if (
    buf?.ws &&
    buf.ws.readyState === WebSocket.OPEN &&
    !terminalInputQueue.hasInFlight(sessionId)
  ) {
    buf.ws.send(sharedTextEncoder.encode(text))
    return
  }
  terminalInputQueue.enqueue(sessionId, text)
}

function stopPollTimerIfIdle(): void {
  if (terminalPollTimer === null) return
  for (const buf of terminalStreams.values()) {
    if (!buf.exited && !buf.channelDelivered) return
  }
  if (typeof window !== 'undefined') {
    window.clearInterval(terminalPollTimer)
  }
  terminalPollTimer = null
}

function deliverTerminalFrame(
  buf: TerminalStreamBuffer,
  kind: 'output' | 'exit',
  seq: number | undefined,
  getBytes: () => Uint8Array,
  fromPush = false,
): void {
  if (fromPush) {
    const firstDelivery = !buf.channelDelivered
    buf.channelDelivered = true
    if (firstDelivery) {
      stopPollTimerIfIdle()
    }
  }
  if (typeof seq === 'number') {
    if (seq <= buf.lastSeq) return
    buf.lastSeq = seq
  }
  if (!buf.sink) {
    buf.queued.push(kind === 'exit' ? { kind: 'exit' } : { kind: 'output', bytes: getBytes() })
    return
  }
  if (kind === 'output') {
    const bytes = getBytes()
    if (bytes.byteLength > 0) buf.sink.onData(bytes)
  } else {
    buf.exited = true
    buf.sink.onExit()
  }
}

function dispatchTerminalEvent(
  buf: TerminalStreamBuffer,
  ev: TauriTerminalEvent,
  fromChannel = false,
) {
  if (ev.kind === 'output' && ev.data_b64) {
    const b64 = ev.data_b64
    deliverTerminalFrame(buf, 'output', ev.seq, () => decodeBase64Bytes(b64), fromChannel)
  } else if (ev.kind === 'exit') {
    deliverTerminalFrame(buf, 'exit', ev.seq, () => new Uint8Array(0), fromChannel)
  }
}

function attachTerminalWebSocket(
  sessionId: string,
  buf: TerminalStreamBuffer,
  wsUrl: string,
): void {
  if (typeof WebSocket === 'undefined' || !wsUrl) return
  const sep = wsUrl.includes('?') ? '&' : '?'
  const ws = new WebSocket(`${wsUrl}${sep}since=${buf.lastSeq}`)
  ws.binaryType = 'arraybuffer'
  buf.ws = ws
  ws.onmessage = event => {
    if (!(event.data instanceof ArrayBuffer)) return
    const decoded = decodeTerminalWsFrame(event.data)
    if (!decoded) return
    if (decoded.kind === 'output') {
      const bytes = decoded.bytes ?? new Uint8Array(0)
      deliverTerminalFrame(buf, 'output', decoded.seq, () => bytes, true)
    } else {
      deliverTerminalFrame(buf, 'exit', decoded.seq, () => new Uint8Array(0), true)
    }
  }
  ws.onclose = () => {
    if (buf.ws === ws) buf.ws = null
    if (!buf.exited && terminalStreams.get(sessionId) === buf) {
      // Loopback WS dropped (e.g. after OS sleep/wake): re-enable polling fallback
      // so any pending output or Exit event is delivered immediately.
      buf.channelDelivered = false
      ensureTerminalPollLoop()
    }
  }
  ws.onerror = () => {
    if (buf.ws === ws) buf.ws = null
    if (!buf.exited && terminalStreams.get(sessionId) === buf) {
      buf.channelDelivered = false
      ensureTerminalPollLoop()
    }
  }
}

async function pollTerminalEventsOnce(): Promise<void> {
  if (terminalPollInFlight) return
  const cursors: Record<string, number> = {}
  let pending = false
  for (const [sessionId, buf] of terminalStreams.entries()) {
    if (!buf.exited && !buf.channelDelivered) {
      cursors[sessionId] = buf.lastSeq
      pending = true
    }
  }
  if (!pending) {
    if (terminalPollTimer !== null) {
      window.clearInterval(terminalPollTimer)
      terminalPollTimer = null
    }
    return
  }
  terminalPollInFlight = true
  try {
    const batches = await invoke<Record<string, TauriTerminalEvent[]>>('poll_terminal_events', {
      cursors,
    })
    for (const [sessionId, events] of Object.entries(batches || {})) {
      const buf = terminalStreams.get(sessionId)
      if (!buf || !Array.isArray(events)) continue
      for (const ev of events) {
        dispatchTerminalEvent(buf, ev, false)
      }
    }
  } catch (err) {
    console.error('[tauriBridge] poll_terminal_events failed:', err)
  } finally {
    terminalPollInFlight = false
  }
}

function ensureTerminalPollLoop(): void {
  if (typeof window === 'undefined') return
  if (terminalPollTimer !== null) return
  terminalPollTimer = window.setInterval(() => {
    void pollTerminalEventsOnce()
  }, 25)
}

export function attachTauriTerminalSink(sessionId: string, sink: TerminalSink): () => void {
  let buf = terminalStreams.get(sessionId)
  if (!buf) {
    buf = { sink: null, queued: [], exited: false, lastSeq: 0, channelDelivered: false, ws: null }
    terminalStreams.set(sessionId, buf)
  }
  buf.sink = sink
  const pending = buf.queued.splice(0, buf.queued.length)
  for (const item of pending) {
    if (item.kind === 'output') {
      if (item.bytes.byteLength > 0) sink.onData(item.bytes)
    } else {
      buf.exited = true
      sink.onExit()
    }
  }
  if (!buf.exited && !buf.channelDelivered) {
    ensureTerminalPollLoop()
    void pollTerminalEventsOnce()
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
  if (pathname === '/api/installer/check' && verb === 'POST') {
    return {
      command: 'installer_check',
      args: {
        profileId: body.profile_id ?? body.id ?? '',
        options: body.options ?? {},
        hostKeyPolicy: resolveHostKeyPolicy(body),
      },
    }
  }
  if (pathname === '/api/installer/run' && verb === 'POST') {
    return {
      command: 'installer_run',
      args: {
        profileId: body.profile_id ?? body.id ?? '',
        options: body.options ?? {},
        mode: body.mode ?? 'auto',
        hostKeyPolicy: resolveHostKeyPolicy(body),
      },
    }
  }
  if (pathname === '/api/installer/status' && verb === 'GET') {
    return {
      command: 'installer_status',
      args: {
        profileId: params.get('profile_id') ?? '',
        since: Number(params.get('since') ?? 0) || 0,
      },
    }
  }
  if (pathname === '/api/installer/apply' && verb === 'POST') {
    return {
      command: 'installer_apply_profile',
      args: { profileId: body.profile_id ?? body.id ?? '', options: body.options ?? {} },
    }
  }
  if (pathname === '/api/defaults' && verb === 'GET') {
    return { command: 'profile_defaults' }
  }
  if (pathname === '/api/version' && verb === 'GET') {
    return { command: 'get_version' }
  }

  // Backend & UI settings
  if (pathname === '/api/launcher-logs' && verb === 'GET') {
    return { command: 'get_launcher_logs' }
  }
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
  if (pathname === '/api/local/open' && verb === 'POST') {
    return { command: 'local_open_cmd', args: { path: body.path ?? '' } }
  }
  if (pathname === '/api/local/open-text' && verb === 'POST') {
    return { command: 'local_open_text_cmd', args: { path: body.path ?? '' } }
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
      command: 'sftp_reconnect',
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
  if (pathname === '/api/sftp/open' && verb === 'POST') {
    return {
      command: 'sftp_open_file',
      args: {
        profileId: body.session ?? body.profile_id ?? '',
        path: body.path ?? '',
        asText: false,
      },
    }
  }
  if (pathname === '/api/sftp/open-text' && verb === 'POST') {
    return {
      command: 'sftp_open_file',
      args: {
        profileId: body.session ?? body.profile_id ?? '',
        path: body.path ?? '',
        asText: true,
      },
    }
  }
  if (pathname === '/api/sftp/open-sync-events' && verb === 'GET') {
    return {
      command: 'sftp_open_sync_events',
      args: { after: Number(params.get('after') || 0) },
    }
  }
  if (pathname === '/api/sftp/server-copy-support' && verb === 'GET') {
    return {
      command: 'sftp_server_copy_support',
      args: {
        source: params.get('source') ?? '',
        target: params.get('target') ?? '',
      },
    }
  }
  if (pathname === '/api/sftp/copy-on-server' && verb === 'POST') {
    return {
      command: 'sftp_copy_on_server',
      args: {
        sourceSession: body.source_session ?? '',
        targetSession: body.target_session ?? '',
        targetPath: body.target_path ?? '',
        items: Array.isArray(body.items) ? body.items : [],
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
        dispatchTerminalEvent(boundBuf, ev, true)
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
      ws_url?: string
    }>('open_terminal', {
      profileId: body.profile_id ?? body.id ?? '',
      cols: body.cols ?? 120,
      rows: body.rows ?? 32,
      hostKeyPolicy: resolveHostKeyPolicy(body),
      onEvent: channel,
    })
    let buf = terminalStreams.get(res.id)
    if (!buf) {
      buf = { sink: null, queued: [], exited: false, lastSeq: 0, channelDelivered: false, ws: null }
      terminalStreams.set(res.id, buf)
    }
    boundBuf = buf
    for (const ev of earlyQueue) {
      dispatchTerminalEvent(buf, ev, true)
    }
    if (res.ws_url) {
      attachTerminalWebSocket(res.id, buf, res.ws_url)
    }
    ensureTerminalPollLoop()
    return res as unknown as T
  }

  if (pathname === '/api/session/shell/resize' && verb === 'POST') {
    const sessionId = String(body.session ?? body.id ?? '')
    const cols = Number(body.cols ?? 120)
    const rows = Number(body.rows ?? 32)
    const buf = terminalStreams.get(sessionId)
    if (buf?.ws && buf.ws.readyState === WebSocket.OPEN) {
      buf.ws.send(JSON.stringify({ type: 'resize', cols, rows }))
      return { ok: true } as unknown as T
    }
    await invoke('resize_terminal', { sessionId, cols, rows })
    return { ok: true } as unknown as T
  }

  if (pathname === '/api/session/shell/close' && verb === 'POST') {
    const sessionId = body.session ?? body.id ?? ''
    terminalInputQueue.clear(sessionId)
    const existing = terminalStreams.get(sessionId)
    if (existing?.ws) {
      try {
        existing.ws.close()
      } catch {
        // ignore close errors
      }
      existing.ws = null
    }
    terminalStreams.delete(sessionId)
    await invoke('close_terminal', { sessionId })
    return { ok: true } as unknown as T
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
  if (mapped.command === 'get_session_logs') {
    return (Array.isArray(rawResult) ? { lines: rawResult } : rawResult) as T
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

export async function probeTauriTerminalSessions(sessionIds?: string[]): Promise<string[]> {
  if (!isTauriRuntime()) return []
  const dead = await invoke<string[]>('probe_terminal_sessions', {
    sessionIds: sessionIds && sessionIds.length > 0 ? sessionIds : null,
  })
  return Array.isArray(dead) ? dead : []
}

export interface TauriSessionLogPollResponse {
  profile_id: string
  lines: string[]
  last_seq: number
  reset: boolean
}

export interface SessionLogCursor {
  apply(lines: string[], isReplay: boolean, batchLastSeq?: number): boolean
  lastSeq(): number
}

/**
 * Deduplicates session log batches arriving from both Tauri Channel push and
 * `poll_session_logs` fallback so lines are delivered once and in order.
 */
export function createSessionLogCursor(
  onBatch: (lines: string[], isInitialReplay: boolean) => void,
): SessionLogCursor {
  let cursor = 0
  let initialized = false

  return {
    apply(lines: string[], isReplay: boolean, batchLastSeq?: number): boolean {
      if (typeof batchLastSeq === 'number') {
        if (isReplay) {
          if (initialized && batchLastSeq <= cursor) {
            return false
          }
          cursor = Math.max(cursor, batchLastSeq)
          initialized = true
          onBatch(lines, true)
          return true
        }
        if (batchLastSeq <= cursor) {
          return false
        }
        cursor = batchLastSeq
        initialized = true
        if (lines.length > 0) {
          onBatch(lines, false)
        }
        return true
      }

      if (isReplay) {
        if (initialized && lines.length === 0) return false
        initialized = true
        onBatch(lines, true)
        return true
      }
      if (lines.length === 0) return false
      initialized = true
      onBatch(lines, false)
      return true
    },
    lastSeq(): number {
      return cursor
    },
  }
}

export function subscribeTauriSessionLogs(
  profileId: string,
  onBatch: (lines: string[], isInitialReplay: boolean) => void,
): () => void {
  let disposed = false
  let first = true
  let channelLive = false
  let pollInFlight = false
  let pollTimer: number | null = null

  const cursor = createSessionLogCursor((lines, isReplay) => {
    if (!disposed) onBatch(lines, isReplay)
  })

  const pollOnce = async () => {
    if (disposed || pollInFlight) return
    pollInFlight = true
    try {
      const res = await invoke<TauriSessionLogPollResponse>('poll_session_logs', {
        profileId,
        afterSeq: cursor.lastSeq(),
      })
      if (disposed || !res) return
      const lines = Array.isArray(res.lines) ? res.lines : []
      cursor.apply(lines, Boolean(res.reset), typeof res.last_seq === 'number' ? res.last_seq : undefined)
    } catch {
      // Fallback to legacy get_session_logs if poll_session_logs is unavailable.
      try {
        const legacy = await invoke<string[]>('get_session_logs', { profileId })
        if (!disposed && Array.isArray(legacy) && legacy.length > 0) {
          cursor.apply(legacy, true)
        }
      } catch {
        // Ignore transient IPC errors; next poll tick will retry.
      }
    } finally {
      pollInFlight = false
    }
  }

  const schedulePoll = () => {
    if (disposed) return
    const delayMs = channelLive ? 2500 : 500
    pollTimer = window.setTimeout(() => {
      pollTimer = null
      void pollOnce().finally(schedulePoll)
    }, delayMs)
  }

  const channel = new Channel<
    string[] | { profile_id?: string; lines?: string[]; is_replay?: boolean; last_seq?: number }
  >()
  channel.onmessage = payload => {
    if (disposed) return
    const isObj = typeof payload === 'object' && payload !== null && !Array.isArray(payload)
    const isInitial = isObj && typeof payload.is_replay === 'boolean' ? payload.is_replay : first
    first = false
    const lines = Array.isArray(payload)
      ? payload
      : Array.isArray(payload?.lines)
        ? payload.lines
        : []
    const lastSeq = isObj && typeof payload.last_seq === 'number' ? payload.last_seq : undefined
    if (!isInitial && lines.length > 0) {
      channelLive = true
    }
    cursor.apply(lines, isInitial, lastSeq)
  }

  // Always fetch initial snapshot via standard invoke so iframe Channel issues
  // can never leave the log panel blank, then keep polling as a safety net.
  void pollOnce().finally(schedulePoll)
  void invoke('stream_session_logs', { profileId, onEvent: channel }).catch(() => undefined)

  return () => {
    disposed = true
    if (pollTimer !== null) {
      window.clearTimeout(pollTimer)
      pollTimer = null
    }
  }
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
  // Opens the WebView2 download list, or the Downloads folder where it is unavailable.
  window.chemsshOpenDownloads = async () => {
    await invoke('open_downloads_panel')
  }
  window.chemsshCloseDownloads = async () => {
    await invoke('close_downloads_panel')
  }
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
  // Do not expose window.chemsshDragOutFiles here until Tauri supports delayed
  // SFTP staging during OLE DoDragDrop: invoking start_native_drag_out with
  // empty staged_local_paths fails immediately and aborts remote file drags
  // right after long-press arming. Leaving it unset falls back cleanly to the
  // built-in HTML5 drag-and-drop flow (same-pane move & cross-pane transfer).
}

export function tauriFileIconURL(name: string, isDir: boolean, size = 16): string {
  const params = new URLSearchParams({
    name: name || (isDir ? 'folder' : 'file'),
    is_dir: isDir ? '1' : '0',
    size: String(size || 16),
  })
  const isWindows =
    typeof navigator !== 'undefined' && /windows/i.test(navigator.userAgent || '')
  const origin = isWindows ? 'http://fileicon.localhost' : 'fileicon://localhost'
  return `${origin}/icon?${params.toString()}`
}

export async function uploadFormViaTauri(
  form: FormData,
  onProgress?: (loaded: number, total: number) => void,
): Promise<void> {
  const file = form.get('file')
  if (!(file instanceof File)) {
    throw new Error('upload form is missing file payload')
  }
  const sessionId = String(form.get('session') ?? '')
  const targetDir = String(form.get('path') ?? '.')
  const relativePath = String(form.get('relative_path') ?? '') || file.name
  const transferId = String(form.get('transfer_id') ?? '')
  const remotePath = joinTargetPath(targetDir, relativePath, 'remote')

  const { upload_id: uploadId } = await invoke<{ upload_id: string }>('sftp_upload_bytes_begin', {
    profileId: sessionId,
    remotePath,
    transferId: transferId || null,
    totalSize: file.size,
  })

  const chunkSize = 512 * 1024
  try {
    let offset = 0
    while (offset < file.size) {
      const end = Math.min(file.size, offset + chunkSize)
      const buf = await file.slice(offset, end).arrayBuffer()
      const chunkB64 = encodeBase64Bytes(new Uint8Array(buf))
      await invoke('sftp_upload_bytes_chunk', { uploadId, chunkB64 })
      offset = end
      onProgress?.(offset, file.size)
    }
    await invoke('sftp_upload_bytes_finish', { uploadId, abort: false })
  } catch (err) {
    await invoke('sftp_upload_bytes_finish', { uploadId, abort: true }).catch(() => undefined)
    throw err
  }
}

export async function downloadRemoteFileViaTauri(
  sessionId: string,
  remotePath: string,
  suggestedName?: string,
): Promise<void> {
  const res = await invoke<{ name: string; data_b64: string; size: number }>(
    'sftp_download_bytes',
    {
      profileId: sessionId,
      remotePath,
    },
  )
  const bytes = decodeBase64Bytes(res.data_b64)
  const copy = new Uint8Array(bytes.byteLength)
  copy.set(bytes)
  const blob = new Blob([copy.buffer])
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = suggestedName || res.name || fileBaseName(remotePath) || 'download'
  document.body.appendChild(link)
  link.click()
  link.remove()
  window.setTimeout(() => URL.revokeObjectURL(url), 1000)
}
