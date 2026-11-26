import { shallowRef } from 'vue'
import { appEvents } from './appEvents.ts'
import {
  cancelTauriDownload,
  newTauriDownloadId,
  runTauriDownload,
  type TauriDownloadSource,
  type TauriDownloadTarget,
} from './tauriBridge.ts'
import type { TransferProgressSnapshot } from './transferProgress'

export type DownloadState = 'queued' | 'running' | 'cancelling' | 'committing' | 'saved' | 'cancelled' | 'failed'
export type DownloadOutcome = 'saved' | 'cancelled' | 'failed'

export interface NativeDownloadJob {
  id: string
  sessionId: string
  sourceName: string
  remotePath: string
  localPath: string
  loaded: number
  total: number
  startedAt: number | null
  state: DownloadState
  error: string
}

interface DownloadRequest {
  sessionId: string
  sourceName: string
  sources: readonly TauriDownloadSource[]
  targets: readonly TauriDownloadTarget[]
}

interface DownloadTransport {
  id: () => string
  watch: (id: string, fn: (snapshot: TransferProgressSnapshot) => void) => () => void
  run: typeof runTauriDownload
  cancel: typeof cancelTauriDownload
}

interface Worker {
  // `request` resolves after the backend accepted or refused cancellation.
  request?: Promise<TransferProgressSnapshot>
  cancelled?: boolean
  settle: (outcome: DownloadOutcome) => void
  done: Promise<DownloadOutcome>
}

export function downloadActive(job: NativeDownloadJob): boolean {
  return !['saved', 'cancelled', 'failed'].includes(job.state)
}

export function canCancelDownload(job: NativeDownloadJob): boolean {
  return job.state === 'queued' || job.state === 'running'
}

export function createDownloadQueue(transport: DownloadTransport, concurrency = 2) {
  const jobs = shallowRef<readonly NativeDownloadJob[]>([])
  const workers = new Map<string, Worker>()
  const waiting: { id: string; overwrite: boolean }[] = []
  let running = 0

  const current = (id: string) => jobs.value.find(job => job.id === id)
  const update = (id: string, patch: Partial<NativeDownloadJob>) => {
    jobs.value = jobs.value.map(job => job.id === id ? { ...job, ...patch } : job)
  }

  const progress = (id: string, snapshot: TransferProgressSnapshot) => {
    const job = current(id)
    const worker = workers.get(id)
    if (!job || !worker || !downloadActive(job)) return
    if (snapshot.cancelled) worker.cancelled = true
    const state = snapshot.committing || job.state === 'committing' ? 'committing'
      : snapshot.cancelled || job.state === 'cancelling' ? 'cancelling' : 'running'
    update(id, { loaded: snapshot.loaded, total: snapshot.total, state })
  }

  function finish(id: string, outcome: DownloadOutcome, error = '') {
    const worker = workers.get(id)
    workers.delete(id)
    update(id, { state: outcome, error })
    worker?.settle(outcome)
  }

  function pump() {
    while (running < concurrency && waiting.length) {
      const next = waiting.shift()!
      const job = current(next.id)
      if (!job || job.state !== 'queued') continue
      running++
      update(job.id, { state: 'running', startedAt: Date.now() })
      void run(job, next.overwrite).finally(() => { running--; pump() })
    }
  }

  async function run(job: NativeDownloadJob, overwrite: boolean) {
    let stop: (() => void) | undefined
    try {
      stop = transport.watch(job.id, snapshot => progress(job.id, snapshot))
      await transport.run(job.sessionId, job.remotePath, job.localPath, job.id, overwrite)
      const row = current(job.id)
      if (row?.total) update(job.id, { loaded: row.total })
      finish(job.id, 'saved')
    } catch (error) {
      // The invoke settles after staging cleanup; a cancellation request may
      // have been acknowledged earlier or may have lost the publication race.
      const worker = workers.get(job.id)
      const requested = await worker?.request?.then(snapshot => snapshot.cancelled, () => false)
      finish(job.id, worker?.cancelled || requested ? 'cancelled' : 'failed', error instanceof Error ? error.message : String(error))
    } finally {
      stop?.()
    }
  }

  function enqueue({ sessionId, sourceName, sources, targets }: DownloadRequest) {
    if (sources.length !== targets.length) throw new Error('download target count did not match source count')
    const added = sources.map((source, index): NativeDownloadJob => ({
      id: transport.id(), sessionId, sourceName, remotePath: source.path,
      localPath: targets[index]!.local_path, loaded: 0, total: source.size || 0,
      startedAt: null, state: 'queued', error: '',
    }))
    const results = added.map(job => {
      let settle!: Worker['settle']
      const done = new Promise<DownloadOutcome>(resolve => { settle = resolve })
      workers.set(job.id, { settle, done })
      waiting.push({ id: job.id, overwrite: sources.length === 1 })
      return done
    })
    const existingActive = jobs.value.filter(downloadActive)
    jobs.value = [...added, ...existingActive, ...jobs.value.filter(job => !downloadActive(job)).slice(0, 100)]
    pump()
    return { count: added.length, done: Promise.all(results) }
  }

  async function cancel(id: string): Promise<void> {
    const job = current(id)
    const worker = workers.get(id)
    if (!job || !worker || !canCancelDownload(job)) return
    if (job.state === 'queued') {
      const index = waiting.findIndex(next => next.id === id)
      if (index !== -1) waiting.splice(index, 1)
      finish(id, 'cancelled')
      return
    }
    update(id, { state: 'cancelling' })
    const request = Promise.resolve().then(() => transport.cancel(id))
    worker.request = request
    try {
      const snapshot = await request
      if (snapshot.cancelled) worker.cancelled = true
      if (workers.get(id) !== worker) return
      if (snapshot.committing) update(id, { state: 'committing' })
      else if (!snapshot.cancelled && current(id)?.state === 'cancelling') update(id, { state: 'running' })
    } catch (error) {
      if (current(id)?.state === 'cancelling') update(id, { state: 'running' })
      throw error
    }
  }

  function waitForSession(sessionId: string): Promise<DownloadOutcome[]> {
    return Promise.all(jobs.value.filter(job => job.sessionId === sessionId).flatMap(job => {
      const worker = workers.get(job.id)
      return worker ? [worker.done] : []
    }))
  }

  function clearFinished() {
    jobs.value = jobs.value.filter(downloadActive)
  }

  return { jobs, enqueue, cancel, waitForSession, clearFinished }
}

export const nativeDownloads = createDownloadQueue({
  id: newTauriDownloadId,
  watch: (id, fn) => appEvents.watchTransfer(id, fn),
  run: runTauriDownload,
  cancel: cancelTauriDownload,
})
