import assert from 'node:assert/strict'
import { test } from 'node:test'
import { AppEvents } from '../src/appEvents.ts'
import { nextTerminalFlowControl } from '../src/terminalFlowControl.ts'

test('only XOFF/XON bytes change the last sent flow-control request', () => {
  for (const current of ['none', 'pause-requested', 'resume-requested']) {
    for (const input of ['', 'ls -la\r', 'Ctrl+S Ctrl+Q', '\x03', '\x1b[1;5S', '中文']) {
      assert.equal(nextTerminalFlowControl(current, input), current)
    }
    assert.equal(nextTerminalFlowControl(current, '\x13'), 'pause-requested')
    assert.equal(nextTerminalFlowControl(current, '\x11'), 'resume-requested')
  }
})

test('coalesced or pasted input uses the last flow-control byte in order', () => {
  assert.equal(nextTerminalFlowControl('none', 'a\x13b\x11c'), 'resume-requested')
  assert.equal(nextTerminalFlowControl('none', 'a\x11b\x13c'), 'pause-requested')
  assert.equal(nextTerminalFlowControl('none', '\x13\x13'), 'pause-requested')
  assert.equal(nextTerminalFlowControl('none', '\x11\x11'), 'resume-requested')
})

test('each tab retains its own last request, with no timeout or output inference', () => {
  let first = 'none'
  let second = 'none'
  first = nextTerminalFlowControl(first, '\x13')
  second = nextTerminalFlowControl(second, 'echo hello\r')
  assert.equal(first, 'pause-requested')
  assert.equal(second, 'none')
  second = nextTerminalFlowControl(second, '\x11')
  assert.equal(first, 'pause-requested')
  assert.equal(second, 'resume-requested')
})

test('only input handed to an OPEN socket is recorded, and dropped controls are never replayed', () => {
  const savedWindow = globalThis.window
  const savedWebSocket = globalThis.WebSocket
  const sockets = []
  class FakeWebSocket {
    static OPEN = 1
    readyState = 0
    sent = []
    constructor() { sockets.push(this) }
    send(frame) { this.sent.push(JSON.parse(frame)) }
    open() { this.readyState = 1; this.onopen?.() }
    close() { this.readyState = 3; this.onclose?.() }
  }
  globalThis.window = {
    location: { protocol: 'http:', host: 'localhost' },
    setTimeout: (...args) => setTimeout(...args),
    clearTimeout: id => clearTimeout(id)
  }
  globalThis.WebSocket = FakeWebSocket
  const events = new AppEvents()
  try {
    let state = 'none'
    const send = input => {
      const accepted = events.sendTerminalInput('session-1', input)
      if (accepted) state = nextTerminalFlowControl(state, input)
      return accepted
    }
    assert.equal(events.sendTerminalInput('', '\x13'), false)
    assert.equal(send(''), false)
    assert.equal(sockets.length, 0)
    assert.equal(send('\x13'), false) // CONNECTING, not sent
    assert.equal(state, 'none')
    sockets[0].open()
    assert.equal(sockets[0].sent.length, 0)
    assert.equal(send('\x13'), true)
    assert.equal(state, 'pause-requested')
    assert.deepEqual(sockets[0].sent[0], { type: 'terminal:input', session: 'session-1', data: 'Ew==' })

    sockets[0].readyState = 2 // Cached open flag is stale until onclose fires.
    assert.equal(send('\x11'), false)
    assert.equal(state, 'pause-requested')
    sockets[0].close()
    assert.equal(send('\x11'), false) // CLOSED / reconnect pending
    assert.equal(state, 'pause-requested')
    sockets[1].open()
    assert.equal(sockets[1].sent.length, 0) // Never replay XON/XOFF.
    assert.equal(send('\x11'), true)
    assert.equal(state, 'resume-requested')
    assert.equal(sockets[1].sent[0].data, 'EQ==')

    sockets[1].send = () => { throw new Error('send failed') }
    assert.throws(() => send('\x13'), /send failed/)
    assert.equal(state, 'resume-requested')
    events.dispose()
    assert.equal(send('\x13'), false)
  } finally {
    events.dispose()
    globalThis.window = savedWindow
    globalThis.WebSocket = savedWebSocket
  }
})
