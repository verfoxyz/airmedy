import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  dislike: vi.fn(),
  undislike: vi.fn(),
  preview: vi.fn(),
  on: vi.fn(),
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/lastfmservice', () => ({
  ListDislikes: mocks.list,
  Dislike: mocks.dislike,
  Undislike: mocks.undislike,
  PreviewDislike: mocks.preview,
}))

vi.mock('@wailsio/runtime', () => ({
  Events: { On: mocks.on },
  Create: {
    Nullable: (fn: any) => (v: any) => (v == null ? null : fn(v)),
    Array: (fn: any) => (arr: any[]) => (arr ?? []).map(fn),
    Struct: (ctor: any) => (v: any) => (v == null ? null : new ctor(v)),
    Map: () => (val: any) => val,
    Any: (val: any) => val,
  },
}))

import { useDislikeStore, type DislikeEntry } from './dislike'

function entry(overrides: Partial<DislikeEntry> = {}): DislikeEntry {
  return {
    id: 'd1',
    kind: 'track',
    match_key: 'drop\x00song',
    title: 'Song',
    artist: 'Drop',
    created_at: '2026-11-17T00:00:00Z',
    ...overrides,
  }
}

describe('dislike store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocks.list.mockReset()
    mocks.dislike.mockReset()
    mocks.undislike.mockReset()
    mocks.on.mockReset()
    mocks.on.mockReturnValue(() => {})
    mocks.list.mockResolvedValue([])
    mocks.dislike.mockResolvedValue({ entry: entry(), deleted_tracks: 0, purge_error: '' })
    mocks.preview.mockReset()
    mocks.preview.mockResolvedValue(0)
    mocks.undislike.mockResolvedValue(undefined)
  })

  it('loads the list on init and keeps one event subscription', async () => {
    mocks.list.mockResolvedValue([entry()])
    const store = useDislikeStore()
    store.init()
    store.init()
    await flushPromises()

    expect(mocks.list).toHaveBeenCalledTimes(1)
    expect(mocks.on).toHaveBeenCalledWith('dislike:updated', expect.any(Function))
    expect(store.isTrackDisliked('drop\x00song', 'Drop')).toBe(true)
  })

  it('blocks a track by artist, including a multi-artist credit', () => {
    const store = useDislikeStore()
    store.items = [entry({ id: 'd2', kind: 'artist', match_key: 'drop', artist: 'Drop' })]

    expect(store.isArtistDisliked('Drop; Someone Else')).toBe(true)
    expect(store.isTrackDisliked('other\x00song', 'Drop')).toBe(true)
    expect(store.isTrackDisliked('other\x00song', 'Keep')).toBe(false)
  })

  it('upserts the returned row and removes it again', async () => {
    const store = useDislikeStore()
    const saved = await store.dislikeTrack({ matchKey: 'drop\x00song', title: 'Song', artist: 'Drop' })

    expect(saved?.id).toBe('d1')
    expect(store.items).toHaveLength(1)
    expect(mocks.dislike).toHaveBeenCalledWith('track', 'drop\x00song', 'Song', 'Drop', false)

    await store.remove('d1')
    expect(mocks.undislike).toHaveBeenCalledWith('d1')
    expect(store.items).toHaveLength(0)
  })

  it('replaces the cache from the update event and releases the listener', () => {
    const off = vi.fn()
    mocks.on.mockReturnValue(off)
    const store = useDislikeStore()
    store.init()
    const handler = mocks.on.mock.calls[0][1]

    handler({ data: { items: [entry({ id: 'd3', kind: 'artist', artist: 'Drop' })] } })
    expect(store.items).toHaveLength(1)

    store.dispose()
    expect(off).toHaveBeenCalled()
  })

  it('asks for confirmation before purging files', async () => {
    const { useAppStore } = await import('./app')
    useAppStore().dislikePurgeFiles = true
    mocks.preview.mockResolvedValue(3)
    const store = useDislikeStore()

    await store.requestDislike({
      kind: 'track', matchKey: 'drop\x00song', title: 'Song', artist: 'Drop',
    })

    expect(mocks.preview).toHaveBeenCalledWith('track', 'drop\x00song', 'Song', 'Drop')
    expect(store.pending?.title).toBe('Song')
    expect(store.pendingCount).toBe(3)
    expect(mocks.dislike).not.toHaveBeenCalled()

    await store.confirmPending(true)
    expect(mocks.dislike).toHaveBeenCalledWith('track', 'drop\x00song', 'Song', 'Drop', true)
    expect(store.pending).toBeNull()
  })

  it('confirms every dislike, even when files are not purged', async () => {
    const { useAppStore } = await import('./app')
    useAppStore().dislikePurgeFiles = false
    const store = useDislikeStore()
    const confirmed = vi.fn()

    await store.requestDislike({
      kind: 'track', matchKey: '', title: 'Song', artist: 'Drop', onConfirmed: confirmed,
    })

    expect(store.pending?.title).toBe('Song')
    expect(mocks.preview).not.toHaveBeenCalled()
    expect(mocks.dislike).not.toHaveBeenCalled()

    await store.confirmPending(false)
    expect(mocks.dislike).toHaveBeenCalledWith('track', '', 'Song', 'Drop', false)
    expect(confirmed).toHaveBeenCalled()
  })

  it('does nothing when the confirmation is dismissed', async () => {
    const store = useDislikeStore()
    await store.requestDislike({
      kind: 'track', matchKey: '', title: 'Song', artist: 'Drop',
    })
    store.dismissPending()

    expect(store.pending).toBeNull()
    expect(mocks.dislike).not.toHaveBeenCalled()
  })

  it('keeps a partial purge failure on the store', async () => {
    mocks.dislike.mockResolvedValue({
      entry: entry(), deleted_tracks: 1, purge_error: 'file is locked',
    })
    const store = useDislikeStore()
    await store.dislikeTrack({ title: 'Song', artist: 'Drop' })

    expect(store.purgeError).toBe('file is locked')
  })
})
