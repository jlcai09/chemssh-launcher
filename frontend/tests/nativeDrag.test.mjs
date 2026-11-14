import assert from 'node:assert/strict'
import { test } from 'node:test'
import { cancelNativeDragJob, controlNativeDragJob, dragOutFiles, mergeNativeDragRows, nativeDragCanControl, nativeDragHost, nativeDragTransfer, nativeDragTransferRoots, nativeDragTransferUpdates, retainTransferHistory, subscribeNativeDragJobs } from '../src/nativeDrag.ts'

test('large ordinary transfer batches retain active native jobs and only trim finished history', () => {
  const native = { id: 'native', status: 'running', paused: true }
  const ordinary = Array.from({ length: 350 }, (_, i) => ({ id: `ordinary-${i}`, status: 'running' }))
  const finished = Array.from({ length: 350 }, (_, i) => ({ id: `done-${i}`, status: 'done' }))
  const kept = retainTransferHistory([...ordinary, ...finished, native])
  assert.equal(kept.filter(item => item.status === 'running').length, 351)
  assert.equal(kept.filter(item => item.status === 'done').length, 300)
  assert.equal(kept.at(-1), native)
})

test('native row updates suppress explicit clears but recover missing active rows', () => {
  const revisions = new Map()
  const cleared = new Set()
  const job = { id: 'job', session: 'session', paths: ['/home/user/file'], state: 'caching', revision: 2, createdAt: 10 }
  assert.equal(nativeDragTransferUpdates([{ ...job, state: 'queued', revision: 1 }], revisions, cleared).length, 0)
  assert.equal(nativeDragTransferUpdates([job], revisions, cleared).length, 1)
  assert.equal(nativeDragTransferUpdates([job], revisions, cleared).length, 0)
  // No explicit clear: the next update can restore a missing active row.
  assert.equal(nativeDragTransferUpdates([{ ...job, revision: 3 }], revisions, cleared).length, 1)
  assert.equal(nativeDragTransferUpdates([{ ...job, state: 'done', revision: 4 }], revisions, cleared).length, 1)
  cleared.add(job.id)
  assert.equal(nativeDragTransferUpdates([{ ...job, state: 'done', revision: 5 }], revisions, cleared).length, 0)
  assert.equal(nativeDragTransferUpdates([{ ...job, revision: 3 }], revisions, cleared).length, 0)
  assert.equal(nativeDragTransferUpdates([{ ...job, id: 'internal', state: 'cancelled' }], revisions, cleared).length, 0)
})

test('native task controls use the owning top host and only cancel has a legacy fallback', async () => {
  const host = new FakeWindow()
  const frame = new FakeWindow()
  frame.top = host
  host.chemsshDragOutFiles = () => Promise.resolve()
  const calls = []
  host.chemsshControlNativeDragJob = function(id, action) {
    assert.equal(this, host)
    calls.push([id, action])
    return Promise.resolve(action !== 'resume')
  }
  assert.equal(await controlNativeDragJob('job', 'pause', frame), true)
  assert.equal(await controlNativeDragJob('job', 'resume', frame), false)
  assert.equal(await cancelNativeDragJob('job', frame), true)
  assert.deepEqual(calls, [['job', 'pause'], ['job', 'resume'], ['job', 'cancel']])
  delete host.chemsshControlNativeDragJob
  host.chemsshCancelNativeDragJob = id => Promise.resolve(id === 'job')
  assert.equal(await controlNativeDragJob('job', 'pause', frame), false)
  assert.equal(await cancelNativeDragJob('job', frame), true)
})

test('native job projection preserves byte progress, paused state and terminal time in transfer rows', () => {
  const job = { id: 'job', session: 'session', paths: ['/home/user/file'], state: 'caching', revision: 1, createdAt: 10, loaded: 25, total: 100, paused: true }
  const paused = nativeDragTransfer(job)
  assert.equal(paused.nativeDragID, 'job')
  assert.equal(paused.status, 'running')
  assert.equal(paused.paused, true)
  assert.equal(paused.loaded, 25)
  assert.equal(paused.total, 100)
  assert.equal(paused.endedAt, null)
  const delivery = nativeDragTransfer({ ...job, state: 'delivering', loaded: 100, paused: false })
  assert.equal(delivery.status, 'running')
  assert.equal(nativeDragCanControl(delivery.nativeDragState), false)
  assert.equal(nativeDragCanControl('accepted'), true)
  assert.equal(nativeDragCanControl('caching'), true)
  const done = nativeDragTransfer({ ...job, state: 'done', endedAt: 50 })
  assert.equal(done.status, 'done')
  assert.equal(done.paused, false)
  assert.equal(done.endedAt, 50)
  assert.equal(nativeDragTransfer({ ...job, state: 'error', error: 'failed' }).error, 'failed')
  const cancelled = nativeDragTransfer({ ...job, state: 'cancelled' })
  assert.equal(cancelled.status, 'error')
  assert.equal(cancelled.cancelled, true)
})

test('background jobs survive gesture completion and merge late snapshots by revision', async () => {
  const host = new FakeWindow()
  const frame = new FakeWindow()
  frame.top = host
  let resolveSnapshot, payload
  host.chemsshNativeDragJobs = () => new Promise(resolve => { resolveSnapshot = resolve })
  host.chemsshDragOutFiles = json => { payload = JSON.parse(json); return Promise.resolve() }
  const updates = []
  const stop = subscribeNativeDragJobs(jobs => updates.push(jobs), frame)
  const gesture = dragOutFiles(request, frame)
  const emit = job => host.dispatchEvent(new CustomEvent('chemssh-native-drag-job', { detail: job }))
  const base = { id: payload.id, session: 'session', paths: ['/home/user/file'], createdAt: 1 }
  emit({ ...base, state: 'caching', revision: 3 })
  host.finish({ id: payload.id, status: 'pending', ok: true })
  await gesture
  resolveSnapshot([{ ...base, state: 'accepted', revision: 2 }])
  await Promise.resolve()
  assert.equal(updates.at(-1)[0].state, 'caching')
  emit({ ...base, state: 'delivering', revision: 4 })
  assert.equal(updates.at(-1)[0].state, 'delivering')
  emit({ ...base, state: 'done', revision: 5 })
  assert.equal(updates.at(-1)[0].state, 'done')
  stop()
  emit({ ...base, state: 'error', revision: 6 })
  assert.equal(updates.at(-1)[0].state, 'done')
  assert.equal(host.listeners.get('chemssh-native-drag-job').size, 0)
})

test('new file page restores background jobs and cancellation calls the top host', async () => {
  const host = new FakeWindow()
  const frame = new FakeWindow()
  frame.top = host
  host.chemsshDragOutFiles = () => Promise.resolve()
  const job = { id: 'background', paths: ['/home/user/file'], session: 'session', state: 'caching', revision: 10, createdAt: 1 }
  host.chemsshNativeDragJobs = () => Promise.resolve([job])
  let cancelled
  host.chemsshCancelNativeDragJob = function(id) { assert.equal(this, host); cancelled = id; return Promise.resolve(true) }
  let jobs
  subscribeNativeDragJobs(value => { jobs = value }, frame)
  await Promise.resolve()
  assert.deepEqual(jobs, [job])
  assert.equal(await cancelNativeDragJob(job.id, frame), true)
  assert.equal(cancelled, job.id)
  frame.dispatchEvent(new Event('pagehide'))
  assert.equal(host.listeners.get('chemssh-native-drag-job').size, 0)
})

test('native drag folders project into collapsible path groups with per-file byte progress', () => {
  const job = {
    id: 'grouped', session: 'session', paths: ['/data/compare', '/data/readme.txt'],
    state: 'caching', revision: 8, createdAt: 10, loaded: 9, total: 20,
    files: [
      { path: '/data/compare', kind: 'folder', loaded: 0, total: 0, state: 'caching' },
      { path: '/data/compare/S/is.xsd', kind: 'file', loaded: 7, total: 10, state: 'caching' },
      { path: '/data/compare/P/is.xsd', kind: 'file', loaded: 10, total: 10, state: 'done' },
      { path: '/data/readme.txt', kind: 'file', loaded: 9, total: 20, state: 'caching' }
    ]
  }
  const roots = nativeDragTransferRoots(job)
  assert.equal(roots.length, 2)
  assert.equal(roots[0].group, true)
  assert.equal(roots[0].name, 'compare')
  assert.equal(roots[0].loaded, 17)
  assert.equal(roots[0].total, 20)
  assert.deepEqual(roots[0].children.map(file => file.name), ['S/is.xsd', 'P/is.xsd'])
  assert.deepEqual(roots[0].children.map(file => [file.loaded, file.total]), [[7, 10], [10, 10]])
  assert.equal(roots[1].group, false)
  assert.equal(roots[1].path, '/data/readme.txt')
})

test('remote root folder children keep their full relative paths', () => {
  const roots = nativeDragTransferRoots({
    id: 'root', session: 'session', paths: ['/'], state: 'caching', revision: 1, createdAt: 1,
    files: [{ path: '/', kind: 'folder', loaded: 0, total: 0, state: 'caching' }, { path: '/tmp/file.dat', kind: 'file', loaded: 1, total: 2, state: 'caching' }]
  })
  assert.equal(roots[0].group, true)
  assert.equal(roots[0].children[0].name, 'tmp/file.dat')
})

test('progress updates replace native rows in place instead of reshuffling the list', () => {
  const row = (id, owner, name = id) => ({ id, nativeDragOwnerID: owner, name })
  const first = [row('a1', 'a'), row('b1', 'b'), { id: 'plain' }]

  // A's progress must not move A above B even though A updated first.
  const afterA = mergeNativeDragRows(first, 'a', [row('a1', 'a', 'A1')])
  assert.deepEqual(afterA.map(item => item.id), ['a1', 'b1', 'plain'])
  assert.equal(afterA[0].name, 'A1')

  // B's progress must not reorder B either, even when its row count grows.
  const afterB = mergeNativeDragRows(afterA, 'b', [row('b1', 'b'), row('b2', 'b')])
  assert.deepEqual(afterB.map(item => item.id), ['a1', 'b1', 'b2', 'plain'])

  // Only a job that has no rows yet joins at the top; later updates stay put.
  const fresh = mergeNativeDragRows(afterB, 'c', [row('c1', 'c')])
  assert.deepEqual(fresh.map(item => item.id), ['c1', 'a1', 'b1', 'b2', 'plain'])
  const updated = mergeNativeDragRows(fresh, 'c', [row('c1', 'c', 'C1')])
  assert.deepEqual(updated.map(item => item.id), ['c1', 'a1', 'b1', 'b2', 'plain'])
  assert.equal(updated[0].name, 'C1')

  // Rows matched by their nativeDragID are replaced too, as child rows are.
  const child = { id: 'a1:file:x', nativeDragID: 'a', parentID: 'a1' }
  const withChild = mergeNativeDragRows([row('a1', 'a'), child, row('b1', 'b')], 'a', [row('a1', 'a')])
  assert.deepEqual(withChild.map(item => item.id), ['a1', 'b1'])
})

test('transfer history retains a complete collapsible parent and child family', () => {
  const rows = [
    { id: 'new-parent', status: 'done' },
    { id: 'new-child', parentID: 'new-parent', status: 'done' },
    { id: 'active-parent', status: 'running' },
    { id: 'active-child', parentID: 'active-parent', status: 'running' },
    { id: 'old-parent', status: 'done' },
    { id: 'old-child', parentID: 'old-parent', status: 'done' }
  ]
  const kept = retainTransferHistory(rows, 1)
  assert.deepEqual(kept.map(row => row.id), ['new-parent', 'new-child', 'active-parent', 'active-child'])
})

test('finished children of a running group survive history trimming', () => {
  const rows = [
    { id: 'group', status: 'running' },
    { id: 'done-child', parentID: 'group', status: 'done' },
    { id: 'running-child', parentID: 'group', status: 'running' },
    ...Array.from({ length: 350 }, (_, i) => ({ id: `history-${i}`, status: 'done' }))
  ]
  const kept = retainTransferHistory(rows, 300)
  assert.ok(kept.some(row => row.id === 'done-child'))
  assert.ok(kept.some(row => row.id === 'running-child'))
  assert.ok(kept.some(row => row.id === 'group'))
})

test('finished children survive while a running group awaits further discovery', () => {
  const rows = [
    { id: 'group', status: 'running' },
    { id: 'done-child', parentID: 'group', status: 'done' },
    ...Array.from({ length: 350 }, (_, i) => ({ id: `history-${i}`, status: 'done' }))
  ]
  const kept = retainTransferHistory(rows, 300)
  assert.ok(kept.some(row => row.id === 'done-child'))
  assert.ok(kept.some(row => row.id === 'group'))
  assert.equal(kept.filter(row => !row.parentID && row.status === 'done').length, 300)
})

class FakeWindow extends EventTarget {
  location = { origin: 'http://127.0.0.1:8888' }
  top = this
  listeners = new Map()

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || new Set()
    listeners.add(listener)
    this.listeners.set(type, listeners)
    super.addEventListener(type, listener)
  }

  removeEventListener(type, listener) {
    this.listeners.get(type)?.delete(listener)
    super.removeEventListener(type, listener)
  }

  finish(result) {
    this.dispatchEvent(new CustomEvent('chemssh-native-drag-finished', { detail: result }))
  }
}

const request = { session: 'session', paths: ['/home/user/results', '/home/user/run.log'], marker: 'marker' }

test('iframe calls the top-level binding and waits for OLE completion, not RPC acknowledgment', async () => {
  const host = new FakeWindow()
  const frame = new FakeWindow()
  frame.top = host
  frame.chemsshDragOutFiles = () => { throw new Error('iframe RPC cannot reach the host') }
  let payload
  host.chemsshDragOutFiles = function (json) {
    assert.equal(this, host)
    payload = JSON.parse(json)
    return Promise.resolve()
  }
  assert.equal(nativeDragHost(frame), host)
  let settled = false
  const operation = dragOutFiles(request, frame).then(result => { settled = true; return result })
  await Promise.resolve()
  assert.equal(settled, false)
  assert.deepEqual(payload.paths, request.paths)
  assert.equal(payload.session, request.session)
  assert.equal(payload.marker, request.marker)
  host.finish({ id: 'different-request', status: 'cancelled', ok: true })
  await Promise.resolve()
  assert.equal(settled, false)
  const result = { id: payload.id, status: 'dropped', ok: true }
  host.finish(result)
  assert.deepEqual(await operation, result)
  assert.equal(host.listeners.get('chemssh-native-drag-finished').size, 0)
  assert.equal(frame.listeners.get('pagehide').size, 0)
})

test('multiple file pages only receive their own completion', async () => {
  const host = new FakeWindow()
  const payloads = []
  host.chemsshDragOutFiles = json => { payloads.push(JSON.parse(json)); return Promise.resolve() }
  const first = new FakeWindow()
  const second = new FakeWindow()
  first.top = second.top = host
  const a = dragOutFiles(request, first)
  const b = dragOutFiles(request, second)
  assert.notEqual(payloads[0].id, payloads[1].id)
  host.finish({ id: payloads[1].id, status: 'busy', ok: false })
  assert.equal((await b).status, 'busy')
  assert.equal(host.listeners.get('chemssh-native-drag-finished').size, 1)
  host.finish({ id: payloads[0].id, status: 'dropped', ok: true })
  assert.equal((await a).status, 'dropped')
  assert.equal(host.listeners.get('chemssh-native-drag-finished').size, 0)
})

test('RPC rejection and closing the file page remove the host listener', async () => {
  const host = new FakeWindow()
  const frame = new FakeWindow()
  frame.top = host
  host.chemsshDragOutFiles = () => Promise.reject(new Error('native refused'))
  await assert.rejects(dragOutFiles(request, frame), /native refused/)
  assert.equal(host.listeners.get('chemssh-native-drag-finished').size, 0)
  host.chemsshDragOutFiles = () => Promise.resolve()
  const operation = dragOutFiles(request, frame)
  frame.dispatchEvent(new Event('pagehide'))
  await assert.rejects(operation, /closed/)
  assert.equal(host.listeners.get('chemssh-native-drag-finished').size, 0)
})

test('caching status belongs to the requesting iframe and clears on completion', async () => {
  const host = new FakeWindow()
  const frame = new FakeWindow()
  frame.top = host
  let payload
  host.chemsshDragOutFiles = json => { payload = JSON.parse(json); return Promise.resolve() }
  const states = []
  const operation = dragOutFiles(request, frame, value => states.push(value))
  const stage = detail => host.dispatchEvent(new CustomEvent('chemssh-native-drag-staging', { detail }))
  stage({ id: 'another-drag', caching: true })
  stage({ id: payload.id, caching: 'invalid' })
  assert.deepEqual(states, [])
  stage({ id: payload.id, caching: true })
  assert.deepEqual(states, [true])
  stage({ id: payload.id, caching: false })
  host.finish({ id: payload.id, status: 'dropped', ok: true })
  await operation
  assert.equal(states.at(-1), false)
  assert.equal(host.listeners.get('chemssh-native-drag-staging').size, 0)
  stage({ id: payload.id, caching: true })
  assert.equal(states.at(-1), false)
})

test('cache prompt is cleared after cancellation, RPC failure or page close', async () => {
  for (const mode of ['cancelled', 'rpc-failure', 'page-close']) {
    const host = new FakeWindow()
    const frame = new FakeWindow()
    frame.top = host
    let payload, rejectRPC
    host.chemsshDragOutFiles = json => {
      payload = JSON.parse(json)
      return new Promise((resolve, reject) => { rejectRPC = reject })
    }
    const states = []
    const operation = dragOutFiles(request, frame, value => states.push(value))
    host.dispatchEvent(new CustomEvent('chemssh-native-drag-staging', { detail: { id: payload.id, caching: true } }))
    assert.deepEqual(states, [true])
    if (mode === 'cancelled') {
      host.finish({ id: payload.id, status: 'cancelled', ok: true })
      await operation
    } else {
      const rejection = assert.rejects(operation)
      if (mode === 'rpc-failure') rejectRPC(new Error('native failure'))
      else frame.dispatchEvent(new Event('pagehide'))
      await rejection
    }
    assert.deepEqual(states, [true, false])
    assert.equal(host.listeners.get('chemssh-native-drag-staging').size, 0)
  }
})

test('browser-only and cross-origin embeddings cannot claim native support', async () => {
  const page = new FakeWindow()
  assert.equal(nativeDragHost(page), null)
  await assert.rejects(dragOutFiles(request, page), /not available/)
  const host = new FakeWindow()
  host.chemsshDragOutFiles = () => Promise.resolve()
  page.top = host
  host.location.origin = 'https://example.com'
  assert.equal(nativeDragHost(page), null)
  Object.defineProperty(page, 'top', { get() { throw new Error('cross origin') } })
  assert.equal(nativeDragHost(page), null)
})
