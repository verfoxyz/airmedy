import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick, reactive } from 'vue'
import LastFmListView from './LastFmListView.vue'
import DetailHero from '@/components/DetailHero.vue'

const mocks = vi.hoisted(() => ({
  push: vi.fn(),
  replace: vi.fn(),
  back: vi.fn(),
  getList: vi.fn(),
  refreshList: vi.fn(),
  refreshAll: vi.fn(),
  refreshItem: vi.fn(),
  getWeek: vi.fn(),
  refreshWeek: vi.fn(),
  loadMore: vi.fn(),
  preparePlayback: vi.fn(),
  save: vi.fn(),
  playTrackIds: vi.fn(),
  handlers: new Map<string, (event: { data?: unknown }) => void>(),
  off: vi.fn(),
  route: { name: 'lastfm-list', params: { kind: 'top_tracks' } as { kind?: string } },
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => {
      if (params && 'count' in params) return `${key}:${params.count}`
      if (params?.source) return `${key}:${params.source}`
      return key
    },
  }),
}))

vi.mock('vue-router', () => ({
  useRoute: () => mocks.route,
  useRouter: () => ({ push: mocks.push, replace: mocks.replace, back: mocks.back }),
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
  RefreshAllRecommendationLists: mocks.refreshAll,
  RefreshRecommendationItem: mocks.refreshItem,
  GetWeeklyEchoWeek: mocks.getWeek,
  RefreshWeeklyEchoWeek: mocks.refreshWeek,
  LoadMoreRecommendationItems: mocks.loadMore,
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

// jsdom has no IntersectionObserver; the fake keeps each instance so a test can
// drive the sentinel's intersection and assert the observer was disconnected.
class FakeIntersectionObserver {
  static instances: FakeIntersectionObserver[] = []
  readonly options: IntersectionObserverInit | undefined
  disconnected = false
  observed: Element[] = []
  private readonly callback: IntersectionObserverCallback

  constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
    this.callback = callback
    this.options = options
    FakeIntersectionObserver.instances.push(this)
  }

  observe(target: Element) {
    this.observed.push(target)
  }

  unobserve(target: Element) {
    this.observed = this.observed.filter((element) => element !== target)
  }

  disconnect() {
    this.disconnected = true
    this.observed = []
  }

  takeRecords() {
    return []
  }

  trigger(isIntersecting: boolean) {
    this.callback(
      [{ isIntersecting } as IntersectionObserverEntry],
      this as unknown as IntersectionObserver,
    )
  }
}

function lastObserver() {
  return FakeIntersectionObserver.instances.at(-1)!
}

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
    play_count: 0,
    week_from: 0,
    week_to: 0,
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
    week_from: 0,
    week_to: 0,
    prev_from: 0,
    next_from: 0,
    has_more: false,
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
    mocks.back.mockReset()
    mocks.getList.mockReset()
    mocks.refreshList.mockReset()
    mocks.refreshAll.mockReset()
    mocks.refreshItem.mockReset()
    mocks.getWeek.mockReset()
    mocks.refreshWeek.mockReset()
    mocks.loadMore.mockReset()
    mocks.preparePlayback.mockReset()
    mocks.save.mockReset()
    mocks.playTrackIds.mockReset()
    mocks.handlers.clear()
    mocks.off.mockReset()
    document.body.innerHTML = ''
    mocks.route.name = 'lastfm-list'
    mocks.route.params = reactive({ kind: 'top_tracks' })
    mocks.preparePlayback.mockResolvedValue('track-1')
    mocks.save.mockResolvedValue(true)
    mocks.playTrackIds.mockResolvedValue(undefined)
    mocks.refreshList.mockResolvedValue(undefined)
    mocks.refreshAll.mockResolvedValue(undefined)
    mocks.refreshItem.mockResolvedValue(undefined)
    mocks.refreshWeek.mockResolvedValue(undefined)
    FakeIntersectionObserver.instances = []
    vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
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

  it('switches a promoted row to the in-library chip', async () => {
    mocks.getList.mockResolvedValue(list([item('Hit', 'matched')]))
    const wrapper = await mountView()

    const promoted = item('Hit', 'matched')
    promoted.state = 'in_library'
    promoted.local_track_id = 'local-1'
    promoted.provider = ''
    promoted.external_id = ''
    promoted.sources = []
    promoted.downloadable = false
    await emitMatch({ kind: 'top_tracks', items: [promoted], done: false })

    const row = wrapper.find('[data-test="top-track-row"]')
    expect(row.find('[data-test="top-track-status"]').text()).toBe('lastfm.in_library')
    expect(row.find('[data-test="top-track-save"]').exists()).toBe(false)
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

  it('browses weekly echo history with the window and play counts', async () => {
    mocks.route.params = reactive({ kind: 'weekly_echo' })
    const rows = [item('Echo', 'matched')]
    rows[0].play_count = 7
    const latest = {
      ...list(rows),
      kind: 'weekly_echo',
      title: 'lastfm.weekly_echo',
      week_from: 200,
      week_to: 300,
      prev_from: 100,
      next_from: 0,
    }
    mocks.getList.mockResolvedValue(latest)
    const wrapper = await mountView()

    expect(wrapper.find('[data-test="lastfm-week-nav"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="lastfm-week-range"]').text()).toMatch(
      /^\d{2}\/\d{2} – \d{2}\/\d{2}$/,
    )
    expect(wrapper.find('[data-test="top-track-plays"]').text()).toBe('lastfm.plays_count:7')
    expect(wrapper.find('[data-test="lastfm-week-prev"]').attributes('disabled')).toBeUndefined()
    expect(wrapper.find('[data-test="lastfm-week-next"]').attributes('disabled')).toBeDefined()

    mocks.getWeek.mockResolvedValue({
      ...latest,
      week_from: 100,
      week_to: 200,
      prev_from: 0,
      next_from: 200,
    })
    await wrapper.find('[data-test="lastfm-week-prev"]').trigger('click')
    await flushPromises()

    expect(mocks.getWeek).toHaveBeenCalledWith(100)
    expect(wrapper.find('[data-test="lastfm-week-next"]').attributes('disabled')).toBeUndefined()
  })

  it('appends another discover page from the load-more button', async () => {
    mocks.route.params = reactive({ kind: 'discover' })
    const first = {
      ...list([item('One', 'matched')]),
      kind: 'discover',
      title: 'lastfm.discover',
      has_more: true,
    }
    mocks.getList.mockResolvedValue(first)
    const wrapper = await mountView()

    const button = () => wrapper.find('[data-test="lastfm-load-more"]')
    expect(button().text()).toBe('lastfm.load_more')

    mocks.loadMore.mockResolvedValue({
      ...first,
      items: [item('One', 'matched'), item('Two', 'matched')],
      has_more: false,
    })
    await button().trigger('click')
    await flushPromises()

    expect(mocks.loadMore).toHaveBeenCalledWith('discover')
    expect(wrapper.findAll('[data-test="top-track-row"]')).toHaveLength(2)
    expect(button().text()).toBe('lastfm.no_more')
    expect(button().attributes('disabled')).toBeDefined()
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

  it('keeps playing the other rows when one row cannot be prepared', async () => {
    mocks.preparePlayback.mockImplementation(async (row: { title: string }) => {
      if (row.title === 'Broken') throw new Error('no source')
      return `id-${row.title}`
    })
    mocks.getList.mockResolvedValue(list([
      item('Hit', 'matched'),
      item('Broken', 'matched'),
      item('Other', 'matched'),
    ]))
    const warning = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-list-play"]').trigger('click')
    await flushPromises()

    expect(mocks.playTrackIds).toHaveBeenCalledWith(['id-Hit', 'id-Other'])
    expect(wrapper.find('[data-test="lastfm-play-notice"]').text()).toBe('lastfm.play_skipped:1')
    expect(warning).toHaveBeenCalled()
    warning.mockRestore()
  })

  it('words an all-matching list instead of doing nothing', async () => {
    mocks.getList.mockResolvedValue(list([item('One'), item('Two')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-list-play"]').trigger('click')
    await flushPromises()

    expect(mocks.preparePlayback).not.toHaveBeenCalled()
    expect(mocks.playTrackIds).not.toHaveBeenCalled()
    expect(wrapper.find('[data-test="lastfm-play-notice"]').text()).toBe('lastfm.play_matching')
  })

  it('words a list with no playable row', async () => {
    mocks.getList.mockResolvedValue(list([item('Gone', 'no_match'), item('Also gone', 'no_match')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-list-play"]').trigger('click')
    await flushPromises()

    expect(mocks.playTrackIds).not.toHaveBeenCalled()
    expect(wrapper.find('[data-test="lastfm-play-notice"]').text()).toBe('lastfm.play_unavailable')
  })

  it('plays the matched subset and words the skipped rows', async () => {
    mocks.preparePlayback.mockImplementation(async (row: { title: string }) => `id-${row.title}`)
    mocks.getList.mockResolvedValue(list([
      item('Hit', 'matched'),
      item('Gone', 'no_match'),
      item('Other', 'matched'),
    ]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-list-play"]').trigger('click')
    await flushPromises()

    expect(mocks.playTrackIds).toHaveBeenCalledWith(['id-Hit', 'id-Other'])
    expect(wrapper.find('[data-test="lastfm-play-notice"]').text()).toBe('lastfm.play_skipped:1')
  })

  it('ignores a second click while the list is still resolving', async () => {
    mocks.getList.mockResolvedValue(list([item('Hit', 'matched')]))
    let release: (id: string) => void = () => {}
    mocks.preparePlayback.mockReturnValue(new Promise<string>((resolve) => {
      release = resolve
    }))
    const wrapper = await mountView()

    const button = wrapper.find('[data-test="lastfm-list-play"]')
    await button.trigger('click')
    await button.trigger('click')

    expect(mocks.preparePlayback).toHaveBeenCalledTimes(1)

    release('id-Hit')
    await flushPromises()
    expect(mocks.playTrackIds).toHaveBeenCalledTimes(1)
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

  it('shows the sticky bar when the hero title leaves the scroller', async () => {
    mocks.getList.mockResolvedValue(list([item('One'), item('Two', 'matched')]))
    const wrapper = await mountView()
    const bar = wrapper.find('[data-test="lastfm-sticky-bar"]')
    const observer = lastObserver()

    expect(observer).toBeDefined()
    expect(bar.attributes('data-visible')).toBe('false')

    observer.trigger(false)
    await nextTick()
    expect(bar.attributes('data-visible')).toBe('true')
    expect(bar.find('[role="toolbar"]').attributes('aria-label')).toBe('lastfm.top')

    observer.trigger(true)
    await nextTick()
    expect(bar.attributes('data-visible')).toBe('false')
  })

  it('returns from the sticky bar', async () => {
    mocks.getList.mockResolvedValue(list([item('One')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-sticky-back"]').trigger('click')
    expect(mocks.back).toHaveBeenCalledTimes(1)
  })

  it('plays from the sticky bar like the hero', async () => {
    mocks.preparePlayback.mockImplementation(async (row: { title: string }) => `id-${row.title}`)
    mocks.getList.mockResolvedValue(list([item('Hit', 'matched'), item('Other', 'matched')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-sticky-play"]').trigger('click')
    await flushPromises()

    expect(mocks.playTrackIds).toHaveBeenCalledWith(['id-Hit', 'id-Other'])
  })

  it('filters from the sticky search and closes it on Escape', async () => {
    mocks.getList.mockResolvedValue(list([item('One', 'matched'), item('Two', 'matched')]))
    const wrapper = await mountView()

    await wrapper.find('[data-test="lastfm-sticky-search"]').trigger('click')
    const input = wrapper.find('[data-test="lastfm-sticky-search-input"]')
    expect(input.exists()).toBe(true)

    await input.setValue('Two')
    const rows = wrapper.findAll('[data-test="top-track-row"]')
    expect(rows).toHaveLength(1)
    expect(rows[0].text()).toContain('Two')

    await input.trigger('keydown', { key: 'Escape' })
    expect(wrapper.find('[data-test="lastfm-sticky-search-input"]').exists()).toBe(false)
  })

  it('disconnects the title observer on unmount', async () => {
    mocks.getList.mockResolvedValue(list([item('One')]))
    const wrapper = await mountView()
    const observer = lastObserver()

    wrapper.unmount()

    expect(observer.disconnected).toBe(true)
  })

  it('returns to the cards page', async () => {
    mocks.getList.mockResolvedValue(list([item('One')]))
    const wrapper = await mountView()

    await wrapper.findComponent(DetailHero).find('button').trigger('click')
    expect(mocks.back).toHaveBeenCalledTimes(1)
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

  it('does not cancel the navigation when leaving the list route', async () => {
    mocks.getList.mockResolvedValue(list([item('One')]))
    await mountView()

    // Leaving the route drops :kind. The listener must not redirect back to
    // /lastfm, which would replace the navigation the user just started.
    mocks.route.name = 'albums'
    mocks.route.params.kind = undefined
    await nextTick()
    await flushPromises()

    expect(mocks.replace).not.toHaveBeenCalled()
  })

  it('falls back to the cards page for an unknown list kind', async () => {
    mocks.getList.mockResolvedValue(list([item('One')]))
    await mountView()

    mocks.route.params.kind = 'unknown'
    await nextTick()
    await flushPromises()

    expect(mocks.replace).toHaveBeenCalledWith('/lastfm')
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
    expect(wrapper.find('[data-test="lastfm-list-title"]').exists()).toBe(false)

    await emitEvent('lastfm:recommendation-items-added', {
      kind: 'similar',
      items: [item('Early', 'matching', 'similar')],
    })
    expect(wrapper.find('[data-test="lastfm-list-title"]').exists()).toBe(true)
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
