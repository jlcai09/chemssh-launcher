// go-webview2 injects bindings in every frame, but only receives RPC messages
// from the top document. Always call the binding in its owning window.
export interface NativeDragResult {
  id: string
  status: string
  ok: boolean
}

export interface NativeDragRequest {
  session: string
  paths: string[]
  marker: string
}

declare global {
  interface Window {
    chemsshDragOutFiles?: (payload: string) => Promise<void>
    chemsshNativeDragJobs?: () => Promise<NativeDragJob[]>
    chemsshCancelNativeDragJob?: (id: string) => Promise<boolean>
    chemsshControlNativeDragJob?: (id: string, action: NativeDragAction) => Promise<boolean>
  }
}

export type NativeDragAction = 'pause' | 'resume' | 'cancel'

export interface NativeDragFile {
  path: string
  kind: 'file' | 'folder'
  loaded: number
  total: number
  state: 'caching' | 'done' | 'error'
  error?: string
}

export interface NativeDragJob {
  id: string
  session: string
  paths: string[]
  state: 'queued' | 'accepted' | 'caching' | 'delivering' | 'done' | 'error' | 'cancelled'
  error?: string
  revision: number
  createdAt: number
  loaded?: number
  total?: number
  paused?: boolean
  endedAt?: number
  files?: NativeDragFile[]
}

const jobEvent = 'chemssh-native-drag-job'

// A gesture may finish before extraction even starts. Subscribe separately and
// merge a host snapshot so navigation does not lose running transfers.
export function subscribeNativeDragJobs(changed: (jobs: NativeDragJob[]) => void, page: Window = window): () => void {
  const host = nativeDragHost(page)
  if (!host) return () => {}
  const jobs = new Map<string, NativeDragJob>()
  let stopped = false
  const receive = (job: NativeDragJob) => {
    if (stopped || !job || typeof job.id !== 'string' || !Array.isArray(job.paths) || !Number.isFinite(job.revision)) return
    if ((jobs.get(job.id)?.revision ?? -1) >= job.revision) return
    jobs.set(job.id, job)
    const history = [...jobs.values()].filter(item => ['done', 'error', 'cancelled'].includes(item.state)).sort((a, b) => b.revision - a.revision)
    for (const old of history.slice(50)) jobs.delete(old.id)
    changed([...jobs.values()].sort((a, b) => b.createdAt - a.createdAt || b.revision - a.revision))
  }
  const event = (event: Event) => receive((event as CustomEvent<NativeDragJob>).detail)
  const stop = () => {
    stopped = true
    host.removeEventListener(jobEvent, event)
    page.removeEventListener('pagehide', stop)
  }
  host.addEventListener(jobEvent, event)
  page.addEventListener('pagehide', stop)
  void host.chemsshNativeDragJobs?.().then(snapshot => {
    if (Array.isArray(snapshot)) snapshot.forEach(receive)
  }).catch(() => undefined)
  return stop
}

export function controlNativeDragJob(id: string, action: NativeDragAction, page: Window = window): Promise<boolean> {
  const host = nativeDragHost(page)
  if (host?.chemsshControlNativeDragJob) return host.chemsshControlNativeDragJob(id, action)
  if (action === 'cancel') return host?.chemsshCancelNativeDragJob?.(id) ?? Promise.resolve(false)
  return Promise.resolve(false)
}

export function cancelNativeDragJob(id: string, page: Window = window): Promise<boolean> {
  return controlNativeDragJob(id, 'cancel', page)
}

export function nativeDragCanControl(state: NativeDragJob['state']): boolean {
  return state === 'accepted' || state === 'caching'
}

// Project into the existing transfer row shape without exposing the cache path
// as the destination: the system drop target owns that location.
export function nativeDragTransferUpdates(jobs: NativeDragJob[], revisions: Map<string, number>, cleared: Set<string>): NativeDragJob[] {
  return [...jobs].reverse().filter(job => {
    const previous = revisions.get(job.id)
    if (job.state === 'queued' || (previous === undefined && job.state === 'cancelled')) return false
    if ((previous ?? -1) >= job.revision) return false
    revisions.set(job.id, job.revision)
    return !cleared.has(job.id)
  })
}

export interface NativeDragTransferDetail {
  key: string
  name: string
  path: string
  kind: 'file' | 'folder'
  loaded: number
  total: number
  status: 'running' | 'done' | 'error'
  error: string
}

export interface NativeDragTransferRoot extends NativeDragTransferDetail {
  group: boolean
  children: NativeDragTransferDetail[]
}

function normalizedRemotePath(path: string): string {
  const normalized = path.replace(/\/+$/, '')
  return normalized || '/'
}

function remotePrefix(path: string): string {
  return path === '/' ? '/' : `${path}/`
}

function remoteBaseName(path: string): string {
  const normalized = normalizedRemotePath(path)
  return normalized.slice(normalized.lastIndexOf('/') + 1) || normalized
}

export function nativeDragTransferRoots(job: NativeDragJob): NativeDragTransferRoot[] {
  const files = job.files || []
  const normalizedFiles = files.map(file => ({ ...file, normalizedPath: normalizedRemotePath(file.path) }))
  const selected = job.paths.map(normalizedRemotePath)
  const folderRoots = selected.filter(path => {
    const exact = normalizedFiles.find(file => file.normalizedPath === path)
    return exact?.kind === 'folder' || normalizedFiles.some(file => file.kind === 'file' && file.normalizedPath.startsWith(remotePrefix(path)))
  })
  const fileOwner = new Map<string, string>()
  for (const file of normalizedFiles.filter(item => item.kind === 'file')) {
    const owner = folderRoots.filter(root => file.normalizedPath.startsWith(remotePrefix(root))).sort((a, b) => b.length - a.length)[0]
    if (owner) fileOwner.set(file.normalizedPath, owner)
  }

  return selected.map((path, index) => {
    const rootFile = normalizedFiles.find(file => file.normalizedPath === path)
    const group = folderRoots.includes(path)
    const children = group
      ? normalizedFiles.filter(file => file.kind === 'file' && fileOwner.get(file.normalizedPath) === path).map(file => ({
          key: file.normalizedPath,
          name: file.normalizedPath.slice(remotePrefix(path).length),
          path: file.path,
          kind: 'file' as const,
          loaded: Math.max(0, file.loaded || 0),
          total: Math.max(0, file.total || 0, file.loaded || 0),
          status: file.state === 'done' ? 'done' as const : file.state === 'error' ? 'error' as const : 'running' as const,
          error: file.error || ''
        }))
      : []
    const aggregate = group
      ? { loaded: children.reduce((sum, file) => sum + file.loaded, 0), total: children.reduce((sum, file) => sum + file.total, 0) }
      : { loaded: rootFile?.loaded ?? (selected.length === 1 ? job.loaded || 0 : 0), total: rootFile?.total ?? (selected.length === 1 ? job.total || 0 : 0) }
    const projected = nativeDragTransfer(job)
    return {
      key: `${index}:${path}`,
      name: remoteBaseName(path),
      path: job.paths[index],
      kind: group ? 'folder' as const : 'file' as const,
      loaded: aggregate.loaded,
      total: Math.max(aggregate.total, aggregate.loaded),
      status: rootFile?.state === 'error' && children.length === 0 ? 'error' as const : projected.status,
      error: rootFile?.error || projected.error,
      group,
      children
    }
  })
}

// Replace one job's projected rows where they already are. Progress events must
// not move a visible task; only a job without rows yet joins at the top.
export function mergeNativeDragRows<T extends { id: string; nativeDragOwnerID?: string; nativeDragID?: string }>(
  items: T[],
  jobID: string,
  rows: T[]
): T[] {
  const owned = (item: T) => item.nativeDragOwnerID === jobID || item.nativeDragID === jobID
  const anchor = items.findIndex(owned)
  if (anchor < 0) return [...rows, ...items]
  const rest = items.filter(item => !owned(item))
  return [...rest.slice(0, anchor), ...rows, ...rest.slice(anchor)]
}

export function retainTransferHistory<T extends { id: string; status: string; parentID?: string }>(items: T[], limit = 300): T[] {
  const terminalRoots = items.filter(item => !item.parentID && item.status !== 'running').slice(0, limit)
  const retained = new Set(terminalRoots.map(item => item.id))
  // Scanning groups can have no running children between directory listings.
  const activeParents = new Set(items.filter(item => item.status === 'running').map(item => item.parentID || item.id))
  return items.filter(item => item.status === 'running' || (item.parentID ? retained.has(item.parentID) || activeParents.has(item.parentID) : retained.has(item.id)))
}

export function nativeDragTransfer(job: NativeDragJob) {
  const terminal = ['done', 'error', 'cancelled'].includes(job.state)
  return {
    nativeDragID: job.id,
    nativeDragState: job.state,
    loaded: Math.max(0, job.loaded || 0),
    total: Math.max(0, job.total || 0, job.loaded || 0),
    startedAt: job.createdAt,
    endedAt: terminal ? (job.endedAt || job.createdAt) : null,
    status: (job.state === 'done' ? 'done' : terminal ? 'error' : 'running') as 'running' | 'done' | 'error',
    paused: !terminal && Boolean(job.paused),
    cancelled: job.state === 'cancelled',
    error: job.error || ''
  }
}

const finishedEvent = 'chemssh-native-drag-finished'
const stagingEvent = 'chemssh-native-drag-staging'
let nextDragID = 0

export function nativeDragHost(page: Window = window): Window | null {
  try {
    const host = page.top || page
    // Reading location also rejects an embedded page from a different origin.
    if (host.location.origin !== page.location.origin) return null
    return typeof host.chemsshDragOutFiles === 'function' ? host : null
  } catch {
    return null
  }
}

// Resolve only when the OLE loop finishes, not when Bind acknowledges the RPC.
// An ID keeps a late completion from resetting a different page's next gesture.
export function dragOutFiles(
  request: NativeDragRequest,
  page: Window = window,
  onCaching?: (caching: boolean) => void
): Promise<NativeDragResult> {
  const host = nativeDragHost(page)
  if (!host) return Promise.reject(new Error('Native file drag is not available'))
  const id = `${Date.now()}-${++nextDragID}-${Math.random().toString(36).slice(2)}`
  return new Promise((resolve, reject) => {
    const cleanup = () => {
      host.removeEventListener(finishedEvent, finished)
      host.removeEventListener(stagingEvent, staging)
      page.removeEventListener('pagehide', unloaded)
      onCaching?.(false)
    }
    const failed = (error: unknown) => {
      cleanup()
      reject(error)
    }
    const finished = (event: Event) => {
      const result = (event as CustomEvent<NativeDragResult>).detail
      if (!result || result.id !== id) return
      cleanup()
      resolve(result)
    }
    const staging = (event: Event) => {
      const state = (event as CustomEvent<{ id: string; caching: boolean }>).detail
      if (state?.id === id && typeof state.caching === 'boolean') onCaching?.(state.caching)
    }
    const unloaded = () => failed(new Error('File page was closed during native drag'))
    host.addEventListener(finishedEvent, finished)
    host.addEventListener(stagingEvent, staging)
    page.addEventListener('pagehide', unloaded)
    try {
      void host.chemsshDragOutFiles!(JSON.stringify({ ...request, id })).catch(failed)
    } catch (error) {
      failed(error)
    }
  })
}
