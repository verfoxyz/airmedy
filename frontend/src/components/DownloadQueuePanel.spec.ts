import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import DownloadQueuePanel from './DownloadQueuePanel.vue'
import { useDownloadStore } from '@/stores/download'
import * as DownloadService from '../../bindings/airmedy/internal/infra/wails/downloadservice'
import * as LibraryService from '../../bindings/airmedy/internal/infra/wails/libraryservice'

// Real store, real component: the queue is the contract under test, so mocking
// the store would only assert that the component calls a mock.
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

// Translate through the key so assertions read the key, not an English string.
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

const mountPanel = (open = true) =>
  mount(DownloadQueuePanel, {
    props: { open },
    global: {
      stubs: {
        // The scroller only recycles rows; rendering them plainly keeps the
        // component test about the panel, not about vue-virtual-scroller.
        DynamicScroller: {
          props: ['items'],
          template: '<div><slot v-for="(item, index) in items" :item="item" :index="index" :active="true" /></div>',
        },
        DynamicScrollerItem: { template: '<div><slot /></div>' },
      },
    },
  })

describe('DownloadQueuePanel', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    setActivePinia(createPinia())
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([])
    vi.mocked(DownloadService.CancelDownload).mockResolvedValue(undefined)
    vi.mocked(DownloadService.RetryDownload).mockResolvedValue(undefined)
    vi.mocked(LibraryService.GetTracksByIDs).mockResolvedValue([])
  })

  it('renders nothing while closed and hydrates when opened', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([job()] as never)

    const wrapper = mountPanel(false)
    expect(wrapper.find('[data-test="download-queue-panel"]').exists()).toBe(false)
    expect(DownloadService.ListDownloads).not.toHaveBeenCalled()

    await wrapper.setProps({ open: true })
    await flushPromises()

    expect(DownloadService.ListDownloads).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-test="download-queue-panel"]').exists()).toBe(true)
    expect(wrapper.findAll('[data-job-id]')).toHaveLength(1)
  })

  it('shows the empty state when the queue has nothing pending', async () => {
    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.find('[data-test="download-queue-empty"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('download.queue_empty')
  })

  it('lists queued, active, failed and canceled rows but never a done one', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ id: 'job-queued', state: 'queued' }),
      job({ id: 'job-active', track_id: 'track-2', state: 'active', bytes_done: 50, bytes_total: 100 }),
      job({ id: 'job-failed', track_id: 'track-3', state: 'failed', last_error: 'stream unavailable' }),
      job({ id: 'job-canceled', track_id: 'track-4', state: 'canceled' }),
      job({ id: 'job-done', track_id: 'track-5', state: 'done' }),
    ] as never)

    const wrapper = mountPanel()
    await flushPromises()

    const ids = wrapper.findAll('[data-job-id]').map(row => row.attributes('data-job-id'))
    expect(ids).toEqual(['job-queued', 'job-active', 'job-failed', 'job-canceled'])
    expect(wrapper.find('[data-test="download-queue-empty"]').exists()).toBe(false)
  })

  it('shows the title and artist resolved for a job', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([job()] as never)
    vi.mocked(LibraryService.GetTracksByIDs).mockResolvedValue([
      { id: 'track-1', title: 'A Song', raw_artist_names: 'An Artist' },
    ] as never)

    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.text()).toContain('A Song')
    expect(wrapper.text()).toContain('An Artist')
  })

  it('falls back to the track id when the track no longer exists', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([job({ track_id: 'deleted-track' })] as never)
    vi.mocked(LibraryService.GetTracksByIDs).mockResolvedValue([] as never)

    const wrapper = mountPanel()
    await flushPromises()

    // The row must still render (and stay cancelable) instead of crashing.
    const row = wrapper.find('[data-test="download-job-queued"]')
    expect(row.exists()).toBe(true)
    expect(row.text()).toContain('deleted-track')
  })

  it('shows the unknown-track fallback when the job carries no title or id', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([job({ track_id: '' })] as never)

    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.find('[data-test="download-job-queued"]').text()).toContain('download.unknown_track')
  })

  it('renders an indeterminate bar instead of 0% when the size is unknown', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ state: 'active', bytes_done: 4096, bytes_total: 0 }),
    ] as never)

    const wrapper = mountPanel()
    await flushPromises()

    const bar = wrapper.get('[role="progressbar"]')
    expect(bar.attributes('data-indeterminate')).toBe('true')
    // No aria-valuenow: the percentage is unknown, not zero.
    expect(bar.attributes('aria-valuenow')).toBeUndefined()
    expect(wrapper.find('[data-test="download-bar-indeterminate"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="download-bar-fill"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('0%')
  })

  it('renders a real percentage once the total size is known', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ state: 'active', bytes_done: 256, bytes_total: 1024 }),
    ] as never)

    const wrapper = mountPanel()
    await flushPromises()

    const bar = wrapper.get('[role="progressbar"]')
    expect(bar.attributes('aria-valuenow')).toBe('25')
    expect(bar.attributes('data-indeterminate')).toBeUndefined()
    expect(wrapper.get('[data-test="download-bar-fill"]').attributes('style')).toContain('width: 25%')
  })

  it('surfaces the backend error on a failed job', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ state: 'failed', last_error: 'stream unavailable', attempts: 3, max_attempts: 3 }),
    ] as never)

    const wrapper = mountPanel()
    await flushPromises()

    const status = wrapper.get('[data-test="download-status"]')
    expect(status.text()).toBe('stream unavailable')
    expect(wrapper.text()).toContain('download.attempt')
  })

  it('cancels a running job', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ state: 'active', bytes_done: 10, bytes_total: 100 }),
    ] as never)
    const store = useDownloadStore()
    const spy = vi.spyOn(store, 'cancel')

    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.get('[data-test="download-cancel"]').trigger('click')
    await flushPromises()

    expect(spy).toHaveBeenCalledWith('job-1')
    expect(DownloadService.CancelDownload).toHaveBeenCalledWith('job-1')
    // A canceled job stays listed, grayed, so it can be retried.
    expect(wrapper.find('[data-test="download-job-canceled"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="download-cancel"]').exists()).toBe(false)
  })

  it('retries a failed job', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ state: 'failed', last_error: 'stream unavailable' }),
    ] as never)
    const store = useDownloadStore()
    const spy = vi.spyOn(store, 'retry')

    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.get('[data-test="download-retry"]').trigger('click')
    await flushPromises()

    expect(spy).toHaveBeenCalledWith('job-1')
    expect(DownloadService.RetryDownload).toHaveBeenCalledWith('job-1')
  })

  it('offers retry on a canceled job', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([
      job({ state: 'canceled' }),
    ] as never)

    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.find('[data-test="download-retry"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="download-cancel"]').exists()).toBe(false)
  })

  it('refreshes the queue every time it opens', async () => {
    vi.mocked(DownloadService.ListDownloads).mockResolvedValue([job()] as never)
    const wrapper = mountPanel(false)

    await wrapper.setProps({ open: true })
    await flushPromises()
    expect(DownloadService.ListDownloads).toHaveBeenCalledTimes(1)

    await wrapper.setProps({ open: false })
    await wrapper.setProps({ open: true })
    await flushPromises()
    expect(DownloadService.ListDownloads).toHaveBeenCalledTimes(2)
  })

  it('closes on Escape and on the close button', async () => {
    const wrapper = mountPanel()
    await flushPromises()

    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await wrapper.vm.$nextTick()
    expect(wrapper.emitted('close')).toHaveLength(1)

    await wrapper.get('[data-test="download-queue-close"]').trigger('click')
    expect(wrapper.emitted('close')).toHaveLength(2)
  })

  it('ignores Escape once closed', async () => {
    const wrapper = mountPanel(false)
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await wrapper.vm.$nextTick()

    expect(wrapper.emitted('close')).toBeUndefined()
  })

  it('removes the keydown listener on unmount', async () => {
    const wrapper = mountPanel()
    await flushPromises()
    wrapper.unmount()

    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await flushPromises()
    expect(wrapper.emitted('close')).toBeUndefined()
  })
})
