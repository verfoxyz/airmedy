import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick, reactive } from 'vue'
import LastFmListView from './LastFmListView.vue'

const mocks = vi.hoisted(() => ({
  push: vi.fn(),
  replace: vi.fn(),
  getList: vi.fn(),
  refreshList: vi.fn(),
  refreshItem: vi.fn(),
  preparePlayback: vi.fn(),
  save: vi.fn(),
  playTrackIds: vi.fn(),
  handlers: new Map<string, (event: { data?: unknown }) => void>(),
  off: vi.fn(),
  route: { params: { kind: 'top_tracks' } },
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
  useRoute: () => mocks.route,
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
  RefreshRecommendationItem: mocks.refreshItem,
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

async function emitEvent(name: string, data: unknown) {
  const handler = mocks.handlers.get(name)
  expect(handler).toBeDefined()
  handler!({ data })
  await nextTick()
}

async function emitMatch(data: unknown) {
  await emitEvent('lastfm:recommendations-matched', data)
}

describe('LastFmListView (one persisted list)', () => {
  beforeEach(() => {
    mocks.push.mockReset()
    mocks.replace.mockReset()
    mocks.getList.mockReset()
    mocks.refreshList.mockReset()
    mocks.refreshItem.mockReset()
    mocks.preparePlayback.mockReset()
    mocks.save.mockReset()
    mocks.playTrackIds.mockReset()
    mocks.handlers.clear()
    mocks.off.mockReset()
    document.body.innerHTML = ''
    mocks.route.params = reactive({ kind: 'top_tracks' })
    mocks.preparePlayback.mockResolvedValue('track-1')
    mocks.save.mockResolvedValue(true)
    mocks.playTrackIds.mockResolvedValue(undefined)
    mocks.refreshList.mockResolvedValue(undefined)
    mocks.refreshItem.mockResolvedValue(undefined)
  })

  it('renders the stored rows, mosaic, count and duration', async () => {
    mocks.getList.mockResolvedValue(list([item('One'), item('Two', 'matched')]))
    const wrapper = await mountView()

    expect(mocks.getList).toHaveBeenCalledWith('top_tracks')
    expect(wrapper.find('[data-test="lastfm-list-title"]').text()).toBe('lastfm.top')
    expect(wrapper.find('[data-test="lastfm-list-cover"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="lastfm-list-count"]').text()).toContain('2')
    expect(wrapper.find('[data-test="lastfm-list-duration"]').text()).toBe('6 common.min')
    const rows = wrapper.findAll('[data-test="top-track-row"]')
    expect(rows).toHaveLength(2)
    expect(rows[0].find('[data-test="top-track-status"]').attributes('data-state')).toBe('matching')
    expect(rows[1].find('[data-test="top-track-status"]').attributes('data-state')).toBe('matched')
  })

  it('builds the header mosaic from the row covers', async () => {
    const rows = [item('One', 'matched'), item('Two', 'matched')]
    rows[0].cover_url = 'https://cover/1.jpg'
    rows[1].cover_url = 'https://cover/2.jpg'
    mocks.getList.mockResolvedValue(list(rows))
    const wrapper = await mountView()

    expect(wrapper.find('[data-test="lastfm-list-cover"]').findAll('img')).toHaveLength(2)
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

  it('plays every playable row from the header', async () => {
    mocks.preparePlayback.mockImplementation(async (row: { title: string }) => `id-${row.title}`)
    mocks.getList.mockResolvedValue(list([
      item('Hit', 'matched'),
      item('Pending', 'matching'),
      item('Other', 'matched'),
    ]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-list-play"]').trigger('click')
    await flushPromises()

    expect(mocks.preparePlayback).toHaveBeenCalledTimes(2)
    expect(mocks.playTrackIds).toHaveBeenCalledWith(['id-Hit', 'id-Other'])
  })

  it('shuffles the playable rows from the header', async () => {
    mocks.preparePlayback.mockImplementation(async (row: { title: string }) => `id-${row.title}`)
    mocks.getList.mockResolvedValue(list([item('Only', 'matched')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-list-shuffle"]').trigger('click')
    await flushPromises()

    expect(mocks.playTrackIds).toHaveBeenCalledWith(['id-Only'])
  })

  it('refreshes this list from the header more menu', async () => {
    mocks.getList.mockResolvedValue(list([item('One')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-list-more"]').trigger('click')
    const label = [...document.body.querySelectorAll('span')].find(
      (node) => node.textContent === 'lastfm.refresh_list',
    )
    expect(label).toBeDefined()
    label!.closest('div')!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
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

    expect(mocks.off).toHaveBeenCalledTimes(2)
  })

  it('reloads when the route switches to another list', async () => {
    mocks.getList.mockResolvedValue(list([item('Top One')]))
    const wrapper = await mountView()
    expect(wrapper.find('[data-test="lastfm-list-title"]').text()).toBe('lastfm.top')

    mocks.getList.mockResolvedValue({
      ...list([item('Rec One', 'matched', 'similar')]),
      kind: 'similar',
      title: 'lastfm.similar_tracks',
    })
    mocks.route.params.kind = 'similar'
    await nextTick()
    await flushPromises()

    expect(mocks.getList).toHaveBeenLastCalledWith('similar')
    expect(wrapper.find('[data-test="lastfm-list-title"]').text()).toBe('lastfm.similar_tracks')
    expect(wrapper.findAll('[data-test="top-track-row"]')).toHaveLength(1)
  })

  it('appends progressively built rows without duplicates', async () => {
    mocks.getList.mockResolvedValue(list([item('One')]))
    const wrapper = await mountView()

    await emitEvent('lastfm:recommendation-items-added', {
      kind: 'top_tracks',
      items: [item('Two')],
    })
    expect(wrapper.findAll('[data-test="top-track-row"]')).toHaveLength(2)

    await emitEvent('lastfm:recommendation-items-added', {
      kind: 'top_tracks',
      items: [item('Two'), item('Three')],
    })
    expect(wrapper.findAll('[data-test="top-track-row"]')).toHaveLength(3)

    await emitEvent('lastfm:recommendation-items-added', {
      kind: 'similar',
      items: [item('Ignored', 'matching', 'similar')],
    })
    expect(wrapper.findAll('[data-test="top-track-row"]')).toHaveLength(3)
  })

  it('renders streamed rows before the list build finishes', async () => {
    mocks.route.params = reactive({ kind: 'similar' })
    let resolveList: (value: unknown) => void = () => {}
    mocks.getList.mockReturnValue(new Promise((resolve) => {
      resolveList = resolve
    }))
    const wrapper = await mountView()
    expect(wrapper.find('[data-test="lastfm-list-loading"]').exists()).toBe(true)

    await emitEvent('lastfm:recommendation-items-added', {
      kind: 'similar',
      items: [item('Early', 'matching', 'similar')],
    })
    expect(wrapper.find('[data-test="lastfm-list-loading"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-test="top-track-row"]')).toHaveLength(1)

    resolveList({ ...list([]), kind: 'similar', title: 'lastfm.similar_tracks' })
    await flushPromises()
    expect(wrapper.findAll('[data-test="top-track-row"]')).toHaveLength(1)
  })

  it('re-runs source matching for one row from the context menu', async () => {
    mocks.getList.mockResolvedValue(list([item('Hit', 'matched'), item('Other', 'matched')]))
    const wrapper = await mountView()

    await wrapper
      .findAll('[data-test="top-track-row"]')[0]
      .trigger('contextmenu', { clientX: 12, clientY: 18 })
    const label = [...document.body.querySelectorAll('span')].find((node) =>
      node.textContent?.includes('lastfm.refresh_item'),
    )
    expect(label).toBeDefined()
    label!.closest('div')!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await flushPromises()

    expect(mocks.refreshItem).toHaveBeenCalledWith('top_tracks', 'artist\u0000hit')
  })

  it('disables the row refresh while the row is already matching', async () => {
    mocks.getList.mockResolvedValue(list([item('Pending', 'matching')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="top-track-row"]').trigger('contextmenu')
    const label = [...document.body.querySelectorAll('span')].find((node) =>
      node.textContent?.includes('lastfm.refresh_item'),
    )
    expect(label!.closest('div')!.className).toContain('cursor-not-allowed')
  })
})
