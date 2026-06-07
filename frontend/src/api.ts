export interface Profile {
  id: string
  name: string
  ssh_host: string
  ssh_port: number
  ssh_user: string
  auth_method: string
  has_password: boolean
  private_key_path: string
  has_private_key_passphrase: boolean
  remote_host: string
  remote_port: number
  local_host: string
  local_port: number
  local_url_path: string
  pre_start_commands: string
  start_command: string
  health_check_url: string
  open_browser: boolean
}

export interface FileEntry {
  name: string
  path: string
  is_dir: boolean
  size: number
  mode: string
  mod_time: string
}

export interface HostKeyInfo {
  host: string
  address: string
  key_type: string
  fingerprint: string
  known_hosts_path: string
  mismatch: boolean
}

export class APIError extends Error {
  status = 0
  data: any
}

export async function api<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers: HeadersInit | undefined = options.body instanceof FormData ? undefined : { 'Content-Type': 'application/json' }
  const res = await fetch(path, { ...options, headers: options.headers || headers })
  const text = await res.text()
  const data = text ? JSON.parse(text) : {}
  if (!res.ok) {
    const err = new APIError(data.error || 'Request failed')
    err.status = res.status
    err.data = data
    throw err
  }
  return data as T
}

export function postJSON<T>(path: string, body: unknown) {
  return api<T>(path, { method: 'POST', body: JSON.stringify(body) })
}

export function putJSON<T>(path: string, body: unknown) {
  return api<T>(path, { method: 'PUT', body: JSON.stringify(body) })
}

export function formatBytes(value: number) {
  if (!value) return ''
  if (value < 1024) return `${value} B`
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`
  if (value < 1024 * 1024 * 1024) return `${(value / 1024 / 1024).toFixed(1)} MB`
  return `${(value / 1024 / 1024 / 1024).toFixed(1)} GB`
}

export function formatDate(value: string) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  return date.toLocaleString('zh-CN')
}

export function baseName(path: string) {
  const parts = path.replace(/\\/g, '/').split('/').filter(Boolean)
  return parts.at(-1) || path || '.'
}

export function parentPath(value: string, kind: 'local' | 'remote') {
  const normalized = String(value || '.').replace(/\\/g, '/')
  if (normalized === '.' || normalized === '/') return normalized
  if (kind === 'local' && /^[A-Za-z]:\/?$/.test(normalized)) return normalized
  const parts = normalized.split('/').filter(Boolean)
  parts.pop()
  if (/^[A-Za-z]:/.test(normalized)) return parts.length <= 1 ? `${parts[0]}/` : parts.join('/')
  if (normalized.startsWith('/')) return parts.length ? `/${parts.join('/')}` : '/'
  return parts.length ? parts.join('/') : '.'
}

export function joinPath(parent: string, name: string, kind: 'local' | 'remote') {
  const safe = name.replace(/\\/g, '/').split('/').filter(Boolean).join('/')
  if (!safe) return parent
  const sep = kind === 'local' ? '\\' : '/'
  if (!parent || parent === '.') return safe
  if (parent.endsWith('/') || parent.endsWith('\\')) return `${parent}${safe}`
  return `${parent}${sep}${safe}`
}
