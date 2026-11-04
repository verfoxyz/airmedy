import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import CatalogResults from './CatalogResults.vue'
import { usePlaylistsStore } from '@/stores/playlists'
import type {
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
  activeSource: '',
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
  })

  it('renders 全部 plus one button per source and emits the chosen source', async () => {
    const wrapper = mountResults({ sources: SOURCES })

    const buttons = wrapper.findAll('[data-test="catalog-source"]')
    expect(buttons).toHaveLength(3)
    expect(buttons[0].text()).toBe('cloud_search.all_sources')
    expect(buttons[1].text()).toBe('NetEase')

    await buttons[2].trigger('click')
    expect(wrapper.emitted('update:source')?.[0]).toEqual(['kg'])
    wrapper.unmount()
  })

  it('renders a row as title plus "artist · source"', () => {
    const wrapper = mountResults({ sources: SOURCES, rows: [row()] })

    const listRow = wrapper.find('[data-test="catalog-row"]')
    expect(listRow.text()).toContain('晴天')
    // The subtitle names the source, because the same track can come from two.
    expect(listRow.text()).toContain('周杰伦 · NetEase')
    wrapper.unmount()
  })

  it('renders a cover thumbnail and falls back to the placeholder on load failure', async () => {
    const wrapper = mountResults({
      sources: SOURCES,
      rows: [row({ cover_url: 'https://p1.music.126.net/cover.jpg?param=100y100' })],
    })

    const img = wrapper.get('[data-test="catalog-cover"]')
    expect(img.attributes('src')).toContain('param=100y100')
    expect(img.attributes('loading')).toBe('lazy')

    await img.trigger('error')
    expect(wrapper.find('[data-test="catalog-cover"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="catalog-cover-placeholder"]').exists()).toBe(true)
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