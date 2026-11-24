import test from 'node:test'
import assert from 'node:assert/strict'

import {
  createTerminalInputQueue,
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
})
