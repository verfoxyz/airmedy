import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mockSearch = vi.fn()

vi.mock('../../bindings/airmedy/internal/infra/wails/searchservice', () => ({
  Search: (...args: unknown[]) => mockSearch(...args),
}))

import { useSearchStore } from './search'

describe('useSearchStore', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockSearch.mockReset()
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('refreshes the active query immediately without clearing visible results', async () => {
    const store = useSearchStore()
    store.query = 'old album'
    store.results = { albums: [{ id: 'old' }] } as any
    mockSearch.mockResolvedValue({ albums: [{ id: 'new' }] })

    await store.refresh()

    expect(mockSearch).toHaveBeenCalledWith('old album')
    expect(store.results).toEqual({ albums: [{ id: 'new' }] })
    expect(store.loading).toBe(false)
  })

  it('does not re-query when entering with the same keyword and results', async () => {
    const store = useSearchStore()
    store.query = 'album'
    store.results = { albums: [{ id: 'kept' }] } as any

    store.ensureSearch('album')
    await vi.advanceTimersByTimeAsync(300)

    expect(mockSearch).not.toHaveBeenCalled()
    expect(store.results).toEqual({ albums: [{ id: 'kept' }] })
  })

  it('queries a changed keyword and clears on empty input', async () => {
    const store = useSearchStore()
    store.query = 'album'
    store.results = { albums: [{ id: 'old' }] } as any
    mockSearch.mockResolvedValue({ albums: [{ id: 'new' }] })

    store.ensureSearch('single')
    await vi.advanceTimersByTimeAsync(300)
    expect(mockSearch).toHaveBeenCalledWith('single')
    expect(store.query).toBe('single')
    expect(store.results).toEqual({ albums: [{ id: 'new' }] })

    store.ensureSearch('')
    await vi.advanceTimersByTimeAsync(300)
    expect(store.results).toBeNull()
  })
})
