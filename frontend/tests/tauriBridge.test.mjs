import test from 'node:test'
import assert from 'node:assert/strict'

import {
  acceptTerminalOutput,
  attachTauriTerminalSink,
  createSessionLogCursor,
  createTerminalInputQueue,
  decodeTerminalWsFrame,
  pickTauriDownloadTargets,
  runTauriDownload,
  invokeApi,
  subscribeTauriTransferProgress,
  parseTauriInvokeError,
  resolveTauriCommand,
  tauriFileIconURL,
} from '../src/tauriBridge.ts'

test('maps profile CRUD and secret-safe routes to Tauri commands', () => {
  assert.deepEqual(resolveTauriCommand('/api/profiles', 'GET'), {
    command: 'list_profiles',
  })
  assert.deepEqual(resolveTauriCommand('/api/profiles/p1', 'DELETE'), {
    command: 'delete_profile',
    args: { idOrName: 'p1' },
  })
  assert.deepEqual(resolveTauriCommand('/api/profiles/reorder', 'POST', { ids: ['p2', 'p1'] }), {
    command: 'reorder_profiles',
    args: { ids: ['p2', 'p1'] },
  })
  assert.deepEqual(
    resolveTauriCommand('/api/profiles/p1', 'PUT', {
      profile: { id: 'p1', name: 'example' },
      secrets: {
        password_action: 'replace',
        password: 'password',
        passphrase_action: 'clear',
        passphrase: '',
        security_token_action: 'keep',
        security_token: '',
      },
    }),
    {
      command: 'save_profile',
      args: {
        request: {
          profile: { id: 'p1', name: 'example' },
          password: 'password',
          clear_password: false,
          private_key_passphrase: null,
          clear_private_key_passphrase: true,
          security_token: null,
          clear_security_key: false,
          clear_security_token: false,
        },
      },
    },
  )
})

test('maps session, terminal, SFTP, local FS, and transfer routes to Tauri commands', () => {
  assert.deepEqual(
    resolveTauriCommand('/api/session/start', 'POST', { id: 'p1', accept_host_key: true }),
    {
      command: 'start_session',
      args: { profileId: 'p1', hostKeyPolicy: 'accept_new' },
    },
  )
  assert.deepEqual(resolveTauriCommand('/api/session/stop', 'POST', { id: 'p1' }), {
    command: 'pause_forwarding',
    args: { profileId: 'p1' },
  })
  assert.deepEqual(resolveTauriCommand('/api/session/stop-service', 'POST', { id: 'p1' }), {
    command: 'stop_session',
    args: { profileId: 'p1' },
  })
  assert.deepEqual(resolveTauriCommand('/api/session/activate', 'POST', { id: 'p1' }), {
    command: 'activate_session',
    args: { profileId: 'p1' },
  })
  assert.deepEqual(
    resolveTauriCommand('/api/session/shell/open', 'POST', {
      profile_id: 'p1',
      cols: 100,
      rows: 30,
    }),
    {
      command: 'open_terminal',
      args: { profileId: 'p1', cols: 100, rows: 30, hostKeyPolicy: null },
    },
  )
  assert.deepEqual(resolveTauriCommand('/api/session/shell/cwd?id=term-1', 'GET'), {
    command: 'get_terminal_cwd',
    args: { sessionId: 'term-1' },
  })
  assert.deepEqual(resolveTauriCommand('/api/sftp/list?session=p1&path=%2Fhome%2Fuser', 'GET'), {
    command: 'sftp_list',
    args: { profileId: 'p1', path: '/home/user' },
  })
  assert.deepEqual(
    resolveTauriCommand('/api/sftp/mkdirs', 'POST', { session: 'p1', paths: ['/home/user/a/b'] }),
    {
      command: 'sftp_mkdirs',
      args: { profileId: 'p1', paths: ['/home/user/a/b'] },
    },
  )
  assert.deepEqual(resolveTauriCommand('/api/transfer-control', 'POST', { id: 't1', action: 'pause' }), {
    command: 'control_transfer',
    args: { id: 't1', action: 'pause' },
  })
  assert.deepEqual(resolveTauriCommand('/api/local/home', 'GET'), {
    command: 'get_local_home',
  })
  assert.deepEqual(resolveTauriCommand('/api/backend/open-config-dir', 'POST'), {
    command: 'open_config_dir',
  })
  assert.deepEqual(resolveTauriCommand('/api/launcher-logs', 'GET'), {
    command: 'get_launcher_logs',
  })
  assert.deepEqual(resolveTauriCommand('/api/local/open-text', 'POST', { path: '/tmp/a.txt' }), {
    command: 'local_open_text_cmd',
    args: { path: '/tmp/a.txt' },
  })
  assert.deepEqual(
    resolveTauriCommand('/api/sftp/reconnect', 'POST', { session: 's1', accept_host_key: true }),
    {
      command: 'sftp_reconnect',
      args: { profileId: 's1', hostKeyPolicy: 'accept_new' },
    },
  )
  assert.deepEqual(
    resolveTauriCommand('/api/sftp/open', 'POST', { session: 's1', path: '/home/user/a.png' }),
    {
      command: 'sftp_open_file',
      args: { profileId: 's1', path: '/home/user/a.png', asText: false },
    },
  )
  assert.deepEqual(
    resolveTauriCommand('/api/sftp/open-text', 'POST', { session: 's1', path: '/home/user/a.txt' }),
    {
      command: 'sftp_open_file',
      args: { profileId: 's1', path: '/home/user/a.txt', asText: true },
    },
  )
  assert.deepEqual(resolveTauriCommand('/api/sftp/open-sync-events?after=12', 'GET'), {
    command: 'sftp_open_sync_events',
    args: { after: 12 },
  })
  assert.deepEqual(
    resolveTauriCommand('/api/sftp/server-copy-support?source=s1&target=s2', 'GET'),
    {
      command: 'sftp_server_copy_support',
      args: { source: 's1', target: 's2' },
    },
  )
  assert.deepEqual(
    resolveTauriCommand('/api/sftp/copy-on-server', 'POST', {
      source_session: 's1',
      target_session: 's2',
      target_path: '/home/user/dst',
      items: [{ source_path: '/home/user/a', relative_path: 'a' }],
    }),
    {
      command: 'sftp_copy_on_server',
      args: {
        sourceSession: 's1',
        targetSession: 's2',
        targetPath: '/home/user/dst',
        items: [{ source_path: '/home/user/a', relative_path: 'a' }],
      },
    },
  )
})

test('tauriFileIconURL encodes name, is_dir, and size parameters', () => {
  const url = tauriFileIconURL('script.py', false, 16)
  assert.match(url, /\/icon\?name=script\.py&is_dir=0&size=16$/)
  const dirUrl = tauriFileIconURL('folder', true, 32)
  assert.match(dirUrl, /\/icon\?name=folder&is_dir=1&size=32$/)
})

test('parseTauriInvokeError extracts transport_lost flag cleanly without breaking non-transport errors', () => {
  const lost = parseTauriInvokeError(
    JSON.stringify({ error: 'sftp error: list /home/user: session closed', transport_lost: true }),
  )
  assert.equal(lost.message, 'sftp error: list /home/user: session closed')
  assert.equal(lost.transportLost, true)
  assert.equal(lost.status, 500)

  const status = parseTauriInvokeError(JSON.stringify({ error: 'permission denied' }))
  assert.equal(status.message, 'permission denied')
  assert.equal(status.transportLost, false)

  const plain = parseTauriInvokeError('sftp error: no such file')
  assert.equal(plain.message, 'sftp error: no such file')
  assert.equal(plain.transportLost, false)
})

test('createTerminalInputQueue dispatches first key immediately and coalesces while in flight in FIFO order', async () => {
  const sent = []
  const resolvers = []
  const queue = createTerminalInputQueue((sessionId, data) => {
    sent.push({ sessionId, data })
    return new Promise(resolve => {
      resolvers.push(resolve)
    })
  })

  // First key dispatches synchronously with zero timer delay.
  queue.enqueue('s1', 'a')
  assert.equal(queue.hasInFlight('s1'), true)
  assert.deepEqual(sent, [{ sessionId: 's1', data: 'a' }])

  // Keys typed while first IPC is in flight coalesce into a single ordered follow-up call.
  queue.enqueue('s1', 'b')
  queue.enqueue('s1', 'c')
  queue.enqueue('s1', '\r')
  assert.equal(sent.length, 1)

  resolvers.shift()()
  await Promise.resolve()
  await Promise.resolve()

  assert.deepEqual(sent, [
    { sessionId: 's1', data: 'a' },
    { sessionId: 's1', data: 'bc\r' },
  ])

  resolvers.shift()()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(queue.hasInFlight('s1'), false)
})

function terminalFrame(tag, seq, value) {
  const data = new ArrayBuffer(tag === 2 ? 17 : tag === 0 ? 10 : 9)
  const view = new DataView(data)
  view.setUint8(0, tag)
  view.setBigUint64(1, BigInt(seq))
  if (tag === 2) view.setBigUint64(9, BigInt(value))
  if (tag === 0) view.setUint8(9, value)
  return data
}

test('decodeTerminalWsFrame decodes output, exit, and replay bounds and rejects invalid frames', () => {
  const outBuf = new ArrayBuffer(9 + 5)
  const outView = new Uint8Array(outBuf)
  const outDv = new DataView(outBuf)
  outView[0] = 0
  outDv.setBigUint64(1, 42n, false)
  outView.set(new TextEncoder().encode('hello'), 9)
  const outDecoded = decodeTerminalWsFrame(outBuf)
  assert.equal(outDecoded?.kind, 'output')
  assert.equal(outDecoded?.seq, 42)
  assert.equal(new TextDecoder().decode(outDecoded?.bytes), 'hello')

  const exitBuf = new ArrayBuffer(9)
  const exitView = new Uint8Array(exitBuf)
  const exitDv = new DataView(exitBuf)
  exitView[0] = 1
  exitDv.setBigUint64(1, 99n, false)
  assert.deepEqual(decodeTerminalWsFrame(exitBuf), { kind: 'exit', seq: 99 })

  assert.equal(decodeTerminalWsFrame(new ArrayBuffer(4)), null)
  const unknownBuf = new ArrayBuffer(9)
  new Uint8Array(unknownBuf)[0] = 9
  assert.equal(decodeTerminalWsFrame(unknownBuf), null)
  assert.deepEqual(decodeTerminalWsFrame(terminalFrame(2, 3, 99)), {
    kind: 'ready', lostThrough: 3, latestSeq: 99,
  })
  assert.equal(decodeTerminalWsFrame(terminalFrame(2, 12, 11)), null)
  assert.equal(decodeTerminalWsFrame(terminalFrame(1, 9007199254740992n)), null)
})

test('terminal output does not skip a sequence on either transport', () => {
  const received = []
  const cursor = { lastSeq: 0, exited: false }
  const sink = {
    onData: bytes => received.push(bytes[0]),
    onGap: () => received.push('gap'),
    onExit: () => received.push('exit'),
  }
  const output = seq => acceptTerminalOutput(cursor, seq, 'output', sink, () => Uint8Array.of(seq))
  assert.equal(output(2), 'gap')
  assert.equal(cursor.lastSeq, 0)
  assert.equal(output(1), 'accepted')
  assert.equal(output(2), 'accepted')
  assert.equal(output(2), 'duplicate')
  assert.equal(output(4), 'gap')
  assert.equal(acceptTerminalOutput(cursor, 3, 'exit', sink), 'accepted')
  assert.equal(output(4), 'duplicate')
  assert.deepEqual(received, [1, 2, 'exit'])
})

test('terminal repairs gaps with IPC, fences stale responses, and reconnects from its delivered cursor', async t => {
  let now = 10_000
  let nextTimer = 0
  const timers = new Map()
  const requests = []
  const polls = []
  const sockets = []
  t.mock.method(Date, 'now', () => now)
  const previousWindow = globalThis.window
  const previousWebSocket = globalThis.WebSocket
  globalThis.window = {
    setTimeout(callback, delay) {
      const id = ++nextTimer
      timers.set(id, { callback, due: now + delay })
      return id
    },
    clearTimeout: id => timers.delete(id),
    __TAURI_INTERNALS__: {
      async invoke(command, args) {
        if (command === 'open_terminal') return { id: 'terminal-reconnect-test', ws_url: 'ws://localhost/api/terminal-ws' }
        if (command === 'close_terminal') return
        assert.equal(command, 'poll_terminal_events')
        requests.push(args.cursors['terminal-reconnect-test'])
        return new Promise(resolve => polls.push(resolve))
      },
    },
  }
  globalThis.WebSocket = class {
    constructor(url) { this.url = url; sockets.push(this) }
    close() { this.closed = true }
    frame(tag, seq, value) { this.onmessage?.({ data: terminalFrame(tag, seq, value) }) }
  }
  const pump = (ms = 0) => {
    now += ms
    for (const [id, { callback, due }] of [...timers]) {
      if (due <= now && timers.delete(id)) callback()
    }
  }
  const answer = async (lost_through, latest_seq, events) => {
    assert.ok(polls.length, 'expected a pending replay call')
    polls.shift()({ 'terminal-reconnect-test': { lost_through, latest_seq, events } })
    await new Promise(setImmediate)
  }
  const received = []
  try {
    await invokeApi('/api/session/shell/open', { method: 'POST', body: JSON.stringify({ profile_id: 'p1' }) })
    const detach = attachTauriTerminalSink('terminal-reconnect-test', {
      onData: bytes => received.push(bytes[0]),
      onGap: () => received.push('gap'),
      onExit: () => received.push('exit'),
    })
    const [first] = sockets
    pump()
    assert.deepEqual(requests.at(-1), { since: 0, metadata_only: false })
    first.frame(0, 1, 1)
    assert.deepEqual(received, [], 'backlog is staged until Ready')
    first.frame(2, 0, 1)
    await answer(8, 8, []) // Late IPC reply must not authorize a loss after Ready.
    assert.deepEqual(received, [1])

    first.frame(0, 3, 3)
    assert.equal(first.closed, true)
    pump()
    assert.deepEqual(requests.at(-1), { since: 1, metadata_only: false })
    await answer(0, 4, [
      { kind: 'output', seq: 2, data_b64: 'Ag==' },
      { kind: 'output', seq: 3, data_b64: 'Aw==' },
    ])
    assert.deepEqual(received, [1, 2, 3])
    pump(100)
    assert.equal(sockets.length, 1, 'honors reconnect backoff')
    assert.deepEqual(requests.at(-1), { since: 3, metadata_only: false })
    pump(150)
    assert.equal(sockets.length, 2, 'can reconnect while a remaining IPC replay page is in flight')
    const second = sockets[1]
    assert.equal(second.url.searchParams.get('since'), '3')
    assert.equal(second.url.searchParams.get('attempt'), '2')
    second.frame(0, 4, 4)
    await answer(0, 4, [{ kind: 'output', seq: 4, data_b64: 'BA==' }])
    second.frame(2, 0, 4) // Replay overlap is deduplicated.
    assert.deepEqual(received, [1, 2, 3, 4])

    pump(1000)
    assert.deepEqual(requests.at(-1), { since: 4, metadata_only: true })
    await answer(0, 4, [])
    const staleClose = second.onclose
    second.onclose()
    staleClose() // The close/error pair must schedule only one recovery.
    pump()
    await answer(5, 6, [{ kind: 'output', seq: 6, data_b64: 'Bg==' }])
    assert.deepEqual(received, [1, 2, 3, 4, 'gap', 6])
    pump(500)
    const third = sockets[2]
    assert.equal(third.url.searchParams.get('since'), '6')
    third.frame(2, 5, 6)
    third.frame(1, 7)
    await answer(5, 6, [])
    assert.deepEqual(received, [1, 2, 3, 4, 'gap', 6, 'exit'])
    detach()
    pump(10000)
    assert.equal(sockets.length, 3)
    assert.equal(timers.size, 0)
  } finally {
    await invokeApi('/api/session/shell/close', { method: 'POST', body: JSON.stringify({ session: 'terminal-reconnect-test' }) })
    if (previousWindow === undefined) delete globalThis.window
    else globalThis.window = previousWindow
    globalThis.WebSocket = previousWebSocket
  }
})

test('native save cancellation starts no SFTP transfer and a chosen path uses IPC', async () => {
  const calls = []
  let destination = null
  globalThis.window = {
    __TAURI_INTERNALS__: {
      invoke: async (command, args) => {
        calls.push({ command, args })
        if (command === 'pick_sftp_download_path') return destination
      },
    },
  }
  try {
    const sources = [{ path: '/home/user/report.txt' }]
    assert.equal(await pickTauriDownloadTargets(sources), null)
    assert.deepEqual(calls, [{ command: 'pick_sftp_download_path', args: { suggestedName: 'report.txt' } }])
    destination = '/home/user/Downloads/report.txt'
    assert.deepEqual(await pickTauriDownloadTargets(sources), [{ local_path: destination, renamed: false }])
    await runTauriDownload('session', sources[0].path, destination, 'download-1', true)
    assert.deepEqual(calls.at(-1), { command: 'sftp_download_local', args: {
      profileId: 'session', remotePath: sources[0].path, localPath: destination, transferId: 'download-1', overwrite: true,
    } })
    await invokeApi('/api/transfer-control', { method: 'POST', body: JSON.stringify({ id: 'download-test', action: 'reset' }) })
    assert.deepEqual(calls.at(-1), { command: 'control_transfer', args: { id: 'download-test', action: 'reset' } })
  } finally {
    delete globalThis.window
  }
})

test('native batch picks one folder and routes file downloads without the HTTP attachment', async () => {
  const calls = []
  const targets = [{ local_path: '/downloads/a.txt', renamed: false }, { local_path: '/downloads/a (1).txt', renamed: true }]
  globalThis.window = { __TAURI_INTERNALS__: {
    invoke: async (command, args) => { calls.push({ command, args }); return targets },
  } }
  try {
    assert.deepEqual(await pickTauriDownloadTargets([{ path: '/a/a.txt' }, { path: '/b/a.txt' }]), targets)
    assert.deepEqual(calls, [{ command: 'pick_sftp_download_dir', args: { suggestedNames: ['a.txt', 'a.txt'] } }])
    assert.equal(resolveTauriCommand('/api/sftp/download', 'GET'), null)
    assert.deepEqual(resolveTauriCommand('/api/sftp/download-local', 'POST', {
      session: 'session', path: '/b/a.txt', local_path: '/downloads', relative_path: 'a.txt', transfer_id: 'copy-1',
    }), { command: 'sftp_download_local', args: {
      profileId: 'session', remotePath: '/b/a.txt', localPath: '/downloads/a.txt', transferId: 'copy-1',
    } })
  } finally {
    delete globalThis.window
  }
})

test('transfer progress stop waits for registration and ignores subsequent snapshots', async () => {
  const calls = []
  const received = []
  let callback
  let registered
  const start = new Promise(resolve => { registered = resolve })
  globalThis.window = {
    __TAURI_INTERNALS__: {
      transformCallback: fn => { callback = fn; return 1 },
      invoke: async (command, args) => {
        calls.push({ command, args })
        if (command === 'stream_transfer_progress') await start
      },
    },
  }
  try {
    const stop = subscribeTauriTransferProgress(items => received.push(items))
    callback({ index: 0, message: [] })
    stop()
    stop()
    callback({ index: 1, message: [] })
    assert.equal(calls.length, 1)
    registered()
    await new Promise(resolve => setImmediate(resolve))
    assert.deepEqual(received, [[]])
    assert.deepEqual(calls.at(-1), {
      command: 'stop_transfer_progress', args: { subscriptionId: calls[0].args.subscriptionId },
    })
  } finally {
    delete globalThis.window
  }
})

test('createSessionLogCursor deduplicates batches and replaces a reset snapshot', () => {
  const received = []
  const cursor = createSessionLogCursor((lines, isReplay) => {
    received.push({ lines, isReplay })
  })

  // Initial poll replay arrives first
  assert.equal(cursor.apply(['start requested', 'SSH OK'], true, 2), true)
  assert.equal(cursor.lastSeq(), 2)

  // Stale channel replay with same seq is ignored (does not duplicate or clear)
  assert.equal(cursor.apply(['start requested', 'SSH OK'], true, 2), false)

  // Live line arrives via channel push
  assert.equal(cursor.apply(['ready'], false, 3), true)
  assert.equal(cursor.lastSeq(), 3)

  // Duplicate poll with seq 3 is ignored
  assert.equal(cursor.apply(['ready'], false, 3), false)

  // Next line arrives via poll fallback
  assert.equal(cursor.apply(['identity OK'], false, 4), true)
  assert.equal(cursor.lastSeq(), 4)

  // Ring-buffer reset with newer seq replaces cleanly
  assert.equal(cursor.apply(['tail-1', 'tail-2'], true, 2500), true)
  assert.equal(cursor.lastSeq(), 2500)

  assert.deepEqual(received, [
    { lines: ['start requested', 'SSH OK'], isReplay: true },
    { lines: ['ready'], isReplay: false },
    { lines: ['identity OK'], isReplay: false },
    { lines: ['tail-1', 'tail-2'], isReplay: true },
  ])
})

test('maps installer routes to Tauri commands', () => {
  assert.deepEqual(
    resolveTauriCommand('/api/installer/check', 'POST', {
      profile_id: 'p1',
      options: { install_dir: '~/.chemssh' },
      accept_host_key: true,
    }),
    {
      command: 'installer_check',
      args: { profileId: 'p1', options: { install_dir: '~/.chemssh' }, hostKeyPolicy: 'accept_new' },
    },
  )
  assert.deepEqual(resolveTauriCommand('/api/installer/run', 'POST', { profile_id: 'p1', mode: 'repair' }), {
    command: 'installer_run',
    args: { profileId: 'p1', options: {}, mode: 'repair', hostKeyPolicy: null },
  })
  assert.deepEqual(resolveTauriCommand('/api/installer/status?profile_id=p1&since=7', 'GET'), {
    command: 'installer_status',
    args: { profileId: 'p1', since: 7 },
  })
  assert.deepEqual(resolveTauriCommand('/api/installer/apply', 'POST', { profile_id: 'p1' }), {
    command: 'installer_apply_profile',
    args: { profileId: 'p1', options: {} },
  })
})

test('maps compress routes without pre-locking colliding names and forwards backend archive name', async () => {
  assert.deepEqual(
    resolveTauriCommand('/api/sftp/compress', 'POST', {
      session: 's1',
      target_directory: '/home/user/project',
      paths: ['/home/user/project/a.txt', '/home/user/project/b.txt'],
      format: 'tar.zst',
    }),
    {
      command: 'sftp_compress',
      args: {
        profileId: 's1',
        cwd: '/home/user/project',
        format: 'tar.zst',
        archiveName: null,
        entries: ['a.txt', 'b.txt'],
      },
    },
  )
  assert.deepEqual(
    resolveTauriCommand('/api/local/compress', 'POST', {
      target_directory: '/home/user/project',
      paths: ['/home/user/project/a.txt', '/home/user/project/b.txt'],
      format: 'tar.zst',
    }),
    {
      command: 'local_compress',
      args: {
        cwd: '/home/user/project',
        format: 'tar.zst',
        archiveName: null,
        entries: ['a.txt', 'b.txt'],
      },
    },
  )

  const previousWindow = globalThis.window
  globalThis.window = {
    __TAURI_INTERNALS__: {
      invoke: async command => {
        if (command === 'sftp_compress') return 'archive_new.tar.zst'
        if (command === 'local_compress') return 'archive_new_new.tar.zst'
        return null
      },
    },
  }
  try {
    const remoteRes = await invokeApi('/api/sftp/compress', {
      method: 'POST',
      body: JSON.stringify({
        session: 's1',
        target_directory: '/home/user/project',
        paths: ['/home/user/project/a.txt', '/home/user/project/b.txt'],
        format: 'tar.zst',
      }),
    })
    assert.deepEqual(remoteRes, { ok: true, archive: 'archive_new.tar.zst' })

    const localRes = await invokeApi('/api/local/compress', {
      method: 'POST',
      body: JSON.stringify({
        target_directory: '/home/user/project',
        paths: ['/home/user/project/a.txt', '/home/user/project/b.txt'],
        format: 'tar.zst',
      }),
    })
    assert.deepEqual(localRes, { ok: true, archive: 'archive_new_new.tar.zst' })
  } finally {
    if (previousWindow === undefined) delete globalThis.window
    else globalThis.window = previousWindow
  }
})
