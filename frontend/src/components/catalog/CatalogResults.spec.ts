import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import CatalogResults from './CatalogResults.vue'
import { usePlaylistsStore } from '@/stores/playlists'
import * as CatalogService from '../../../bindings/airmedy/internal/infra/wails/catalogservice'
import type {
  CatalogCoverDTO,
  CatalogCoverRequestDTO,
  CatalogFailureDTO,
  CatalogRowDTO,
  CatalogSourceDTO,
} from '../../../bindings/airmedy/internal/infra/wails/models'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => (params?.source ? `${key}:${params.source}` : key) }),
}))

vi.mock('@wailsio/runtime', async (importOriginal) => ({
  ...await importOriginal<typeof import('@wailsio/runtime')>(),
  Events: { On: vi.fn(() => () => {}) },
}))

// The playlists store loads through this binding; the component only reads the
// store, so a stub that never resolves keeps the test offline.
vi.mock('../../../bindings/airmedy/internal/infra/wails/playlistservice', () => ({
  GetAllPlaylists: vi.fn().mockResolvedValue([]),
  GetPlaylistByID: vi.fn().mockResolvedValue(null),
}))

// The cover batch is resolved by the host. The default implementation answers
// one local asset URL per request; individual tests override it to fail a row.
vi.mock('../../../bindings/airmedy/internal/infra/wails/catalogservice', () => ({
  GetCatalogCovers: vi.fn(),
}))

// --- Cover loading doubles ---------------------------------------------------
//
// jsdom has no layout or image fetching, so `Image` and `IntersectionObserver`
// are faked: the test decides when a row becomes visible and when a preload
// settles. The host RPC is mocked above and returns local URLs.

class FakeIntersectionObserver {
  static instances: FakeIntersectionObserver[] = []

  readonly observed: Element[] = []
  readonly root: Element | null
  readonly rootMargin: string
  disconnected = false

  constructor(
    private readonly callback: (
      entries: Array<{ target: Element; isIntersecting: boolean }>,
      observer: FakeIntersectionObserver,
    ) => void,
    options?: { root?: Element | Document | null; rootMargin?: string; threshold?: number },
  ) {
    this.root = (options?.root as Element | null) ?? null
    this.rootMargin = options?.rootMargin ?? '0px'
    FakeIntersectionObserver.instances.push(this)
  }

  observe(element: Element) {
    if (!this.observed.includes(element)) this.observed.push(element)
  }

  unobserve(element: Element) {
    const index = this.observed.indexOf(element)
    if (index >= 0) this.observed.splice(index, 1)
  }

  disconnect() {
    this.disconnected = true
    this.observed.length = 0
  }

  intersect() {
    this.intersectElement(this.observed[0])
  }

  intersectElement(element: Element | undefined) {
    if (!element) return
    this.callback([{ target: element, isIntersecting: true }], this)
  }

  intersectAll() {
    for (const element of [...this.observed]) {
      this.intersectElement(element)
    }
  }
}

class FakeImage {
  static instances: FakeImage[] = []

  onload: (() => void) | null = null
  onerror: (() => void) | null = null
  src = ''

  constructor() {
    FakeImage.instances.push(this)
  }

  succeed() {
    this.onload?.()
  }

  fail() {
    this.onerror?.()
  }
}

vi.stubGlobal('Image', FakeImage)
vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)

/** answerCovers makes every requested row resolve to a local asset URL. */
function answerCovers(requests: CatalogCoverRequestDTO[]): CatalogCoverDTO[] {
  return requests.map((request) => ({
    provider: request.provider,
    external_id: request.external_id,
    url: `/cover-cache/${request.external_id}.jpg`,
    error: '',
  }))
}

const SOURCES: CatalogSourceDTO[] = [
  { id: 'wy', name: 'NetEase' },
  { id: 'kg', name: 'Kugou' },
]

function row(overrides: Partial<CatalogRowDTO> = {}): CatalogRowDTO {
  return {
    provider: 'wy',
    external_id: '509781655',
    title: '晴天',
    artists: ['周杰伦'],
    album_name: '叶惠美',
    duration: 239,
    sources: ['wy'],
    cover_url: '',
    cover_url_large: '',
    downloadable: true,
    blocked_reason: '',
    state: '',
    ...overrides,
  }
}

const EMPTY_PROPS = {
  sources: [] as CatalogSourceDTO[],
  activeSources: [] as string[],
  rows: [] as CatalogRowDTO[],
  failures: [] as CatalogFailureDTO[],
  hasMore: false,
  loading: false,
  loadingMore: false,
  errorKind: '',
  actionError: '',
  hasQuery: true,
}

function mountResults(props: Partial<typeof EMPTY_PROPS> = {}) {
  return mount(CatalogResults, {
    props: { ...EMPTY_PROPS, ...props },
    global: {
      plugins: [createPinia()],
      mocks: { $t: (key: string) => key },
    },
    attachTo: document.body,
  })
}

describe('CatalogResults', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    FakeImage.instances = []
    FakeIntersectionObserver.instances = []
    vi.mocked(CatalogService.GetCatalogCovers).mockImplementation(
      async (requests: CatalogCoverRequestDTO[]) => answerCovers(requests),
    )
  })

  it('renders 全部 plus one button per source and toggles the chosen source', async () => {
    const wrapper = mountResults({ sources: SOURCES })

    const buttons = wrapper.findAll('[data-test="catalog-source"]')
    expect(buttons).toHaveLength(3)
    expect(buttons[0].text()).toBe('cloud_search.all_sources')
    expect(buttons[1].text()).toBe('NetEase')

    await buttons[2].trigger('click')
    expect(wrapper.emitted('toggle-source')?.[0]).toEqual(['kg'])
    wrapper.unmount()
  })

  it('renders a row as title plus "artist · source"', () => {
    const wrapper = mountResults({ sources: SOURCES, rows: [row()] })

    const listRow = wrapper.find('[data-test="catalog-row"]')
    expect(listRow.text()).toContain('晴天')
    // The artist is the subtitle; the source is a separate badge.
    expect(listRow.text()).toContain('周杰伦')
    expect(listRow.get('[data-test="catalog-row-source"]').text()).toBe('NetEase')
    wrapper.unmount()
  })

  it('labels every source of a cross-source duplicate row', () => {
    const wrapper = mountResults({
      sources: SOURCES,
      rows: [row({ sources: ['wy', 'kg'] })],
    })

    expect(wrapper.findAll('[data-test="catalog-row-source"]').map(badge => badge.text())).toEqual(['NetEase', 'Kugou'])
    wrapper.unmount()
  })

  it('does not request covers for rows that never entered the viewport', async () => {
    const rows = ['a', 'b', 'c'].map((id) =>
      row({ external_id: id, cover_url: `https://cdn.example/${id}.jpg` }),
    )
    const wrapper = mountResults({ sources: SOURCES, rows })
    await flushPromises()

    const observer = FakeIntersectionObserver.instances.at(-1)!
    expect(observer.rootMargin).toContain('200px')
    expect(observer.observed).toHaveLength(3)
    expect(CatalogService.GetCatalogCovers).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('requests only the row that entered the viewport', async () => {
    const rows = ['a', 'b', 'c'].map((id) =>
      row({ external_id: id, cover_url: `https://cdn.example/${id}.jpg` }),
    )
    const wrapper = mountResults({ sources: SOURCES, rows })
    await flushPromises()

    const observer = FakeIntersectionObserver.instances.at(-1)!
    const target = observer.observed[1] as HTMLElement
    expect(target.dataset.rowIndex).toBe('1')
    observer.intersectElement(target)
    await flushPromises()

    const calls = vi.mocked(CatalogService.GetCatalogCovers).mock.calls
    expect(calls).toHaveLength(1)
    expect((calls[0][0] as CatalogCoverRequestDTO[]).map((item) => item.external_id)).toEqual(['b'])
    wrapper.unmount()
  })

  it('does not observe rows without a cover', async () => {
    const rows = [
      row({ external_id: 'w', cover_url: 'https://cdn.example/w.jpg' }),
      row({ provider: 'kg', external_id: 'k', sources: ['kg'] }),
    ]
    const wrapper = mountResults({ sources: SOURCES, rows })
    await flushPromises()

    const observer = FakeIntersectionObserver.instances.at(-1)!
    expect(observer.observed).toHaveLength(1)
    observer.intersectAll()
    await flushPromises()

    const calls = vi.mocked(CatalogService.GetCatalogCovers).mock.calls
    expect(calls).toHaveLength(1)
    expect((calls[0][0] as CatalogCoverRequestDTO[]).map((item) => item.external_id)).toEqual(['w'])
    wrapper.unmount()
  })

  it('batches rows visible in one turn into one host call', async () => {
    const rows = ['a', 'b', 'c'].map((id) =>
      row({ external_id: id, cover_url: `https://cdn.example/${id}.jpg` }),
    )
    const wrapper = mountResults({ sources: SOURCES, rows })
    await flushPromises()

    FakeIntersectionObserver.instances.at(-1)!.intersectAll()
    await flushPromises()

    const calls = vi.mocked(CatalogService.GetCatalogCovers).mock.calls
    expect(calls).toHaveLength(1)
    expect((calls[0][0] as CatalogCoverRequestDTO[]).map((item) => item.external_id)).toEqual(['a', 'b', 'c'])
    wrapper.unmount()
  })

  it('splits more than ten visible rows into two host calls', async () => {
    const rows = Array.from({ length: 12 }, (_, index) =>
      row({ external_id: `r${index}`, cover_url: `https://cdn.example/${index}.jpg` }),
    )
    const wrapper = mountResults({ sources: SOURCES, rows })
    await flushPromises()

    FakeIntersectionObserver.instances.at(-1)!.intersectAll()
    await flushPromises()
    await flushPromises()

    const calls = vi.mocked(CatalogService.GetCatalogCovers).mock.calls
    expect(calls).toHaveLength(2)
    expect((calls[0][0] as CatalogCoverRequestDTO[])).toHaveLength(10)
    expect((calls[1][0] as CatalogCoverRequestDTO[])).toHaveLength(2)
    wrapper.unmount()
  })

  it('requests a visible row only once', async () => {
    const rows = [row({ external_id: 'a', cover_url: 'https://cdn.example/a.jpg' })]
    const wrapper = mountResults({ sources: SOURCES, rows })
    await flushPromises()

    const observer = FakeIntersectionObserver.instances.at(-1)!
    const target = observer.observed[0]
    observer.intersectElement(target)
    await flushPromises()
    observer.intersectElement(target)
    await flushPromises()

    expect(CatalogService.GetCatalogCovers).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('preloads covers and renders host-local asset URLs', async () => {
    const wrapper = mountResults({
      sources: SOURCES,
      rows: [row({ cover_url: 'https://p1.music.126.net/cover.jpg?param=100y100' })],
    })
    await flushPromises()

    FakeIntersectionObserver.instances.at(-1)!.intersectAll()
    await flushPromises()

    expect(FakeImage.instances).toHaveLength(1)
    expect(FakeImage.instances[0].src).toBe('/cover-cache/509781655.jpg')

    FakeImage.instances[0].succeed()
    await flushPromises()

    const img = wrapper.get('[data-test="catalog-cover"]')
    expect(img.attributes('src')).toBe('/cover-cache/509781655.jpg')
    // The native hint is gone: the queue decides when to load.
    expect(img.attributes('loading')).toBeUndefined()
    wrapper.unmount()
  })

  it('falls back to the placeholder when the host reports an error and never retries it', async () => {
    vi.mocked(CatalogService.GetCatalogCovers).mockImplementation(
      async (requests: CatalogCoverRequestDTO[]) =>
        requests.map((request) => ({
          provider: request.provider,
          external_id: request.external_id,
          url: '',
          error: 'unavailable',
        })),
    )
    const wrapper = mountResults({
      sources: SOURCES,
      rows: [row({ cover_url: 'https://p1.music.126.net/cover.jpg?param=100y100' })],
    })
    await flushPromises()

    const observer = FakeIntersectionObserver.instances.at(-1)!
    const target = observer.observed[0]
    observer.intersectElement(target)
    await flushPromises()

    expect(wrapper.find('[data-test="catalog-cover"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="catalog-cover-placeholder"]').exists()).toBe(true)
    expect(FakeImage.instances).toHaveLength(0)

    observer.intersectElement(target)
    await flushPromises()
    expect(CatalogService.GetCatalogCovers).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('falls back to the placeholder if the rendered image errors', async () => {
    const wrapper = mountResults({
      sources: SOURCES,
      rows: [row({ cover_url: 'https://p1.music.126.net/cover.jpg?param=100y100' })],
    })
    await flushPromises()
    FakeIntersectionObserver.instances.at(-1)!.intersectAll()
    await flushPromises()
    FakeImage.instances[0].succeed()
    await flushPromises()

    await wrapper.get('[data-test="catalog-cover"]').trigger('error')
    expect(wrapper.find('[data-test="catalog-cover"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="catalog-cover-placeholder"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('keeps at most four cover images in flight', async () => {
    const rows = Array.from({ length: 10 }, (_, index) =>
      row({ external_id: String(index), cover_url: `https://cdn.example/${index}.jpg` }),
    )
    const wrapper = mountResults({ sources: SOURCES, rows })
    await flushPromises()
    FakeIntersectionObserver.instances.at(-1)!.intersectAll()
    await flushPromises()

    const exposed = wrapper.vm as unknown as {
      coverQueueSize: () => number
      coverInFlightCount: () => number
    }
    expect(FakeImage.instances).toHaveLength(4)
    expect(exposed.coverInFlightCount()).toBe(4)
    expect(exposed.coverQueueSize()).toBe(6)

    // One completion admits exactly one queued image.
    FakeImage.instances[0].succeed()
    await flushPromises()
    expect(FakeImage.instances).toHaveLength(5)
    expect(exposed.coverInFlightCount()).toBe(4)
    expect(exposed.coverQueueSize()).toBe(5)
    wrapper.unmount()
  })

  it('clears pending covers and detaches images on unmount', async () => {
    const rows = Array.from({ length: 10 }, (_, index) =>
      row({ external_id: String(index), cover_url: `https://cdn.example/${index}.jpg` }),
    )
    const wrapper = mountResults({ sources: SOURCES, rows })
    await flushPromises()
    FakeIntersectionObserver.instances.at(-1)!.intersectAll()
    await flushPromises()

    const exposed = wrapper.vm as unknown as { coverQueueSize: () => number }
    const queued = exposed.coverQueueSize
    expect(queued()).toBe(6)
    const observer = FakeIntersectionObserver.instances.at(-1)!

    wrapper.unmount()

    expect(observer.disconnected).toBe(true)
    expect(queued()).toBe(0)
    // In-flight callbacks are detached, so a late image cannot touch unmounted state.
    for (const image of FakeImage.instances) {
      expect(image.onload).toBeNull()
      expect(image.onerror).toBeNull()
    }
    FakeImage.instances[0].succeed()
    expect(FakeImage.instances).toHaveLength(4)
  })

  it('observes appended rows and requests them when they become visible', async () => {
    const initial = ['a', 'b'].map((id) =>
      row({ external_id: id, cover_url: `https://cdn.example/${id}.jpg` }),
    )
    const wrapper = mountResults({ sources: SOURCES, rows: initial })
    await flushPromises()

    expect(FakeIntersectionObserver.instances.at(-1)!.observed).toHaveLength(2)

    await wrapper.setProps({
      rows: [...initial, row({ external_id: 'c', cover_url: 'https://cdn.example/c.jpg' })],
    })
    await flushPromises()

    const observer = FakeIntersectionObserver.instances.at(-1)!
    expect(observer.observed).toHaveLength(3)
    observer.intersectElement(observer.observed[2])
    await flushPromises()

    const calls = vi.mocked(CatalogService.GetCatalogCovers).mock.calls
    expect(calls).toHaveLength(1)
    expect((calls[0][0] as CatalogCoverRequestDTO[]).map((item) => item.external_id)).toEqual(['c'])
    wrapper.unmount()
  })

  it('a new search resets the requested covers', async () => {
    const firstPage = ['a', 'b'].map((id) =>
      row({ external_id: id, cover_url: `https://cdn.example/${id}.jpg` }),
    )
    const wrapper = mountResults({ sources: SOURCES, rows: firstPage })
    await flushPromises()
    FakeIntersectionObserver.instances.at(-1)!.intersectAll()
    await flushPromises()
    expect(CatalogService.GetCatalogCovers).toHaveBeenCalledTimes(1)

    const nextPage = ['c', 'd'].map((id) =>
      row({ external_id: id, cover_url: `https://cdn.example/${id}.jpg` }),
    )
    await wrapper.setProps({ rows: nextPage })
    await flushPromises()
    FakeIntersectionObserver.instances.at(-1)!.intersectAll()
    await flushPromises()

    const calls = vi.mocked(CatalogService.GetCatalogCovers).mock.calls
    expect(calls).toHaveLength(2)
    expect((calls[1][0] as CatalogCoverRequestDTO[]).map((item) => item.external_id)).toEqual(['c', 'd'])
    wrapper.unmount()
  })

  it('renders the placeholder when the row has no cover', () => {
    const wrapper = mountResults({ sources: SOURCES, rows: [row()] })

    expect(wrapper.find('[data-test="catalog-cover"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="catalog-cover-placeholder"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('shows the library state and hides the actions for a track already owned', () => {
    const wrapper = mountResults({ sources: SOURCES, rows: [row({ state: 'in_library' })] })

    expect(wrapper.find('[data-test="catalog-state"]').text()).toBe('cloud_search.in_library')
    // Acting on a row already in the library must not be offered.
    expect(wrapper.find('[data-test="catalog-favorite"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows the downloading state while a job is in flight', () => {
    const wrapper = mountResults({ sources: SOURCES, rows: [row({ state: 'downloading' })] })

    expect(wrapper.find('[data-test="catalog-state"]').text()).toBe('cloud_search.downloading')
    wrapper.unmount()
  })

  it('shows the saved state for a metadata-only save and keeps the actions', () => {
    const wrapper = mountResults({ sources: SOURCES, rows: [row({ state: 'saved' })] })

    expect(wrapper.find('[data-test="catalog-state"]').text()).toBe('cloud_search.saved')
    // Saved is not downloaded: the row must still offer the download actions.
    expect(wrapper.find('[data-test="catalog-favorite"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('keeps the actions for an imported match and offers download-anyway', async () => {
    const wrapper = mountResults({
      sources: SOURCES,
      rows: [row({ state: 'in_library', local_track_id: 'local-1' })],
    })

    // The song is on disk through the imported row, so favorite/playlist still
    // work (the backend reuses that row), and a fresh download stays available.
    expect(wrapper.find('[data-test="catalog-favorite"]').exists()).toBe(true)
    const anyway = wrapper.find('[data-test="catalog-download-anyway"]')
    expect(anyway.exists()).toBe(true)

    await anyway.trigger('click')
    expect(wrapper.emitted('force-download')?.[0]?.[0]).toMatchObject({ provider: 'wy' })
    wrapper.unmount()
  })

  it('explains why an undownloadable row is disabled', () => {
    const wrapper = mountResults({
      sources: SOURCES,
      rows: [row({ downloadable: false, blocked_reason: 'no_resolver' })],
    })

    expect(wrapper.text()).toContain('cloud_search.blocked_no_resolver')
    expect(wrapper.find('[data-test="catalog-favorite"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('emits favorite for a downloadable row', async () => {
    const wrapper = mountResults({ sources: SOURCES, rows: [row()] })

    await wrapper.find('[data-test="catalog-favorite"]').trigger('click')
    expect(wrapper.emitted('favorite')?.[0]?.[0]).toMatchObject({ provider: 'wy', external_id: '509781655' })
    wrapper.unmount()
  })

  it('opens the playlist menu and emits the destination playlist', async () => {
    const wrapper = mountResults({ sources: SOURCES, rows: [row()] })
    const playlists = usePlaylistsStore()
    playlists.playlists = [
      { id: 'pl-1', name: 'Road trip', is_smart: false },
      { id: 'pl-2', name: 'Night', is_smart: false },
    ] as never

    await wrapper.find('[data-test="catalog-playlist"]').trigger('click')
    await flushPromises()
    await wrapper.vm.$nextTick()

    // ContextMenu teleports to <body> and renders its items as divs, so the
    // click target is found outside the wrapper's own tree.
    const items = Array.from(document.querySelectorAll('div')).filter(
      (item) => item.textContent?.trim() === 'Road trip',
    )
    expect(items).toHaveLength(1)
    items[0].click()
    await flushPromises()

    expect(wrapper.emitted('add-to-playlist')?.[0]).toEqual([expect.objectContaining({ provider: 'wy' }), 'pl-1'])
    wrapper.unmount()
  })

  it('offers load-more only when the backend says there is more', async () => {
    const withMore = mountResults({ sources: SOURCES, rows: [row()], hasMore: true })
    await withMore.find('[data-test="catalog-load-more"]').trigger('click')
    expect(withMore.emitted('load-more')).toHaveLength(1)
    withMore.unmount()

    const lastPage = mountResults({ sources: SOURCES, rows: [row()], hasMore: false })
    expect(lastPage.find('[data-test="catalog-load-more"]').exists()).toBe(false)
    lastPage.unmount()
  })

  it('words a dead source inline without hiding the rows that worked', () => {
    const wrapper = mountResults({
      sources: SOURCES,
      rows: [row()],
      failures: [{ provider: 'kg', kind: 'timeout', message: 'search timed out' }],
    })

    expect(wrapper.text()).toContain('cloud_search.failure_timeout:Kugou')
    expect(wrapper.findAll('[data-test="catalog-row"]')).toHaveLength(1)
    wrapper.unmount()
  })

  it('explains "no source installed" as a page state, not an empty list', () => {
    const wrapper = mountResults({ errorKind: 'no_sources' })

    expect(wrapper.text()).toContain('cloud_search.no_sources_title')
    expect(wrapper.text()).not.toContain('cloud_search.empty_title')
    wrapper.unmount()
  })

  it('shows the empty state when a usable page has no rows', () => {
    const wrapper = mountResults({ sources: SOURCES, rows: [] })

    expect(wrapper.text()).toContain('cloud_search.empty_title')
    wrapper.unmount()
  })

  it('renders nothing queryable before a keyword is typed', () => {
    const wrapper = mountResults({ hasQuery: false })

    expect(wrapper.find('[data-test="catalog-row"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('cloud_search.empty_title')
    wrapper.unmount()
  })
})