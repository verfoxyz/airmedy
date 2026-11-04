import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createTestingPinia } from '@pinia/testing'
import LibrarySettings from './LibrarySettings.vue'

const { handlers, retry, listFailed, repairDurations, watchedFolders } = vi.hoisted(() => ({
  handlers: new Map<string, (event: { data: unknown }) => void>(),
  retry: vi.fn().mockResolvedValue(undefined),
  listFailed: vi.fn().mockResolvedValue([{ id: 'bad', title: 'Broken', artists: 'Artist', path: '/music/broken.mp3', artworkKey: '', failedComponents: ['ffmpeg'] }]),
  repairDurations: vi.fn(),
  watchedFolders: vi.fn().mockResolvedValue([]),
}))

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, values?: Record<string, number>) => values ? `${key}:${values.done}/${values.total} (${values.percent}%)` : key }) }))
vi.mock('@wailsio/runtime', () => ({
  Events: { On: vi.fn((name, handler) => { handlers.set(name, handler); return () => {} }) },
  Create: {
    Nullable: (create: (value: unknown) => unknown) => (value: unknown) => value == null ? null : create(value),
    Array: (create: (value: unknown) => unknown) => (values: unknown[]) => (values ?? []).map(create),
    Struct: (ctor: new (value: unknown) => unknown) => (value: unknown) => value == null ? null : new ctor(value),
    Map: () => (value: unknown) => value,
    Any: (value: unknown) => value,
  },
}))
vi.mock('../../../bindings/airmedy/internal/infra/wails/analysisservice', () => ({
  GetProgress: vi.fn().mockResolvedValue({ done: 0, total: 0, state: 'done', libraryDone: 8, libraryTotal: 10, failed: 2 }),
  ListFailedTracks: listFailed,
  RetryFailedTracks: retry,
}))
vi.mock('../../../bindings/airmedy/internal/infra/wails/libraryservice', () => ({
  GetWatchedFolders: watchedFolders,
  DelimitersPendingResync: vi.fn().mockResolvedValue(false),
  RepairTrackDurations: (...args: unknown[]) => repairDurations(...args),
}))

function mountSettings() {
  return mount(LibrarySettings, {
    global: {
      plugins: [createTestingPinia({ createSpy: vi.fn, initialState: { app: { libraryAnalysisEnabled: true, libraryAnalysisWorkerCount: 1, libraryAnalysisMaxWorkerCount: 1 } } })],
      stubs: {
        Modal: { template: '<div v-if="$attrs.open"><slot /></div>' },
        VirtualList: { props: ['modelValue'], template: '<div><slot v-for="record in modelValue" name="item" :record="record" /></div>' },
      },
    },
  })
}

describe('LibrarySettings analysis failures', () => {
  it('shows true completed progress and failure actions only when sync is idle', async () => {
    const wrapper = mountSettings()
    await flushPromises()
    expect(wrapper.text()).toContain('settings.library_analysis.analyzed:8/10 (80%)')
    expect(wrapper.text()).toContain('settings.library_analysis.retry_failed')
    handlers.get('library:sync-started')?.({ data: {} })
    await flushPromises()
    expect(wrapper.text()).not.toContain('settings.library_analysis.retry_failed')
  })

  it('retries failed tracks and renders failed rows in the virtual dialog', async () => {
    const wrapper = mountSettings()
    await flushPromises()
    await wrapper.get('[data-testid="retry-failed-tracks"]').trigger('click')
    expect(retry).toHaveBeenCalled()
  })

  it('repairs durations and lists the files that are still unreadable', async () => {
    watchedFolders.mockResolvedValue([{ id: 'folder-1', path: '/music' }])
    repairDurations.mockResolvedValue({
      repaired: 2,
      unreadable: [{ track_id: 'bad', title: 'Broken', path: '/music/broken.flac', reason: 'unreadable' }],
    })
    const wrapper = mountSettings()
    await flushPromises()

    await wrapper.findAll('button')
      .find(button => button.text() === 'settings.sync.repair_durations')!
      .trigger('click')
    await flushPromises()

    expect(repairDurations).toHaveBeenCalled()
    expect(wrapper.text()).toContain('settings.sync.duration_repair_repaired')
    expect(wrapper.text()).toContain('settings.sync.duration_repair_failed')
    expect(wrapper.text()).toContain('Broken')
  })
})
