import test from 'node:test'
import assert from 'node:assert/strict'
import { createDownloadQueue, canCancelDownload } from '../src/nativeDownloads.ts'

const snapshot = (fields = {}) => ({ loaded: 5, total: 10, done: false, cancelled: false, paused: false, error: '', ...fields })
function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
function makeQueue(cancel = async () => snapshot({ cancelled: true })) {
  let seq = 0
  const starts = []
  const listeners = new Map()
  const queue = createDownloadQueue({
    id: () => `download-${++seq}`,
    watch: (id, callback) => { listeners.set(id, callback); return () => listeners.delete(id) },
    run: (...args) => { const pending = deferred(); starts.push({ args, ...pending }); return pending.promise },
    cancel,
  }, 1)
  const enqueue = (...paths) => queue.enqueue({ sessionId: 'session-1', sourceName: 'example-server',
    sources: paths.map(path => ({ path })),
    targets: paths.map(path => ({ local_path: `/downloads${path}`, renamed: false })),
  })
  return { queue, starts, listeners, enqueue }
}

test('batch queues run at bounded concurrency and cancel before starting without IPC', async () => {
  let cancelCalls = 0
  const { queue, starts, listeners, enqueue } = makeQueue(async () => { cancelCalls++; return snapshot() })
  const batch = enqueue('/a', '/b')
  assert.equal(batch.count, 2)
  assert.deepEqual(starts[0].args, ['session-1', '/a', '/downloads/a', 'download-1', false])
  assert.deepEqual(queue.jobs.value.map(job => job.state), ['running', 'queued'])
  await queue.cancel('download-2')
  assert.equal(cancelCalls, 0)
  listeners.get('download-1')(snapshot())
  assert.equal(queue.jobs.value[0].loaded, 5)
  const sessionDone = queue.waitForSession('session-1')
  starts[0].resolve()
  assert.deepEqual(await batch.done, ['saved', 'cancelled'])
  assert.deepEqual(await sessionDone, ['saved'])
  assert.equal(listeners.size, 0)
  assert.equal(starts.length, 1)
})

test('cancellation does not finish before cleanup and a late commit does not overwrite a saved row', async () => {
  const pending = deferred()
  const { queue, starts, listeners, enqueue } = makeQueue(() => pending.promise)
  const first = enqueue('/a')
  assert.equal(starts[0].args.at(-1), true)
  const cancelling = queue.cancel('download-1')
  assert.equal(queue.jobs.value[0].state, 'cancelling')
  pending.resolve(snapshot({ cancelled: true }))
  await cancelling
  assert.equal(queue.jobs.value[0].state, 'cancelling')
  starts[0].reject(new Error('cancelled'))
  assert.deepEqual(await first.done, ['cancelled'])
  await Promise.resolve()
  const second = enqueue('/b')
  await Promise.resolve()
  listeners.get('download-2')(snapshot({ committing: true }))
  assert.equal(canCancelDownload(queue.jobs.value[0]), false)
  starts[1].resolve()
  assert.deepEqual(await second.done, ['saved'])

  const lateCommit = deferred()
  const late = makeQueue(() => lateCommit.promise)
  const batch = late.enqueue('/c')
  const lateCancel = late.queue.cancel('download-1')
  late.starts[0].resolve()
  assert.deepEqual(await batch.done, ['saved'])
  lateCommit.resolve(snapshot({ committing: true }))
  await lateCancel
  assert.equal(late.queue.jobs.value[0].state, 'saved')
})

test('failed cancel IPC cannot mask an independent download failure', async () => {
  const { queue, starts, enqueue } = makeQueue(async () => { throw new Error('IPC rejected') })
  const batch = enqueue('/a')
  const cancelling = queue.cancel('download-1')
  starts[0].reject(new Error('disk full'))
  await assert.rejects(cancelling, /IPC rejected/)
  assert.deepEqual(await batch.done, ['failed'])
  assert.equal(queue.jobs.value[0].error, 'disk full')
})
