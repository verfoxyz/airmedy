import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import LastFmView from './LastFmView.vue'

const mocks = vi.hoisted(() => ({
  push: vi.fn(),
  getTopTracks: vi.fn(),
  preparePlayback: vi.fn(),
  save: vi.fn(),
  playTrackIds: vi.fn(),
  handlers: new Map<string, (event: { data?: unknown }) => void>(),
  off: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => {
      if (params?.artist) return `${key}:${params.artist}`
      if (params?.username) return `${key}:${params.username}`
      if (params?.source) return `${key}:${params.source}`
      return key
    },
  }),
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: mocks.push }),
}))

vi.mock('@wailsio/runtime', async (importOriginal) => ({
  ...await importOriginal<typeof import('@wailsio/runtime')>(),
  Events: {
    On: (name: string, handler: (event: { data?: unknown }) => void) => {
      mocks.handlers.set(name, handler)
      return mocks.off
    },
  },
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/lastfmservice', () => ({
  GetTopTrackRecommendations: mocks.getTopTracks,
}))

vi.mock('@/stores/catalog', () => ({
  useCatalogStore: () => ({
    preparePlayback: mocks.preparePlayback,
    save: mocks.save,
    actionError: '',
  }),
}))

vi.mock('@/stores/player', () => ({
  usePlayerStore: () => ({ playTrackIds: mocks.playTrackIds }),
}))

type RowState = 'matching' | 'matched' | 'no_match'

function item(title: string, state: RowState = 'matching') {
  return {
    title,
    artist: 'Artist',
    album: 'Album',
    duration: 200,
    artists: ['Artist'],
    cover_url: '',
    cover_url_large: '',
    matching: state === 'matching',
    matched: state === 'matched',
    no_source: state === 'no_match',
    provider: state === 'matched' ? 'wy' : '',
    external_id: state === 'matched' ? `wy-${title}` : '',
    state: '',
    local_track_id: '',
    blocked_reason: '',
    sources: state === 'matched' ? ['wy'] : [],
    downloadable: state === 'matched',
    match_key: `artist\u0000${title.toLowerCase()}`,
  }
}

function page(items: ReturnType<typeof item>[], status = 'ok') {
  return { status, items }
}

async function mountView() {
  const wrapper = mount(LastFmView, {
    global: {
      mocks: {
        $t: (key: string) => key,
      },
    },
  })
  await flushPromises()
  return wrapper
}

/** emitMatch delivers one lastfm:recommendations-matched batch. */
async function emitMatch(data: unknown) {
  const handler = mocks.handlers.get('lastfm:recommendations-matched')
  expect(handler).toBeDefined()
  handler!({ data })
  await nextTick()
}

describe('LastFmView (top tracks MVP)', () => {
  beforeEach(() => {
    mocks.push.mockReset()
    mocks.getTopTracks.mockReset()
    mocks.preparePlayback.mockReset()
    mocks.save.mockReset()
    mocks.playTrackIds.mockReset()
    mocks.handlers.clear()
    mocks.off.mockReset()
    mocks.preparePlayback.mockResolvedValue('track-1')
    mocks.save.mockResolvedValue(true)
    mocks.playTrackIds.mockResolvedValue(undefined)
  })

  it('guides the user to settings when Last.fm is not connected', async () => {
    mocks.getTopTracks.mockResolvedValue(page([], 'not_connected'))
    const wrapper = await mountView()

    expect(wrapper.find('[data-test="lastfm-not-connected"]').exists()).toBe(true)
    await wrapper.find('[data-test="lastfm-open-settings"]').trigger('click')
    expect(mocks.push).toHaveBeenCalledWith('/settings/integrations')
  })

  it('renders every row immediately in the matching state', async () => {
    mocks.getTopTracks.mockResolvedValue(page([item('One'), item('Two'), item('Three')]))
    const wrapper = await mountView()

    const rows = wrapper.findAll('[data-test="top-track-row"]')
    expect(rows).toHaveLength(3)
    for (const row of rows) {
      expect(row.find('[data-test="top-track-status"]').attributes('data-state')).toBe('matching')
      expect(row.find('[data-test="top-track-play"]').exists()).toBe(false)
      expect(row.find('[data-test="top-track-save"]').exists()).toBe(false)
    }
  })

  it('shows the three status chips', async () => {
    mocks.getTopTracks.mockResolvedValue(page([
      item('Pending', 'matching'),
      item('Matched', 'matched'),
      item('Missing', 'no_match'),
    ]))
    const wrapper = await mountView()

    const statuses = wrapper.findAll('[data-test="top-track-status"]')
    expect(statuses.map((status) => status.attributes('data-state'))).toEqual([
      'matching',
      'matched',
      'no-match',
    ])
    expect(statuses[1].text()).toBe('lastfm.matched_from:wy')
    expect(statuses[2].text()).toBe('lastfm.no_match')
  })

  it('updates a row in place when a match event arrives', async () => {
    mocks.getTopTracks.mockResolvedValue(page([item('Hit', 'matching')]))
    const wrapper = await mountView()

    await emitMatch({ items: [item('Hit', 'matched')], done: false })

    const row = wrapper.find('[data-test="top-track-row"]')
    expect(row.find('[data-test="top-track-status"]').attributes('data-state')).toBe('matched')
    expect(row.find('[data-test="top-track-play"]').exists()).toBe(true)
    expect(row.find('[data-test="top-track-save"]').exists()).toBe(true)
  })

  it('marks unresolved rows after done', async () => {
    mocks.getTopTracks.mockResolvedValue(page([item('Missing', 'matching')]))
    const wrapper = await mountView()

    await emitMatch({ items: [], done: true })

    const row = wrapper.find('[data-test="top-track-row"]')
    expect(row.find('[data-test="top-track-status"]').attributes('data-state')).toBe('no-match')
    expect(row.find('[data-test="top-track-play"]').exists()).toBe(false)
  })

  it('plays and saves a matched row through the shared catalogue actions', async () => {
    mocks.getTopTracks.mockResolvedValue(page([item('Hit', 'matched')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="top-track-play"]').trigger('click')
    await flushPromises()
    expect(mocks.preparePlayback).toHaveBeenCalledWith(expect.objectContaining({ title: 'Hit', provider: 'wy' }))
    expect(mocks.playTrackIds).toHaveBeenCalledWith(['track-1'])

    await wrapper.find('[data-test="top-track-save"]').trigger('click')
    await flushPromises()
    expect(mocks.save).toHaveBeenCalledWith(
      expect.objectContaining({ title: 'Hit', provider: 'wy' }),
      { favorite: true },
    )
  })

  it('returns a failed row to matching and survives a done frame', async () => {
    mocks.getTopTracks.mockResolvedValue(page([item('Hit', 'matched')]))
    const wrapper = await mountView()

    // The backend re-queues the row and, while it is searching, an older worker
    // run sends Done. The row must stay in the matching state.
    await emitMatch({ items: [item('Hit', 'matching')], done: true })

    const row = wrapper.find('[data-test="top-track-row"]')
    expect(row.find('[data-test="top-track-status"]').attributes('data-state')).toBe('matching')
    expect(row.find('[data-test="top-track-play"]').exists()).toBe(false)

    await emitMatch({ items: [item('Hit', 'matched')], done: false })
    expect(row.find('[data-test="top-track-status"]').attributes('data-state')).toBe('matched')
    expect(row.find('[data-test="top-track-play"]').exists()).toBe(true)
  })

  it('shows no match when the failover finds nothing', async () => {
    mocks.getTopTracks.mockResolvedValue(page([item('Hit', 'matched')]))
    const wrapper = await mountView()

    await emitMatch({ items: [item('Hit', 'matching')], done: false })
    await emitMatch({ items: [item('Hit', 'no_match')], done: false })

    const row = wrapper.find('[data-test="top-track-row"]')
    expect(row.find('[data-test="top-track-status"]').attributes('data-state')).toBe('no-match')
    expect(row.find('[data-test="top-track-play"]').exists()).toBe(false)
  })

  it('refreshes the page and returns to the matching state', async () => {
    mocks.getTopTracks.mockResolvedValue(page([item('Hit', 'matching')]))
    const wrapper = await mountView()
    await emitMatch({ items: [], done: true })
    expect(wrapper.find('[data-test="top-track-status"]').attributes('data-state')).toBe('no-match')

    await wrapper.find('[data-test="lastfm-refresh"]').trigger('click')
    await flushPromises()

    expect(mocks.getTopTracks).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-test="top-track-status"]').attributes('data-state')).toBe('matching')
  })

  it('shows the empty state and keeps the refresh button', async () => {
    mocks.getTopTracks.mockResolvedValue(page([]))
    const wrapper = await mountView()

    const empty = wrapper.find('[data-test="lastfm-empty"]')
    expect(empty.exists()).toBe(true)
    await empty.find('button').trigger('click')
    await flushPromises()
    expect(mocks.getTopTracks).toHaveBeenCalledTimes(2)
  })

  it('releases the match listener on unmount', async () => {
    mocks.getTopTracks.mockResolvedValue(page([item('Hit')]))
    const wrapper = await mountView()

    wrapper.unmount()

    expect(mocks.off).toHaveBeenCalledTimes(1)
  })
})
