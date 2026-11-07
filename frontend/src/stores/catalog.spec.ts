import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { flushPromises } from '@vue/test-utils'
import { useCatalogStore } from './catalog'

/**
 * The store is the whole cloud branch's state machine: paging, source filtering,
 * the row save action and the two download events that update a row. These tests
 * drive it against a stubbed binding — no window, no network.
 */
const api = vi.hoisted(() => ({
  SearchCatalog: vi.fn(),
  GetCatalogSources: vi.fn(),
  SaveCatalogResult: vi.fn(),
  GetCatalogRowStates: vi.fn(),
  GetTracksByIDs: vi.fn(),
  handlers: new Map<string, (event: { data: unknown }) => void>(),
}))

vi.mock('@wailsio/runtime', () => ({
  Events: {
    On: (name: string, handler: (event: { data: unknown }) => void) => {
      api.handlers.set(name, handler)
      return () => api.handlers.delete(name)
    },
  },
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/catalogservice', () => ({
  SearchCatalog: api.SearchCatalog,
  GetCatalogSources: api.GetCatalogSources,
  SaveCatalogResult: api.SaveCatalogResult,
  GetCatalogRowStates: api.GetCatalogRowStates,
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/libraryservice', () => ({
  GetTracksByIDs: api.GetTracksByIDs,
}))

const SOURCES = [
  { id: 'wy', name: 'NetEase' },
  { id: 'kg', name: 'Kugou' },
]

function row(externalId: string, overrides: Record<string, unknown> = {}) {
  return {
    provider: 'wy',
    external_id: externalId,
    title: `track ${externalId}`,
    artists: ['周杰伦'],
    album_name: '专辑',
    duration: 200,
    sources: ['wy'],
    cover_url: '',
    cover_url_large: '',
    downloadable: true,
    blocked_reason: '',
    state: '',
    ...overrides,
  }
}

function page(rows: ReturnType<typeof row>[], hasMore = false) {
  return {
    rows,
    has_more: hasMore,
    sources: SOURCES,
    failures: [],
    error_kind: '',
  }
}

describe('catalog store', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
    vi.clearAllMocks()
    api.handlers.clear()
    vi.useFakeTimers()
  })

  it('searches after the debounce and stores the page', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')], true))
    const store = useCatalogStore()

    store.search('周杰伦')
    expect(store.loading).toBe(true)
    expect(api.SearchCatalog).not.toHaveBeenCalled()

    await vi.advanceTimersByTimeAsync(300)

    expect(api.SearchCatalog).toHaveBeenCalledWith({
      query: '周杰伦',
      sources: [],
      limit: 5,
      offset: 0,
    })
    expect(store.rows).toHaveLength(1)
    expect(store.sources).toHaveLength(2)
    expect(store.hasMore).toBe(true)
    expect(store.loading).toBe(false)
  })

  it('lists sources without searching, so chips render before a query', async () => {
    api.GetCatalogSources.mockResolvedValue(SOURCES)
    const store = useCatalogStore()

    await store.loadSources()

    expect(store.sources).toEqual(SOURCES)
    expect(api.SearchCatalog).not.toHaveBeenCalled()
  })

  it('restores the loaded page when re-entering with the same keyword', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1'), row('2')], true))
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    expect(api.SearchCatalog).toHaveBeenCalledTimes(1)
    expect(store.rows).toHaveLength(2)

    // Returning to cloud with the same keyword must keep rows, hasMore and the
    // source filter rather than fetching page 1 again.
    store.activeSources = ['wy']
    store.setMode('local')
    store.ensureSearch('周杰伦')
    await vi.advanceTimersByTimeAsync(300)

    expect(api.SearchCatalog).toHaveBeenCalledTimes(1)
    expect(store.rows).toHaveLength(2)
    expect(store.hasMore).toBe(true)
    expect(store.activeSources).toEqual(['wy'])
  })

  it('searches a changed keyword instead of restoring the old page', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')]))
    const store = useCatalogStore()
    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)

    api.SearchCatalog.mockResolvedValue(page([row('9')]))
    store.ensureSearch('晴天')
    await vi.advanceTimersByTimeAsync(300)

    expect(api.SearchCatalog).toHaveBeenCalledTimes(2)
    expect(api.SearchCatalog).toHaveBeenLastCalledWith({
      query: '晴天',
      sources: [],
      limit: 5,
      offset: 0,
    })
    expect(store.rows).toHaveLength(1)
  })

  it('appends the next page at the current offset', async () => {
    api.SearchCatalog.mockResolvedValueOnce(page([row('1')], true))
    api.SearchCatalog.mockResolvedValueOnce(page([row('2')], false))
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    store.loadMore()
    await vi.advanceTimersByTimeAsync(0)

    expect(api.SearchCatalog).toHaveBeenLastCalledWith(expect.objectContaining({ limit: 10, offset: 1 }))
    expect(store.rows.map((r) => r.external_id)).toEqual(['1', '2'])
    expect(store.hasMore).toBe(false)
  })

  it('loads ten more at the current merged offset', async () => {
    api.SearchCatalog.mockResolvedValueOnce(page(Array.from({ length: 5 }, (_, i) => row(`a${i}`)), true))
    api.SearchCatalog.mockResolvedValueOnce(page(Array.from({ length: 10 }, (_, i) => row(`b${i}`)), true))
    api.SearchCatalog.mockResolvedValueOnce(page(Array.from({ length: 10 }, (_, i) => row(`c${i}`)), false))
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    expect(store.rows).toHaveLength(5)

    store.loadMore()
    await vi.advanceTimersByTimeAsync(0)
    expect(api.SearchCatalog).toHaveBeenLastCalledWith(expect.objectContaining({ limit: 10, offset: 5 }))
    expect(store.rows).toHaveLength(15)

    store.loadMore()
    await vi.advanceTimersByTimeAsync(0)
    expect(api.SearchCatalog).toHaveBeenLastCalledWith(expect.objectContaining({ limit: 10, offset: 15 }))
    expect(store.rows).toHaveLength(25)
    expect(store.hasMore).toBe(false)
  })

  it('toggles sources, re-searches without the debounce and persists the choice', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')]))
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)

    store.toggleSource('kg')
    await vi.advanceTimersByTimeAsync(0)
    expect(store.activeSources).toEqual(['kg'])
    expect(api.SearchCatalog).toHaveBeenLastCalledWith(expect.objectContaining({ sources: ['kg'] }))

    store.toggleSource('wy')
    await vi.advanceTimersByTimeAsync(0)
    expect(store.activeSources).toEqual(['kg', 'wy'])
    expect(api.SearchCatalog).toHaveBeenLastCalledWith(expect.objectContaining({ sources: ['kg', 'wy'] }))
    expect(JSON.parse(localStorage.getItem('airmedy.catalog.sources') ?? '[]')).toEqual(['kg', 'wy'])

    store.toggleSource('')
    expect(store.activeSources).toEqual([])
  })

  it('clears rows when the keyword is emptied', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')]))
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    store.search('  ')

    expect(store.rows).toHaveLength(0)
    expect(store.loading).toBe(false)
  })

  it('surfaces a page-level error kind instead of rows', async () => {
    api.SearchCatalog.mockResolvedValue({ ...page([]), error_kind: 'no_sources' })
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)

    expect(store.errorKind).toBe('no_sources')
    expect(store.rows).toHaveLength(0)
  })

  it('saves a row with the favorite flag and reflects downloading', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')]))
    api.SaveCatalogResult.mockResolvedValue({ track_id: 't1', state: 'downloading' })
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    const ok = await store.save(store.rows[0], { favorite: true })

    expect(ok).toBe(true)
    expect(api.SaveCatalogResult).toHaveBeenCalledWith(expect.objectContaining({
      provider: 'wy',
      external_id: '1',
      favorite: true,
      playlist_id: '',
    }))
    expect(store.rows[0].state).toBe('downloading')
  })

  it('prefers the large cover URL when saving', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1', {
      cover_url: 'https://x/cover-100.jpg',
      cover_url_large: 'https://x/cover-500.jpg',
    })]))
    api.SaveCatalogResult.mockResolvedValue({ track_id: 't1', state: 'downloading' })
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    await store.save(store.rows[0], { favorite: true })

    expect(api.SaveCatalogResult).toHaveBeenCalledWith(expect.objectContaining({
      cover_url: 'https://x/cover-500.jpg',
    }))
  })

  it('falls back to the thumbnail when the source has no large cover', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1', { cover_url: 'https://x/cover-100.jpg' })]))
    api.SaveCatalogResult.mockResolvedValue({ track_id: 't1', state: 'downloading' })
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    await store.save(store.rows[0], { favorite: true })

    expect(api.SaveCatalogResult).toHaveBeenCalledWith(expect.objectContaining({
      cover_url: 'https://x/cover-100.jpg',
    }))
  })

  it('reuses a matched local row unless the user forces a download', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1', { local_track_id: 'local-1', state: 'in_library' })]))
    api.SaveCatalogResult.mockResolvedValue({ track_id: 'local-1', state: 'in_library' })
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    await store.save(store.rows[0], { playlistId: 'pl-1' })

    expect(api.SaveCatalogResult).toHaveBeenLastCalledWith(expect.objectContaining({
      local_track_id: 'local-1',
      force_download: false,
    }))

    await store.save(store.rows[0], { forceDownload: true })
    expect(api.SaveCatalogResult).toHaveBeenLastCalledWith(expect.objectContaining({
      local_track_id: '',
      force_download: true,
    }))
  })

  it('adds a playlist destination to the save call', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')]))
    api.SaveCatalogResult.mockResolvedValue({ track_id: 't1', state: 'downloading' })
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    await store.save(store.rows[0], { playlistId: 'pl-1' })

    expect(api.SaveCatalogResult).toHaveBeenLastCalledWith(expect.objectContaining({ playlist_id: 'pl-1' }))
  })

  it('marks a failed save as an action error without dropping the row', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')]))
    api.SaveCatalogResult.mockRejectedValue(new Error('offline'))
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    const ok = await store.save(store.rows[0])

    expect(ok).toBe(false)
    expect(store.actionError).toBe('save_failed')
    expect(store.rows).toHaveLength(1)
  })

  it('re-reads the row state by identity when the download finishes', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')]))
    api.SaveCatalogResult.mockResolvedValue({ track_id: 't1', state: 'downloading' })
    // The merge deleted the remote row the job named; only the identity survives.
    api.GetCatalogRowStates.mockResolvedValue([
      { provider: 'wy', external_id: '1', state: 'in_library' },
    ])
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    await store.save(store.rows[0])

    const onDone = api.handlers.get('download:done')
    expect(onDone).toBeDefined()
    onDone?.({ data: { track_id: 't1' } })
    await flushPromises()

    expect(api.GetCatalogRowStates).toHaveBeenCalledWith([{ provider: 'wy', external_id: '1' }])
    expect(api.GetTracksByIDs).not.toHaveBeenCalled()
    expect(store.rows[0].state).toBe('in_library')
  })

  // The regression for "ma_player_load failed: -10 (path: remote://...)" seen
  // from the UI side: a row the library still has no file for must not read as
  // playable, whatever the availability column claims.
  it('keeps a ready-by-flag row out of in-library when its path is still remote', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')]))
    api.SaveCatalogResult.mockResolvedValue({ track_id: 't1', state: 'downloading' })
    // No state comes back by identity, so the fallback reads the saved row.
    api.GetCatalogRowStates.mockResolvedValue([])
    api.GetTracksByIDs.mockResolvedValue([
      { id: 't1', path: 'remote://wy/1', availability: 'ready' },
    ])
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    await store.save(store.rows[0])

    api.handlers.get('download:done')?.({ data: { track_id: 't1' } })
    await flushPromises()

    expect(store.rows[0].state).not.toBe('in_library')
  })

  it('marks the row failed when the library reports a failure', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')]))
    api.SaveCatalogResult.mockResolvedValue({ track_id: 't1', state: 'downloading' })
    api.GetTracksByIDs.mockResolvedValue([
      { id: 't1', path: 'remote://wy/1', availability: 'failed' },
    ])
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    await store.save(store.rows[0])

    api.handlers.get('download:done')?.({ data: { track_id: 't1' } })
    await flushPromises()

    expect(store.rows[0].state).toBe('failed')
  })

  it('marks the row failed when the download fails', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')]))
    api.SaveCatalogResult.mockResolvedValue({ track_id: 't1', state: 'downloading' })
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    await store.save(store.rows[0])

    api.handlers.get('download:failed')?.({ data: { track_id: 't1' } })

    expect(store.rows[0].state).toBe('failed')
  })

  it('ignores download events for tracks it did not save', async () => {
    api.SearchCatalog.mockResolvedValue(page([row('1')]))
    const store = useCatalogStore()

    store.search('周杰伦')
    await vi.advanceTimersByTimeAsync(300)
    api.handlers.get('download:done')?.({ data: { track_id: 'someone-else' } })

    expect(store.rows[0].state).toBe('')
  })
})