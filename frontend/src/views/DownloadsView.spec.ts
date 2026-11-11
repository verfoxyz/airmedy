import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import DownloadsView from './DownloadsView.vue'
import { useDownloadStore } from '@/stores/download'
import * as DownloadService from '../../bindings/airmedy/internal/infra/wails/downloadservice'
import * as LibraryService from '../../bindings/airmedy/internal/infra/wails/libraryservice'

// Real store, real view: the queue is the contract under test, so mocking the
// store would only assert that the view calls a mock.
vi.mock('@wailsio/runtime', () => ({
  Events: { On: vi.fn(() => () => {}) },
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/downloadservice', () => ({
  ListDownloads: vi.fn(),
  CancelDownload: vi.fn(),
  RetryDownload: vi.fn(),
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/libraryservice', () => ({
  GetTracksByIDs: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, values?: Record<string, unknown>) =>
      values ? `${key}:${JSON.stringify(values)}` : key,
  }),
}))

const job = (overrides: Record<string, unknown> = {}) => ({
  id: 'job-1',
  track_id: 'track-1',
  playlist_id: 'pl-1',
  quality: 'lossless',
  state: 'queued',
  attempts: 0,
  max_attempts: 3,
  last_error: '',
  bytes_done: 0,
  bytes_total: 0,
  target_path: '',
  created_at: '2026-01-01T00:00:00.000Z',
  updated_at: '2026-01-01T00:00:00.000Z',
  ...overrides,
})

const mountView = () =>
  mount(DownloadsView, {
    global: {
      stubs: {
        ViewHeader: { props: ['title'], template: '<div data-test="view-header">{{ title }}</div>' },
        DynamicScroller: {
          props: ['items'],
          template: '<div data-test="scroller"><slot v-for="(item, index) in items" :item="item" :index="index" :active="true" /></div>',
        },
        DynamicScrollerItem: { template: '<div><slot /></div>' },
      },
    },
  })

describe('DownloadsView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    setActivePinia(createPinia())
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([])
    vi.mocked(DownloadService.CancelDownload).mockResolvedValue(undefined)
    vi.mocked(DownloadService.RetryDownload).mockResolvedValue(undefined)
    vi.mocked(LibraryService.GetTracksByIDs).mockResolvedValue([])
  })

  it('hydrates the queue on mount and lists the jobs', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([job()] as never)

    const wrapper = mountView()
    await flushPromises()

    expect(DownloadService.ListDownloads).toHaveBeenCalled()
    expect(wrapper.get('[data-test="view-header"]').text()).toBe('download.queue_title')
    expect(wrapper.find('[data-test="downloads-list"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('shows the empty state when nothing is queued', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-test="downloads-empty"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('download.queue_empty')
    wrapper.unmount()
  })

  it('cancels a job from its row action', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ id: 'job-9', state: 'active' }),
    ] as never)
    vi.mocked(DownloadService.CancelDownload).mockResolvedValue(undefined)

    const wrapper = mountView()
    await flushPromises()

    const cancel = wrapper.find('[data-test="download-cancel"]')
    expect(cancel.exists()).toBe(true)
    await cancel.trigger('click')
    await flushPromises()

    expect(DownloadService.CancelDownload).toHaveBeenCalledWith('job-9')
    wrapper.unmount()
  })
})
