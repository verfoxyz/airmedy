import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import LastFmListView from './LastFmListView.vue'

const mocks = vi.hoisted(() => ({
  push: vi.fn(),
  replace: vi.fn(),
  getList: vi.fn(),
  refreshList: vi.fn(),
  preparePlayback: vi.fn(),
  save: vi.fn(),
  playTrackIds: vi.fn(),
  handlers: new Map<string, (event: { data?: unknown }) => void>(),
  off: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => {
      if (params?.source) return `${key}:${params.source}`
      return key
    },
  }),
}))

vi.mock('vue-router', () => ({
  useRoute: () => ({ params: { kind: 'top_tracks' } }),
  useRouter: () => ({ push: mocks.push, replace: mocks.replace }),
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
  GetRecommendationList: mocks.getList,
  RefreshRecommendationList: mocks.refreshList,
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

function item(title: string, state: RowState = 'matching', kind = 'top_tracks') {
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
    kind,
    match_key: `artist\u0000${title.toLowerCase()}`,
  }
}

function list(items: ReturnType<typeof item>[]) {
  return {
    kind: 'top_tracks',
    title: 'lastfm.top',
    items,
    updated_at: '2026-01-01T00:00:00Z',
  }
}

async function mountView() {
  const wrapper = mount(LastFmListView, {
    global: {
      mocks: { $t: (key: string) => key },
    },
  })
  await flushPromises()
  return wrapper
}

async function emitMatch(data: unknown) {
  const handler = mocks.handlers.get('lastfm:recommendations-matched')
  expect(handler).toBeDefined()
  handler!({ data })
  await nextTick()
}

describe('LastFmListView (one persisted list)', () => {
  beforeEach(() => {
    mocks.push.mockReset()
    mocks.replace.mockReset()
    mocks.getList.mockReset()
    mocks.refreshList.mockReset()
    mocks.preparePlayback.mockReset()
    mocks.save.mockReset()
    mocks.playTrackIds.mockReset()
    mocks.handlers.clear()
    mocks.off.mockReset()
    mocks.preparePlayback.mockResolvedValue('track-1')
    mocks.save.mockResolvedValue(true)
    mocks.playTrackIds.mockResolvedValue(undefined)
    mocks.refreshList.mockResolvedValue(undefined)
  })

  it('renders the stored rows and the list title', async () => {
    mocks.getList.mockResolvedValue(list([item('One'), item('Two', 'matched')]))
    const wrapper = await mountView()

    expect(mocks.getList).toHaveBeenCalledWith('top_tracks')
    expect(wrapper.find('[data-test="lastfm-list-title"]').text()).toBe('lastfm.top')
    expect(wrapper.find('[data-test="lastfm-list-count"]').text()).toBe('2')
    const rows = wrapper.findAll('[data-test="top-track-row"]')
    expect(rows).toHaveLength(2)
    expect(rows[0].find('[data-test="top-track-status"]').attributes('data-state')).toBe('matching')
    expect(rows[1].find('[data-test="top-track-status"]').attributes('data-state')).toBe('matched')
  })

  it('updates a row from the shared match stream', async () => {
    mocks.getList.mockResolvedValue(list([item('Hit')]))
    const wrapper = await mountView()

    await emitMatch({ kind: 'top_tracks', items: [item('Hit', 'matched')], done: false })

    const row = wrapper.find('[data-test="top-track-row"]')
    expect(row.find('[data-test="top-track-status"]').attributes('data-state')).toBe('matched')
    expect(row.find('[data-test="top-track-play"]').exists()).toBe(true)
  })

  it('ignores updates for another list', async () => {
    mocks.getList.mockResolvedValue(list([item('Hit')]))
    const wrapper = await mountView()

    await emitMatch({ kind: 'similar', items: [item('Hit', 'matched', 'similar')], done: true })

    const row = wrapper.find('[data-test="top-track-row"]')
    expect(row.find('[data-test="top-track-status"]').attributes('data-state')).toBe('matching')
  })

  it('marks unresolved rows after done', async () => {
    mocks.getList.mockResolvedValue(list([item('Missing')]))
    const wrapper = await mountView()

    await emitMatch({ kind: 'top_tracks', items: [], done: true })

    const row = wrapper.find('[data-test="top-track-row"]')
    expect(row.find('[data-test="top-track-status"]').attributes('data-state')).toBe('no-match')
    expect(row.find('[data-test="top-track-play"]').exists()).toBe(false)
  })

  it('plays and saves a matched row through the shared catalogue actions', async () => {
    mocks.getList.mockResolvedValue(list([item('Hit', 'matched')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="top-track-play"]').trigger('click')
    await flushPromises()
    expect(mocks.preparePlayback).toHaveBeenCalledWith(
      expect.objectContaining({ title: 'Hit', provider: 'wy' }),
    )
    expect(mocks.playTrackIds).toHaveBeenCalledWith(['track-1'])

    await wrapper.find('[data-test="top-track-save"]').trigger('click')
    await flushPromises()
    expect(mocks.save).toHaveBeenCalledWith(
      expect.objectContaining({ title: 'Hit', provider: 'wy' }),
      { favorite: true },
    )
  })

  it('refreshes only this list and reloads it', async () => {
    mocks.getList.mockResolvedValue(list([item('One')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-refresh"]').trigger('click')
    await flushPromises()

    expect(mocks.refreshList).toHaveBeenCalledWith('top_tracks')
    expect(mocks.getList).toHaveBeenCalledTimes(2)
  })

  it('returns to the cards page', async () => {
    mocks.getList.mockResolvedValue(list([item('One')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-back"]').trigger('click')
    expect(mocks.push).toHaveBeenCalledWith('/lastfm')
  })

  it('shows the empty state with a refresh action', async () => {
    mocks.getList.mockResolvedValue(list([]))
    const wrapper = await mountView()

    const empty = wrapper.find('[data-test="lastfm-list-empty"]')
    expect(empty.exists()).toBe(true)
    await empty.find('button').trigger('click')
    await flushPromises()
    expect(mocks.refreshList).toHaveBeenCalledWith('top_tracks')
  })

  it('releases the match listener on unmount', async () => {
    mocks.getList.mockResolvedValue(list([item('One')]))
    const wrapper = await mountView()

    wrapper.unmount()

    expect(mocks.off).toHaveBeenCalledTimes(1)
  })
})
