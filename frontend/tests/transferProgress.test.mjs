import assert from 'node:assert/strict'
import { test } from 'node:test'
import { waitForTransferProgress } from '../src/transferProgress.ts'

const snapshot = overrides => ({
  loaded: 0,
  total: 10,
  done: false,
  cancelled: false,
  error: '',
  paused: false,
  ...overrides
})

function channel() {
  const state = { handler: null, unsubscribed: 0 }
  const subscribe = handler => {
    state.handler = handler
    return () => {
      state.unsubscribed += 1
      state.handler = null
    }
  }
  return {
    subscribe,
    push: progress => state.handler?.(progress),
    get unsubscribed() {
      return state.unsubscribed
    },
    get active() {
      return state.handler !== null
    }
  }
}

test('resolves only after the pushed progress reaches a terminal state', async () => {
  const source = channel()
  const seen = []
  const waiter = waitForTransferProgress(source.subscribe, p => seen.push(p.loaded), () => 'cancelled')
  source.push(snapshot({ loaded: 2 }))
  source.push(snapshot({ loaded: 8 }))
  let resolved = false
  void waiter.done.then(() => { resolved = true })
  await Promise.resolve()
  assert.equal(resolved, false)
  source.push(snapshot({ loaded: 10, total: 10, done: true }))
  await waiter.done
  assert.deepEqual(seen, [2, 8, 10])
  assert.equal(source.unsubscribed, 1)
})

test('resolves when the first pushed snapshot is already done', async () => {
  const source = channel()
  const waiter = waitForTransferProgress(source.subscribe, () => {}, () => 'cancelled')
  source.push(snapshot({ loaded: 10, total: 10, done: true }))
  await waiter.done
  assert.equal(source.active, false)
})

test('settles synchronously when a terminal snapshot is pushed inside subscribe', async () => {
  const waiter = waitForTransferProgress(
    handler => {
      handler(snapshot({ done: true, loaded: 10, total: 10 }))
      return () => {}
    },
    () => {},
    () => 'cancelled'
  )
  await waiter.done
})

test('rejects when the transfer finishes with an error', async () => {
  const source = channel()
  const waiter = waitForTransferProgress(source.subscribe, () => {}, () => 'cancelled')
  source.push(snapshot({ done: true, error: 'permission denied' }))
  await assert.rejects(waiter.done, /permission denied/)
})

test('rejects a cancelled transfer with the backend error or the fallback text', async () => {
  const withError = channel()
  const first = waitForTransferProgress(withError.subscribe, () => {}, () => 'fallback')
  withError.push(snapshot({ cancelled: true, error: 'transfer cancelled' }))
  await assert.rejects(first.done, /transfer cancelled/)

  const withoutError = channel()
  const second = waitForTransferProgress(withoutError.subscribe, () => {}, () => 'fallback')
  withoutError.push(snapshot({ cancelled: true }))
  await assert.rejects(second.done, /fallback/)
})

test('stop unsubscribes and ignores later snapshots', async () => {
  const source = channel()
  const seen = []
  const waiter = waitForTransferProgress(source.subscribe, p => seen.push(p.loaded), () => 'cancelled')
  source.push(snapshot({ loaded: 1 }))
  waiter.stop()
  source.push(snapshot({ loaded: 2 }))
  assert.deepEqual(seen, [1])
  assert.equal(source.unsubscribed, 1)
  assert.equal(source.active, false)
})
