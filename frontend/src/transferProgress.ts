// Path-based transfers start asynchronously on the backend, so their HTTP
// response only means "accepted". Completion, cancellation and failure all
// arrive as pushed progress events over the shared app events channel.

export interface TransferProgressSnapshot {
  loaded: number
  total: number
  done: boolean
  cancelled: boolean
  error: string
  paused: boolean
  retrying?: boolean
  attempt?: number
  max_attempts?: number
}

export interface TransferProgressWaiter {
  /** Resolves on success; rejects on cancellation or failure. */
  done: Promise<void>
  stop: () => void
}

export function waitForTransferProgress(
  subscribe: (handler: (progress: TransferProgressSnapshot) => void) => () => void,
  onProgress: (progress: TransferProgressSnapshot) => void,
  cancelledMessage: () => string
): TransferProgressWaiter {
  let stopped = false
  let unsubscribe: (() => void) | null = null
  let resolveDone: () => void = () => {}
  let rejectDone: (error: Error) => void = () => {}
  const done = new Promise<void>((resolve, reject) => {
    resolveDone = resolve
    rejectDone = reject
  })
  // The caller may stop before a terminal state (start request failed); keep
  // that rejection from surfacing as an unhandled promise.
  done.catch(() => undefined)

  const stop = () => {
    if (stopped) return
    stopped = true
    unsubscribe?.()
    unsubscribe = null
  }

  const handle = (progress: TransferProgressSnapshot) => {
    if (stopped) return
    onProgress(progress)
    if (progress.cancelled) {
      stop()
      rejectDone(new Error(progress.error || cancelledMessage()))
      return
    }
    if (progress.done) {
      stop()
      if (progress.error) rejectDone(new Error(progress.error))
      else resolveDone()
    }
  }

  unsubscribe = subscribe(handle)
  if (stopped) {
    // A synchronous terminal snapshot settled before the subscription handle
    // was returned.
    const pending = unsubscribe
    unsubscribe = null
    pending?.()
  }
  return { done, stop }
}
