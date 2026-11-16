import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, h, nextTick } from 'vue'
import LastFmView from './LastFmView.vue'

const mocks = vi.hoisted(() => ({
  push: vi.fn(),
  list: vi.fn(),
  refreshList: vi.fn(),
  refreshAll: vi.fn(),
  getList: vi.fn(),
  preparePlayback: vi.fn(),
  playTrackIds: vi.fn(),
  handlers: new Map<string, (event: { data?: unknown }) => void>(),
  off: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
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
  ListRecommendationLists: mocks.list,
  RefreshRecommendationList: mocks.refreshList,
  RefreshAllRecommendationLists: mocks.refreshAll,
  GetRecommendationList: mocks.getList,
}))

vi.mock('@/stores/catalog', () => ({
  useCatalogStore: () => ({ preparePlayback: mocks.preparePlayback }),
}))

vi.mock('@/stores/player', () => ({
  usePlayerStore: () => ({ playTrackIds: mocks.playTrackIds }),
}))

function summary(kind: string, count: number, covers: string[] = [], duration = 0) {
  return {
    kind,
    title: kind === 'similar' ? 'lastfm.similar_tracks' : 'lastfm.top',
    track_count: count,
    matching_count: 0,
    total_duration: duration,
    cover_urls: covers,
    updated_at: '2026-01-01T00:00:00Z',
  }
}

function trackRow(title: string, state: 'matched' | 'matching' | 'no_match' = 'matched') {
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
    kind: 'top_tracks',
    match_key: `artist\u0000${title.toLowerCase()}`,
  }
}

function collection(status = 'ok') {
  return {
    status,
    lists: status === 'not_connected'
      ? []
      : [summary('similar', 3, ['https://cover/a.jpg'], 380), summary('top_tracks', 50, [], 9000)],
  }
}

// The real grid virtualises through RecycleScroller, which is not installed in
// these unit tests; this stub renders every card so the assertions can see them.
const VirtualizedGridStub = defineComponent({
  props: { items: { type: Array, required: true } },
  setup(props, { slots }) {
    return () =>
      h(
        'div',
        (props.items as { id: string }[]).map((item) =>
          h('div', { key: item.id }, slots.default!({ item })),
        ),
      )
  },
})

async function mountView() {
  const wrapper = mount(LastFmView, {
    global: {
      mocks: { $t: (key: string) => key },
      stubs: { VirtualizedGrid: VirtualizedGridStub },
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

// The card transitions are released from requestAnimationFrame callbacks; the
// queue lets a test step exactly one frame at a time.
const frames = new Map<number, FrameRequestCallback>()
let frameId = 0

function runFrame() {
  const callbacks = [...frames.values()]
  frames.clear()
  for (const callback of callbacks) callback(0)
}

describe('LastFmView (persisted recommendation cards)', () => {
  beforeEach(() => {
    mocks.push.mockReset()
    mocks.list.mockReset()
    mocks.refreshList.mockReset()
    mocks.refreshAll.mockReset()
    mocks.getList.mockReset()
    mocks.preparePlayback.mockReset()
    mocks.playTrackIds.mockReset()
    mocks.handlers.clear()
    mocks.off.mockReset()
    document.body.innerHTML = ''
    mocks.refreshList.mockResolvedValue(undefined)
    mocks.refreshAll.mockResolvedValue(undefined)
    mocks.preparePlayback.mockImplementation(async (row: { title: string }) => `id-${row.title}`)
    mocks.playTrackIds.mockResolvedValue(undefined)
    frames.clear()
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
      frames.set(++frameId, callback)
      return frameId
    })
    vi.stubGlobal('cancelAnimationFrame', (id: number) => {
      frames.delete(id)
    })
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('guides the user to settings when Last.fm is not connected', async () => {
    mocks.list.mockResolvedValue(collection('not_connected'))
    const wrapper = await mountView()

    expect(wrapper.find('[data-test="lastfm-not-connected"]').exists()).toBe(true)
    await wrapper.find('[data-test="lastfm-open-settings"]').trigger('click')
    expect(mocks.push).toHaveBeenCalledWith('/settings/integrations')
  })

  it('renders one card per list with its count and covers', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()

    const cards = wrapper.findAll('[data-test="lastfm-card"]')
    expect(cards).toHaveLength(2)
    expect(cards[0].attributes('data-kind')).toBe('similar')
    expect(cards[0].text()).toContain('lastfm.similar_tracks')
    expect(cards[0].find('[data-test="lastfm-card-count"]').text()).toBe('3')
    expect(cards[1].attributes('data-kind')).toBe('top_tracks')
    expect(cards[1].find('[data-test="lastfm-card-count"]').text()).toBe('50')
    expect(cards[0].find('[data-test="lastfm-card-duration"]').text()).toBe('6 common.min')
    expect(cards[0].findAll('img')).toHaveLength(1)
  })

  it('opens the list detail when a card is clicked', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()

    await wrapper.findAll('[data-test="lastfm-card"]')[1].trigger('click')
    expect(mocks.push).toHaveBeenCalledWith('/lastfm/lists/top_tracks')
  })

  it('suppresses card transitions until the grid has settled', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()
    const card = () => wrapper.findAll('[data-test="lastfm-card"]')[0]

    expect(card().classes()).toContain('no-card-transition')

    runFrame()
    await nextTick()
    expect(card().classes()).toContain('no-card-transition')

    runFrame()
    await nextTick()
    expect(card().classes()).not.toContain('no-card-transition')
  })

  it('reuses the card element when the summaries reload', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()
    const before = wrapper.findAll('[data-test="lastfm-card"]')[0].element

    await emitMatch({ kind: 'top_tracks', items: [], done: true })
    await flushPromises()

    expect(wrapper.findAll('[data-test="lastfm-card"]')[0].element).toBe(before)
  })

  it('refreshes only the clicked card', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()

    const buttons = wrapper.findAll('[data-test="lastfm-card-refresh"]')
    await buttons[0].trigger('click')
    await flushPromises()

    expect(mocks.refreshList).toHaveBeenCalledTimes(1)
    expect(mocks.refreshList).toHaveBeenCalledWith('similar')
    expect(mocks.refreshAll).not.toHaveBeenCalled()
    expect(mocks.list).toHaveBeenCalledTimes(2)
  })

  it('refreshes every list from the header', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-refresh-all"]').trigger('click')
    await flushPromises()

    expect(mocks.refreshAll).toHaveBeenCalledTimes(1)
    expect(mocks.list).toHaveBeenCalledTimes(2)
  })

  it('reloads the cards when a worker run finishes', async () => {
    mocks.list.mockResolvedValue(collection())
    await mountView()
    expect(mocks.list).toHaveBeenCalledTimes(1)

    await emitMatch({ kind: 'top_tracks', items: [], done: true })
    await flushPromises()

    expect(mocks.list).toHaveBeenCalledTimes(2)
  })

  it('shows the empty state and keeps a refresh action', async () => {
    mocks.list.mockResolvedValue({ status: 'ok', lists: [] })
    const wrapper = await mountView()

    const empty = wrapper.find('[data-test="lastfm-empty"]')
    expect(empty.exists()).toBe(true)
    await empty.find('button').trigger('click')
    await flushPromises()
    expect(mocks.refreshAll).toHaveBeenCalledTimes(1)
  })

  it('plays every playable row of a card', async () => {
    mocks.list.mockResolvedValue(collection())
    mocks.getList.mockResolvedValue({
      kind: 'top_tracks',
      title: 'lastfm.top',
      items: [trackRow('One'), trackRow('Pending', 'matching'), trackRow('Two')],
      updated_at: '2026-01-01T00:00:00Z',
    })
    const wrapper = await mountView()

    await wrapper.findAll('[data-test="lastfm-card-play"]')[1].trigger('click')
    await flushPromises()

    expect(mocks.getList).toHaveBeenCalledWith('top_tracks')
    expect(mocks.preparePlayback).toHaveBeenCalledTimes(2)
    expect(mocks.playTrackIds).toHaveBeenCalledWith(['id-One', 'id-Two'])
    expect(mocks.push).not.toHaveBeenCalled()
  })

  it('shuffles the playable rows of a card', async () => {
    mocks.list.mockResolvedValue(collection())
    mocks.getList.mockResolvedValue({
      kind: 'top_tracks',
      title: 'lastfm.top',
      items: [trackRow('Only')],
      updated_at: '2026-01-01T00:00:00Z',
    })
    const wrapper = await mountView()

    await wrapper.findAll('[data-test="lastfm-card-shuffle"]')[1].trigger('click')
    await flushPromises()

    expect(mocks.playTrackIds).toHaveBeenCalledWith(['id-Only'])
  })

  it('offers refresh actions from the card more menu', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()

    await wrapper.findAll('[data-test="lastfm-card-more"]')[1].trigger('click')
    const labels = [...document.body.querySelectorAll('span')].map((node) => node.textContent)
    expect(labels).toContain('lastfm.refresh_list')
    expect(labels).toContain('lastfm.refresh_all')

    const refreshThis = [...document.body.querySelectorAll('span')].find(
      (node) => node.textContent === 'lastfm.refresh_list',
    )
    refreshThis!.closest('div')!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await flushPromises()
    expect(mocks.refreshList).toHaveBeenCalledWith('top_tracks')
  })

  it('releases the match listener on unmount', async () => {
    mocks.list.mockResolvedValue(collection())
    const wrapper = await mountView()

    wrapper.unmount()

    expect(mocks.off).toHaveBeenCalledTimes(1)
  })
})
