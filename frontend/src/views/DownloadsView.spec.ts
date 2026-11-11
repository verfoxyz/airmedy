import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import DownloadsView from './DownloadsView.vue'
import * as DownloadService from '../../bindings/airmedy/internal/infra/wails/downloadservice'

// Real stores, real view: the merged task list is the contract under test, so
// mocking the store would only assert that the view calls a mock.
vi.mock('@wailsio/runtime', () => ({
  Events: { On: vi.fn(() => () => {}) },
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/downloadservice', () => ({
  ListTasks: vi.fn(),
  ClearTaskHistory: vi.fn(),
  CancelDownload: vi.fn(),
  RetryDownload: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, values?: Record<string, unknown>) =>
      values ? `${key}:${JSON.stringify(values)}` : key,
  }),
}))

const task = (overrides: Record<string, unknown> = {}) => ({
  id: 'task-1',
  kind: 'download',
  state: 'active',
  track_id: 'track-1',
  title: '晴天',
  artist: '周杰伦',
  quality: '320k',
  error: '',
  created_at: '2026-01-01T00:00:00.000Z',
  ...overrides,
})

const mountView = () =>
  mount(DownloadsView, {
    global: {
      stubs: {
        ViewHeader: {
          props: ['title'],
          template: '<div data-test="view-header">{{ title }}<slot name="actions" /></div>',
        },
        DynamicScroller: {
          props: ['items'],
          template: '<div data-test="scroller"><slot v-for="(item, index) in items" :item="item" :index="index" :active="true" /></div>',
        },
        DynamicScrollerItem: { template: '<div><slot /></div>' },
        ConfirmDialog: {
          props: ['open'],
          emits: ['confirm', 'cancel', 'update:open'],
          template: '<div v-if="open" data-test="confirm-dialog"><button data-test="confirm" @click="$emit(\'confirm\')">ok</button></div>',
        },
      },
    },
  })

describe('DownloadsView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    setActivePinia(createPinia())
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([])
    vi.mocked(DownloadService.ClearTaskHistory).mockResolvedValue(undefined)
    vi.mocked(DownloadService.CancelDownload).mockResolvedValue(undefined)
    vi.mocked(DownloadService.RetryDownload).mockResolvedValue(undefined)
  })

  it('hydrates the merged task list on mount and opens on the active tab', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([task()] as never)

    const wrapper = mountView()
    await flushPromises()

    expect(DownloadService.ListTasks).toHaveBeenCalled()
    expect(wrapper.get('[data-test="view-header"]').text()).toBe('tasks.title')
    expect(wrapper.find('[data-test="tasks-tab-active"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="tasks-list"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('tasks.kind_download')
    wrapper.unmount()
  })

  it('shows the empty state for the selected tab', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-test="tasks-empty"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('tasks.empty_active')

    await wrapper.get('[data-test="tasks-tab-done"]').trigger('click')
    expect(wrapper.text()).toContain('tasks.empty_done')
    wrapper.unmount()
  })

  it('groups rows by tab and shows a count on each tab', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([
      task({ id: 'a', state: 'active' }),
      task({ id: 'd', state: 'done' }),
      task({ id: 'f', state: 'failed', error: '403' }),
    ] as never)

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="tasks-tab-active"]').text()).toContain('1')
    expect(wrapper.get('[data-test="tasks-tab-done"]').text()).toContain('1')
    expect(wrapper.get('[data-test="tasks-tab-failed"]').text()).toContain('1')

    await wrapper.get('[data-test="tasks-tab-done"]').trigger('click')
    expect(wrapper.find('[data-job-id="d"]').exists()).toBe(true)
    expect(wrapper.find('[data-job-id="a"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('labels a promotion row and shows its error without offering retry', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([
      task({ id: 'p', kind: 'promote', state: 'failed', error: 'disk full' }),
    ] as never)

    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tasks-tab-failed"]').trigger('click')

    expect(wrapper.get('[data-test="task-kind"]').text()).toBe('tasks.kind_promote')
    expect(wrapper.text()).toContain('disk full')
    expect(wrapper.find('[data-test="download-retry"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('cancels an active download from its row action', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([task({ id: 'job-9', state: 'active' })] as never)
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

  it('asks for confirmation before clearing the finished history', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([
      task({ id: 'done-1', state: 'done' }),
    ] as never)

    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="tasks-clear"]').trigger('click')
    expect(wrapper.find('[data-test="confirm-dialog"]').exists()).toBe(true)
    expect(DownloadService.ClearTaskHistory).not.toHaveBeenCalled()

    await wrapper.get('[data-test="confirm"]').trigger('click')
    await flushPromises()

    expect(DownloadService.ClearTaskHistory).toHaveBeenCalledTimes(1)
    expect(DownloadService.ListTasks).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })
})
