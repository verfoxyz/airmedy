import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  dislike: vi.fn(),
  undislike: vi.fn(),
  on: vi.fn(),
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/lastfmservice', () => ({
  ListDislikes: mocks.list,
  Dislike: mocks.dislike,
  Undislike: mocks.undislike,
}))

vi.mock('@wailsio/runtime', () => ({
  Events: { On: mocks.on },
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
    mocks.dislike.mockResolvedValue(entry())
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

    expect(saved.id).toBe('d1')
    expect(store.items).toHaveLength(1)
    expect(mocks.dislike).toHaveBeenCalledWith('track', 'drop\x00song', 'Song', 'Drop')

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
})
