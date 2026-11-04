import assert from 'node:assert/strict'
import { test } from 'node:test'
import { cancelNativeDragJob, dragOutFiles, nativeDragHost, subscribeNativeDragJobs } from '../src/nativeDrag.ts'

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
