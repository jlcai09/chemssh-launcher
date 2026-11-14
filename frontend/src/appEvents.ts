// One multiplexed channel carries terminal output, terminal input and transfer
// progress for every launcher page. Keeping these off HTTP means the browser's
// six-per-origin HTTP connection budget stays free for short requests instead
// of being consumed by one stream per terminal tab.

import type { TransferProgressSnapshot } from './transferProgress'

interface EventsFrame {
  type: string
  session?: string
  seq?: number
  data?: string
  id?: string
  progress?: TransferProgressSnapshot
}

export interface TerminalSink {
  onData: (bytes: Uint8Array) => void
  onExit: () => void
  onGap?: () => void
}

interface TerminalWatch {
  sink: TerminalSink
  lastSeq: number
}

const RECONNECT_MIN_MS = 300
const RECONNECT_MAX_MS = 3000

function binaryFromBase64(value: string): Uint8Array {
  const binary = atob(value)
  const bytes = new Uint8Array(binary.length)
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index)
  }
  return bytes
}

function base64FromText(value: string): string {
  const bytes = new TextEncoder().encode(value)
  let binary = ''
  const chunkSize = 8192
  for (let offset = 0; offset < bytes.length; offset += chunkSize) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + chunkSize))
  }
  return btoa(binary)
}

export class AppEvents {
  private socket: WebSocket | null = null
  private open = false
  private disposed = false
  private reconnectTimer: number | null = null
  private attempts = 0
  private terminals = new Map<string, TerminalWatch>()
  private transfers = new Map<string, Set<(progress: TransferProgressSnapshot) => void>>()

  watchTerminal(session: string, sink: TerminalSink): () => void {
    if (!session) return () => {}
    this.terminals.set(session, { sink, lastSeq: 0 })
    this.ensureConnected()
    this.send({ type: 'terminal:attach', session, since: 0 })
    return () => {
      if (!this.terminals.has(session)) return
      this.terminals.delete(session)
      this.send({ type: 'terminal:detach', session })
    }
  }

  sendTerminalInput(session: string, text: string): void {
    if (!session || !text) return
    this.sendTerminalBytes(session, base64FromText(text))
  }

  watchTransfer(id: string, handler: (progress: TransferProgressSnapshot) => void): () => void {
    if (!id) return () => {}
    let handlers = this.transfers.get(id)
    if (!handlers) {
      handlers = new Set()
      this.transfers.set(id, handlers)
      this.ensureConnected()
      this.send({ type: 'transfer:watch', id })
    }
    handlers.add(handler)
    return () => {
      const current = this.transfers.get(id)
      if (!current) return
      current.delete(handler)
      if (current.size === 0) {
        this.transfers.delete(id)
        this.send({ type: 'transfer:unwatch', id })
      }
    }
  }

  /** Stops the channel and every watch. Used by tests and page teardown. */
  dispose(): void {
    this.disposed = true
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    this.terminals.clear()
    this.transfers.clear()
    const socket = this.socket
    this.socket = null
    this.open = false
    socket?.close()
  }

  private sendTerminalBytes(session: string, data: string): void {
    if (this.open && this.socket) {
      this.socket.send(JSON.stringify({ type: 'terminal:input', session, data }))
      return
    }
    // Replaying keystrokes after a reconnect would type into a shell whose
    // state the user cannot see any more, so input typed while the channel is
    // down is dropped instead of queued.
    this.ensureConnected()
  }

  private send(payload: Record<string, unknown>): void {
    if (this.open && this.socket) {
      this.socket.send(JSON.stringify(payload))
      return
    }
    this.ensureConnected()
  }

  private ensureConnected(): void {
    if (this.disposed || this.socket) return
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const socket = new WebSocket(`${protocol}//${window.location.host}/api/events`)
    this.socket = socket
    socket.onopen = () => {
      if (this.socket !== socket) return
      this.open = true
      this.attempts = 0
      for (const [session, watch] of this.terminals) {
        socket.send(JSON.stringify({ type: 'terminal:attach', session, since: watch.lastSeq }))
      }
      for (const id of this.transfers.keys()) {
        socket.send(JSON.stringify({ type: 'transfer:watch', id }))
      }
    }
    socket.onmessage = event => {
      if (this.socket !== socket) return
      this.handleFrame(event.data)
    }
    socket.onclose = () => {
      if (this.socket !== socket) return
      this.open = false
      this.socket = null
      this.scheduleReconnect()
    }
    socket.onerror = () => {
      // onclose follows; the reconnect is scheduled there.
    }
  }

  private scheduleReconnect(): void {
    if (this.disposed || this.reconnectTimer !== null) return
    const delay = Math.min(RECONNECT_MAX_MS, RECONNECT_MIN_MS * 2 ** this.attempts)
    this.attempts += 1
    this.reconnectTimer = window.setTimeout(() => {
      this.reconnectTimer = null
      this.ensureConnected()
    }, delay)
  }

  private handleFrame(raw: unknown): void {
    if (typeof raw !== 'string') return
    let frame: EventsFrame
    try {
      frame = JSON.parse(raw) as EventsFrame
    } catch {
      return
    }
    switch (frame.type) {
      case 'terminal:data': {
        const session = frame.session || ''
        const watch = this.terminals.get(session)
        if (!watch || !frame.data) return
        if (typeof frame.seq === 'number') watch.lastSeq = Math.max(watch.lastSeq, frame.seq)
        watch.sink.onData(binaryFromBase64(frame.data))
        return
      }
      case 'terminal:gap': {
        this.terminals.get(frame.session || '')?.sink.onGap?.()
        return
      }
      case 'terminal:exit': {
        const session = frame.session || ''
        const watch = this.terminals.get(session)
        if (!watch) return
        this.terminals.delete(session)
        watch.sink.onExit()
        return
      }
      case 'transfer:progress': {
        const handlers = this.transfers.get(frame.id || '')
        if (!handlers || !frame.progress) return
        for (const handler of handlers) handler(frame.progress)
        return
      }
    }
  }
}

export const appEvents = new AppEvents()
