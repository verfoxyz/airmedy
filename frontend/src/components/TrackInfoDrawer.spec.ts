import { beforeEach, describe, it, expect, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createTestingPinia } from '@pinia/testing'
import TrackInfoDrawer from './TrackInfoDrawer.vue'
import { usePlayerStore } from '@/stores/player'
import { TrackDTO } from '../../bindings/airmedy/internal/domain/models'

const catalogApi = vi.hoisted(() => ({
  ResolveCatalogTrackCover: vi.fn().mockResolvedValue(''),
}))
vi.mock('../../bindings/airmedy/internal/infra/wails/catalogservice', () => catalogApi)

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key,
  }),
}))

vi.mock('@/components/LazyImg.vue', () => ({
  default: { props: ['src'], template: '<img v-if="src" :src="src" />' },
}))

vi.mock('@wailsio/runtime', () => ({
  Events: { On: vi.fn(), Off: vi.fn() },
  Create: {
    Nullable: (fn: any) => (v: any) => (v == null ? null : fn(v)),
    Array: (fn: any) => (arr: any[]) => (arr ?? []).map(fn),
    Struct: (ctor: any) => (v: any) => (v == null ? null : new ctor(v)),
    Map: (k: any, v: any) => (val: any) => val,
  },
  Call: { ByID: vi.fn().mockResolvedValue(null) },
}))

function makeTrack(overrides: Record<string, unknown> = {}): TrackDTO {
  return new TrackDTO({
    id: 'track-1',
    title: 'My Song',
    format: 'flac',
    bitrate: 1000,
    sample_rate: 44100,
    bit_depth: 16,
    codec: '',
    ...overrides,
  } as any)
}

function mountDrawer(track: TrackDTO) {
  return mount(TrackInfoDrawer, {
    global: {
      plugins: [
        createTestingPinia({
          createSpy: vi.fn,
          initialState: {
            player: { trackInfoTrack: track },
          },
        }),
      ],
    },
  })
}

describe('TrackInfoDrawer quality badge', () => {
  it('shows LOSSY badge for mp3', () => {
    const w = mountDrawer(makeTrack({ format: 'mp3', bit_depth: 0, sample_rate: 44100, bitrate: 320 }))
    expect(w.text()).toContain('track_info.lossy')
  })

  it('shows LOSSLESS badge for 16-bit/44.1kHz flac', () => {
    const w = mountDrawer(makeTrack({ format: 'flac', bit_depth: 16, sample_rate: 44100 }))
    expect(w.text()).toContain('track_info.lossless')
    expect(w.get('.bg-\\[\\#E11D48\\]\\/10').classes()).toContain('text-[#E11D48]')
  })

  it('shows HI-RES badge for 24-bit/96kHz flac', () => {
    const w = mountDrawer(makeTrack({ format: 'flac', bit_depth: 24, sample_rate: 96000 }))
    expect(w.text()).toContain('track_info.hi_res')
  })

  it('shows DSD badge for dsf', () => {
    const w = mountDrawer(makeTrack({ format: 'dsf', bit_depth: 1, sample_rate: 2822400 }))
    expect(w.text()).toContain('track_info.dsd')
  })

  it('distinguishes m4a AAC (lossy) from ALAC (lossless) via codec', () => {
    const aac = mountDrawer(makeTrack({ format: 'm4a', codec: 'aac', bit_depth: 16, sample_rate: 44100 }))
    expect(aac.text()).toContain('track_info.lossy')

    const alac = mountDrawer(makeTrack({ format: 'm4a', codec: 'alac', bit_depth: 16, sample_rate: 44100 }))
    expect(alac.text()).toContain('track_info.lossless')
  })

  it('hides the badge for legacy m4a rows with no codec yet (no blind guessing)', () => {
    const w = mountDrawer(makeTrack({ format: 'm4a', codec: '', bit_depth: 0, bitrate: 1000, sample_rate: 44100 }))
    expect(w.text()).not.toContain('track_info.lossless')
    expect(w.text()).not.toContain('track_info.lossy')
    expect(w.text()).not.toContain('track_info.hi_res')
    expect(w.text()).not.toContain('track_info.dsd')
  })

  it('shows bit depth in the details list when present', () => {
    const w = mountDrawer(makeTrack({ format: 'flac', bit_depth: 24, sample_rate: 96000 }))
    expect(w.text()).toContain('24-bit')
  })

  it('shows copyright in the details list when present', () => {
    const w = mountDrawer(makeTrack({ copyright: '© 2026 Example Records' }))
    expect(w.text()).toContain('track_info.copyright')
    expect(w.text()).toContain('© 2026 Example Records')
  })
})

describe('TrackInfoDrawer artwork', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    catalogApi.ResolveCatalogTrackCover.mockResolvedValue('')
  })

  function mountDrawerWithStore() {
    return mount(TrackInfoDrawer)
  }

  it('keeps showing the opened track and its cover after playback moves on', async () => {
    catalogApi.ResolveCatalogTrackCover.mockResolvedValue('/cover-cache/opened.jpg')
    const store = usePlayerStore()
    const opened = makeTrack({ id: 'remote-opened', artwork_key: null, availability: 'remote' })
    store.openTrackInfo(opened)
    await flushPromises()

    const w = mountDrawerWithStore()
    expect(w.get('img').attributes('src')).toBe('/cover-cache/opened.jpg')
    expect(w.text()).toContain('My Song')

    store.currentTrack = makeTrack({ id: 'another-track', title: 'Another Song' })
    await flushPromises()
    expect(w.get('img').attributes('src')).toBe('/cover-cache/opened.jpg')
    expect(w.text()).toContain('My Song')
  })

  it('resolves a cover by the newly opened row identity', async () => {
    const store = usePlayerStore()
    const first = makeTrack({ id: 'remote-1', artwork_key: null, availability: 'remote' })
    store.openTrackInfo(first)
    await flushPromises()

    catalogApi.ResolveCatalogTrackCover.mockResolvedValue('/cover-cache/second.jpg')
    store.openTrackInfo(makeTrack({ id: 'remote-2', artwork_key: null, availability: 'remote' }))
    await flushPromises()

    expect(catalogApi.ResolveCatalogTrackCover).toHaveBeenCalledWith('remote-2')
    const w = mountDrawerWithStore()
    expect(w.get('img').attributes('src')).toBe('/cover-cache/second.jpg')
  })

  it('does not ask for a cover the row already has', async () => {
    const store = usePlayerStore()
    store.openTrackInfo(makeTrack({ id: 'local-1', artwork_key: 'local-key' }))
    await flushPromises()

    expect(catalogApi.ResolveCatalogTrackCover).not.toHaveBeenCalled()
    const w = mountDrawerWithStore()
    expect(w.get('img').attributes('src')).toContain('local-key')
  })
})
