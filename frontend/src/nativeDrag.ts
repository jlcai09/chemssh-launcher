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
  }
}

export interface NativeDragJob {
  id: string
  session: string
  paths: string[]
  state: 'queued' | 'accepted' | 'caching' | 'delivering' | 'done' | 'error' | 'cancelled'
  error?: string
  revision: number
  createdAt: number
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

export function cancelNativeDragJob(id: string, page: Window = window): Promise<boolean> {
  const host = nativeDragHost(page)
  return host?.chemsshCancelNativeDragJob?.(id) ?? Promise.resolve(false)
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
