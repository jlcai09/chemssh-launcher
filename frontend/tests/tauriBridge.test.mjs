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
})

test('maps session, SFTP, local FS, and transfer routes to Tauri commands', () => {
  assert.deepEqual(resolveTauriCommand('/api/session/start', 'POST', { profile_id: 'p1' }), {
    command: 'start_session',
    args: { profileId: 'p1', hostKeyPolicy: null },
  })
  assert.deepEqual(resolveTauriCommand('/api/sftp/list?profile_id=p1&path=%2Fhome%2Fuser', 'GET'), {
    command: 'sftp_list',
    args: { profileId: 'p1', path: '/home/user' },
  })
  assert.deepEqual(resolveTauriCommand('/api/transfer-control', 'POST', { id: 't1', action: 'pause' }), {
    command: 'control_transfer',
    args: { id: 't1', action: 'pause' },
  })
  assert.deepEqual(resolveTauriCommand('/api/local/home', 'GET'), {
    command: 'get_local_home',
  })
})
