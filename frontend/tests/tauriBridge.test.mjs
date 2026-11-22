import test from 'node:test'
import assert from 'node:assert/strict'

import { resolveTauriCommand } from '../src/tauriBridge.ts'

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
})
