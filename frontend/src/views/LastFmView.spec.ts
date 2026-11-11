import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import LastFmView from './LastFmView.vue'

const mocks = vi.hoisted(() => ({
  push: vi.fn(),
  getRecommendations: vi.fn(),
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
  GetRecommendations: mocks.getRecommendations,
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

function track(title: string, provider = 'wy') {
  return {
    provider,
    external_id: `${provider}-${title}`,
    title,
    artists: ['Artist'],
    album_name: '',
    duration: 200,
    cover_url: '',
    cover_url_large: '',
    downloadable: true,
    blocked_reason: '',
    state: '',
    local_track_id: '',
    sources: [provider],
    score: 1,
    source: 'similar',
    match_key: `artist\u0000${title.toLowerCase()}`,
    matching: false,
  }
}

/** pendingTrack is one Last.fm row before the background matcher runs. */
function pendingTrack(title: string) {
  return {
    ...track(title, ''),
    external_id: '',
    downloadable: false,
    sources: [],
    matching: true,
  }
}

function page(overrides: Record<string, unknown> = {}) {
  return {
    status: 'ok',
    username: 'listener',
    avatar_url: '',
    matching: false,
    tracks: [track('Combined')],
    recent_tracks: [track('Recent')],
    loved_tracks: [track('Loved')],
    top_tracks: [track('Top')],
    similar_artists: [
      { name: 'Similar One', image_url: '', listeners: 1200, url: '' },
    ],
    ...overrides,
  }
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

/** emitMatch delivers one lastfm:recommendations-matched update. */
async function emitMatch(data: unknown) {
  const handler = mocks.handlers.get('lastfm:recommendations-matched')
  expect(handler).toBeDefined()
  handler!({ data })
  await nextTick()
}

describe('LastFmView', () => {
  beforeEach(() => {
    mocks.push.mockReset()
    mocks.getRecommendations.mockReset()
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
    mocks.getRecommendations.mockResolvedValue(page({ status: 'not_connected', tracks: [], recent_tracks: [], loved_tracks: [], top_tracks: [], similar_artists: [] }))
    const wrapper = await mountView()

    expect(wrapper.find('[data-test="lastfm-not-connected"]').exists()).toBe(true)
    await wrapper.find('[data-test="lastfm-open-settings"]').trigger('click')
    expect(mocks.push).toHaveBeenCalledWith('/settings/integrations')
  })

  it('renders the combined list, the three source racks and similar artists', async () => {
    mocks.getRecommendations.mockResolvedValue(page())
    const wrapper = await mountView()

    expect(wrapper.find('[data-test="lastfm-combined"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="lastfm-rack-recent"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="lastfm-rack-loved"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="lastfm-rack-top"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="lastfm-similar-artists"]').exists()).toBe(true)
    // One card in the combined grid and one in each of the three racks.
    expect(wrapper.findAll('[data-test="recommendation-card"]')).toHaveLength(4)
  })

  it('plays and saves through the shared catalogue actions', async () => {
    mocks.getRecommendations.mockResolvedValue(page())
    const wrapper = await mountView()

    await wrapper.find('[data-test="recommendation-play"]').trigger('click')
    await flushPromises()
    expect(mocks.preparePlayback).toHaveBeenCalledWith(expect.objectContaining({ title: 'Combined' }))
    expect(mocks.playTrackIds).toHaveBeenCalledWith(['track-1'])

    await wrapper.find('[data-test="recommendation-save"]').trigger('click')
    await flushPromises()
    expect(mocks.save).toHaveBeenCalledWith(
      expect.objectContaining({ title: 'Combined' }),
      { favorite: true },
    )
  })

  it('opens the search page for a similar artist', async () => {
    mocks.getRecommendations.mockResolvedValue(page())
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-similar-artist"]').trigger('click')
    expect(mocks.push).toHaveBeenCalledWith({ path: '/search', query: { q: 'Similar One' } })
  })

  it('shows the empty state and refreshes with force', async () => {
    mocks.getRecommendations.mockResolvedValue(page({
      tracks: [],
      recent_tracks: [],
      loved_tracks: [],
      top_tracks: [],
      similar_artists: [],
    }))
    const wrapper = await mountView()

    const empty = wrapper.find('[data-test="lastfm-empty"]')
    expect(empty.exists()).toBe(true)
    expect(mocks.getRecommendations).toHaveBeenCalledTimes(1)
    expect(mocks.getRecommendations).toHaveBeenLastCalledWith(false)

    await empty.find('button').trigger('click')
    await flushPromises()
    expect(mocks.getRecommendations).toHaveBeenLastCalledWith(true)
  })

  it('shows the empty state while keeping similar artists visible', async () => {
    mocks.getRecommendations.mockResolvedValue(page({
      tracks: [],
      recent_tracks: [],
      loved_tracks: [],
      top_tracks: [],
      similar_artists: [{ name: 'Alpha', image_url: '', listeners: 10, url: '' }],
    }))
    const wrapper = await mountView()

    expect(wrapper.find('[data-test="lastfm-empty"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="lastfm-similar-artists"]').exists()).toBe(true)
    // A missing Last.fm avatar renders the artist's initial, not a broken image.
    expect(wrapper.find('[data-test="lastfm-artist-initial"]').text()).toBe('A')
  })

  it('reports a partial build without hiding the rows', async () => {
    mocks.getRecommendations.mockResolvedValue(page({ status: 'partial' }))
    const wrapper = await mountView()

    expect(wrapper.find('[data-test="lastfm-partial"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="lastfm-combined"]').exists()).toBe(true)
  })

  it('renders a pending row as matching without actions', async () => {
    mocks.getRecommendations.mockResolvedValue(page({
      tracks: [pendingTrack('Pending')],
      recent_tracks: [],
      loved_tracks: [],
      top_tracks: [],
      similar_artists: [],
    }))
    const wrapper = await mountView()

    expect(wrapper.find('[data-test="recommendation-status"]').text()).toBe('lastfm.matching')
    expect(wrapper.find('[data-test="recommendation-play"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="recommendation-save"]').exists()).toBe(false)
  })

  it('fills a row in place when a match event arrives', async () => {
    mocks.getRecommendations.mockResolvedValue(page({
      tracks: [pendingTrack('Pending')],
      recent_tracks: [],
      loved_tracks: [],
      top_tracks: [],
      similar_artists: [],
    }))
    const wrapper = await mountView()

    await emitMatch({ username: 'listener', items: [track('Pending')], done: false })

    expect(wrapper.find('[data-test="recommendation-status"]').text()).toBe('wy')
    expect(wrapper.find('[data-test="recommendation-play"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="recommendation-save"]').exists()).toBe(true)
  })

  it('marks unresolved rows after the final event', async () => {
    mocks.getRecommendations.mockResolvedValue(page({
      tracks: [pendingTrack('Pending')],
      recent_tracks: [],
      loved_tracks: [],
      top_tracks: [],
      similar_artists: [],
    }))
    const wrapper = await mountView()

    await emitMatch({ username: 'listener', items: [], done: true })

    expect(wrapper.find('[data-test="recommendation-status"]').text()).toBe('lastfm.no_source')
    expect(wrapper.find('[data-test="recommendation-play"]').exists()).toBe(false)
  })

  it('returns rows to the matching state when refreshed', async () => {
    mocks.getRecommendations.mockResolvedValue(page({
      tracks: [pendingTrack('Pending')],
      recent_tracks: [],
      loved_tracks: [],
      top_tracks: [],
      similar_artists: [],
    }))
    const wrapper = await mountView()
    await emitMatch({ username: 'listener', items: [], done: true })
    expect(wrapper.find('[data-test="recommendation-status"]').text()).toBe('lastfm.no_source')

    await wrapper.find('[data-test="lastfm-refresh"]').trigger('click')
    await flushPromises()

    expect(mocks.getRecommendations).toHaveBeenLastCalledWith(true)
    expect(wrapper.find('[data-test="recommendation-status"]').text()).toBe('lastfm.matching')
  })

  it('releases the match listener on unmount', async () => {
    mocks.getRecommendations.mockResolvedValue(page())
    const wrapper = await mountView()

    wrapper.unmount()

    expect(mocks.off).toHaveBeenCalledTimes(1)
  })
})
