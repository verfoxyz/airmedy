import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useLibrarySyncStore } from './librarySync'

const api = vi.hoisted(() => ({
  handlers: new Map<string, (event: { data: unknown }) => void>(),
  offs: [] as Array<ReturnType<typeof vi.fn>>,
  ReindexAll: vi.fn(),
  SyncAll: vi.fn(),
}))

vi.mock('@wailsio/runtime', () => ({
  Events: {
    On: (name: string, handler: (event: { data: unknown }) => void) => {
      api.handlers.set(name, handler)
      const off = vi.fn()
      api.offs.push(off)
      return off
    },
  },
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/libraryservice', () => ({
  ReindexAll: api.ReindexAll,
  SyncAll: api.SyncAll,
}))

describe('library sync store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    api.handlers.clear()
    api.offs = []
    api.ReindexAll.mockResolvedValue(undefined)
    api.SyncAll.mockResolvedValue(undefined)
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('opens on start, tracks progress and clears 1.5s after finishing', () => {
    const store = useLibrarySyncStore()
    store.init()

    api.handlers.get('library:sync-started')!({ data: { total: 10, path: '/music' } })
    expect(store.isSyncing).toBe(true)
    expect(store.visible).toBe(true)
    expect(store.displayType).toBe('sync')

    api.handlers.get('library:sync-progress')!({ data: { current: 5, total: 10, path: '/music/a.flac' } })
    expect(store.progress?.current).toBe(5)

    api.handlers.get('library:sync-finished')!({ data: { background: false } })
    expect(store.isSyncing).toBe(false)
    expect(store.complete).toBe(true)
    expect(store.visible).toBe(true)

    vi.advanceTimersByTime(1500)
    expect(store.complete).toBe(false)
    expect(store.progress).toBeNull()
    expect(store.type).toBeNull()
    expect(store.visible).toBe(false)
  })

  it('keeps a background sync hidden and does not show a completion', () => {
    const store = useLibrarySyncStore()
    store.init()

    api.handlers.get('library:sync-started')!({ data: { background: true, total: 3 } })
    expect(store.isSyncing).toBe(true)
    expect(store.visible).toBe(false)

    api.handlers.get('library:sync-finished')!({ data: { background: true } })
    expect(store.complete).toBe(false)
    expect(store.visible).toBe(false)
  })

  it('runOptimize settles even when no finished event arrives', async () => {
    const store = useLibrarySyncStore()
    store.init()

    const run = store.runOptimize()
    expect(store.type).toBe('optimize')
    expect(store.isSyncing).toBe(true)

    await run
    expect(api.ReindexAll).toHaveBeenCalledTimes(1)
    expect(store.isSyncing).toBe(false)
    expect(store.complete).toBe(true)

    vi.advanceTimersByTime(1500)
    expect(store.complete).toBe(false)
    expect(store.type).toBeNull()
  })

  it('a failed optimize resets instead of showing a completion', async () => {
    api.ReindexAll.mockRejectedValue(new Error('offline'))
    const store = useLibrarySyncStore()
    store.init()

    await store.runOptimize()
    expect(store.isSyncing).toBe(false)
    expect(store.complete).toBe(false)
    expect(store.type).toBeNull()
  })

  it('init is idempotent and dispose releases every subscription', () => {
    const store = useLibrarySyncStore()
    store.init()
    store.init()
    expect(api.offs).toHaveLength(3)

    store.dispose()
    for (const off of api.offs) expect(off).toHaveBeenCalled()
    expect(store.isSyncing).toBe(false)
    expect(store.complete).toBe(false)
  })
})
