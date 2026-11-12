import assert from 'node:assert/strict'
import { test } from 'node:test'
import { isTerminalDirectory, terminalUploadDirectory } from '../src/terminalDirectory.ts'

const unavailable = 'terminal directory unavailable'

test('drop reads actual shell cwd instead of cached startup directory', async () => {
  const actual = '/home/user/project/中文 space'
  assert.equal(await terminalUploadDirectory('/home/user', async () => ({ path: actual }), unavailable), actual)
  assert.equal(await terminalUploadDirectory('', async () => ({ path: actual }), unavailable), actual)
})

test('directory changes and separate tabs resolve independently', async () => {
  let current = '/home/user/one'
  const query = async () => ({ path: current })
  assert.equal(await terminalUploadDirectory('/home/user', query, unavailable), current)
  current = '/home/user/two'
  const [first, second] = await Promise.all([
    terminalUploadDirectory('/home/user/one', query, unavailable),
    terminalUploadDirectory('/home/user', async () => ({ path: '/home/user/other' }), unavailable)
  ])
  assert.equal(first, '/home/user/two')
  assert.equal(second, '/home/user/other')
})

test('hosts without procfs retain a known OSC directory', async () => {
  const known = '/home/user/project'
  assert.equal(await terminalUploadDirectory(known, async () => { throw new Error('unsupported') }, unavailable), known)
})

test('unknown cwd never falls back to SFTP home or dot', async () => {
  for (const known of ['', '.', 'relative', '/bad\npath']) {
    for (const response of [{}, { path: '.' }, { path: '/bad\0path' }]) {
      await assert.rejects(terminalUploadDirectory(known, async () => response, unavailable), { message: unavailable })
    }
    await assert.rejects(terminalUploadDirectory(known, async () => { throw new Error('no procfs') }, unavailable), { message: unavailable })
  }
})

test('valid remote paths preserve spaces, quotes, Unicode and root', async () => {
  for (const path of ['/', '/home/user/中文', "/home/user/quote'", '/home/user/ space ']) {
    assert.equal(isTerminalDirectory(path), true)
    assert.equal(await terminalUploadDirectory('', async () => ({ path }), unavailable), path)
  }
})
