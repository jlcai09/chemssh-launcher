import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createHash } from 'node:crypto'
import { createServer } from 'node:net'
import { AppEvents } from '../src/appEvents.ts'

function frame(opcode, payload) {
  const length = payload.length
  let header
  if (length < 126) {
    header = Buffer.from([0x80 | opcode, length])
  } else if (length < 65536) {
    header = Buffer.alloc(4)
    header[0] = 0x80 | opcode
    header[1] = 126
    header.writeUInt16BE(length, 2)
  } else {
    header = Buffer.alloc(10)
    header[0] = 0x80 | opcode
    header[1] = 127
    header.writeBigUInt64BE(BigInt(length), 2)
  }
  return Buffer.concat([header, payload])
}

// Minimal websocket peer: enough of RFC 6455 to test the client's routing,
// reconnection and input framing without a third-party server.
class FakeSocketServer {
  constructor() {
    this.received = []
    this.connections = 0
    this.sockets = new Set()
    this.server = createServer(socket => this.accept(socket))
  }

  async listen() {
    await new Promise(resolve => this.server.listen(0, '127.0.0.1', resolve))
    this.port = this.server.address().port
    return `127.0.0.1:${this.port}`
  }

  accept(socket) {
    this.connections += 1
    this.sockets.add(socket)
    socket.on('close', () => this.sockets.delete(socket))
    let buffer = Buffer.alloc(0)
    let upgraded = false
    socket.on('data', chunk => {
      buffer = Buffer.concat([buffer, chunk])
      if (!upgraded) {
        const end = buffer.indexOf('\r\n\r\n')
        if (end < 0) return
        const request = buffer.subarray(0, end).toString('latin1')
        buffer = buffer.subarray(end + 4)
        const key = /sec-websocket-key:\s*(\S+)/i.exec(request)?.[1] || ''
        const accept = createHash('sha1').update(key + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest('base64')
        socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`)
        upgraded = true
      }
      for (;;) {
        if (buffer.length < 2) return
        const opcode = buffer[0] & 0x0f
        const masked = (buffer[1] & 0x80) !== 0
        let length = buffer[1] & 0x7f
        let offset = 2
        if (length === 126) {
          if (buffer.length < 4) return
          length = buffer.readUInt16BE(2)
          offset = 4
        } else if (length === 127) {
          if (buffer.length < 10) return
          length = Number(buffer.readBigUInt64BE(2))
          offset = 10
        }
        const mask = masked ? buffer.subarray(offset, offset + 4) : null
        if (masked) offset += 4
        if (buffer.length < offset + length) return
        const payload = Buffer.from(buffer.subarray(offset, offset + length))
        if (mask) for (let i = 0; i < payload.length; i += 1) payload[i] ^= mask[i % 4]
        buffer = buffer.subarray(offset + length)
        if (opcode === 8) {
          socket.end()
          return
        }
        if (opcode === 1) this.received.push(JSON.parse(payload.toString('utf8')))
      }
    })
  }

  send(message) {
    const payload = Buffer.from(JSON.stringify(message))
    for (const socket of this.sockets) socket.write(frame(1, payload))
  }

  drop() {
    for (const socket of this.sockets) socket.destroy()
  }

  async close() {
    for (const socket of this.sockets) socket.destroy()
    await new Promise(resolve => this.server.close(resolve))
  }
}

async function waitFor(predicate, timeoutMs = 2000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (predicate()) return
    await new Promise(resolve => setTimeout(resolve, 10))
  }
  throw new Error('condition not met in time')
}

function installWindow(host) {
  globalThis.window = {
    location: { protocol: 'http:', host },
    setTimeout: (...args) => setTimeout(...args),
    clearTimeout: (...args) => clearTimeout(...args)
  }
}

test('terminal watch decodes pushed output, sends input and reports exit', async () => {
  const server = new FakeSocketServer()
  const host = await server.listen()
  installWindow(host)
  const events = new AppEvents()
  const chunks = []
  let exited = false
  events.watchTerminal('session-1', {
    onData: bytes => chunks.push(new TextDecoder().decode(bytes)),
    onExit: () => { exited = true }
  })
  await waitFor(() => server.received.some(m => m.type === 'terminal:attach'))
  const attach = server.received.find(m => m.type === 'terminal:attach')
  assert.deepEqual(attach, { type: 'terminal:attach', session: 'session-1', since: 0 })

  server.send({ type: 'terminal:data', session: 'session-1', seq: 1, data: Buffer.from('hello ').toString('base64') })
  server.send({ type: 'terminal:data', session: 'session-1', seq: 2, data: Buffer.from('world').toString('base64') })
  await waitFor(() => chunks.join('') === 'hello world')

  events.sendTerminalInput('session-1', 'ls -la\r')
  await waitFor(() => server.received.some(m => m.type === 'terminal:input'))
  const input = server.received.find(m => m.type === 'terminal:input')
  assert.equal(Buffer.from(input.data, 'base64').toString('utf8'), 'ls -la\r')

  server.send({ type: 'terminal:exit', session: 'session-1' })
  await waitFor(() => exited)
  events.dispose()
  await server.close()
})

test('reconnect re-attaches from the last sequence number', async () => {
  const server = new FakeSocketServer()
  const host = await server.listen()
  installWindow(host)
  const events = new AppEvents()
  events.watchTerminal('session-2', { onData: () => {}, onExit: () => {} })
  await waitFor(() => server.received.some(m => m.type === 'terminal:attach' && m.session === 'session-2'))
  server.send({ type: 'terminal:data', session: 'session-2', seq: 7, data: Buffer.from('x').toString('base64') })
  // Give the client time to consume the frame before the transport drops.
  await new Promise(resolve => setTimeout(resolve, 80))
  server.drop()
  await waitFor(() => server.connections >= 2, 4000)
  await waitFor(() => server.received.some(m => m.type === 'terminal:attach' && m.session === 'session-2' && m.since === 7), 4000)
  events.dispose()
  await server.close()
})

test('transfer progress is delivered to every registered handler once', async () => {
  const server = new FakeSocketServer()
  const host = await server.listen()
  installWindow(host)
  const events = new AppEvents()
  const seen = []
  const stopA = events.watchTransfer('transfer-1', progress => seen.push(['a', progress.loaded]))
  const stopB = events.watchTransfer('transfer-1', progress => seen.push(['b', progress.loaded]))
  await waitFor(() => server.received.some(m => m.type === 'transfer:watch' && m.id === 'transfer-1'))
  server.send({ type: 'transfer:progress', id: 'transfer-1', progress: { loaded: 5, total: 10, done: false, cancelled: false, error: '', paused: false } })
  await waitFor(() => seen.length === 2)
  assert.deepEqual(seen, [['a', 5], ['b', 5]])
  stopA()
  stopB()
  await waitFor(() => server.received.some(m => m.type === 'transfer:unwatch' && m.id === 'transfer-1'))
  events.dispose()
  await server.close()
})
