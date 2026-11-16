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
  CancelTask: vi.fn(),
  RetryTask: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, values?: Record<string, unknown>) =>
      values ? `${key}:${JSON.stringify(values)}` : key,
  }),
}))

const task = (overrides: Record<string, unknown> = {}) => ({
  id: 'task-1',
  kind: 'request',
  state: 'active',
  phase: '',
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
    vi.mocked(DownloadService.CancelTask).mockResolvedValue(undefined)
    vi.mocked(DownloadService.RetryTask).mockResolvedValue(undefined)
  })

  it('hydrates the merged task list on mount and opens on the active tab', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([task()] as never)

    const wrapper = mountView()
    await flushPromises()

    expect(DownloadService.ListTasks).toHaveBeenCalled()
    expect(wrapper.get('[data-test="view-header"]').text()).toBe('tasks.title')
    expect(wrapper.find('[data-test="tasks-tab-all"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="tasks-tab-active"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="tasks-list"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('tasks.kind_request')
    wrapper.unmount()
  })

  it('keys a request row by its stable request id, not the changing track id', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([
      task({
        id: 'row-id',
        request_id: 'request-stable',
        track_id: 'merged-away-track',
        title: 'All Eyes On Me',
        state: 'done',
      }),
    ] as never)

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-job-id="request-stable"]').exists()).toBe(true)
    expect(wrapper.find('[data-job-id="row-id"]').exists()).toBe(false)
    expect(wrapper.find('[data-job-id="merged-away-track"]').exists()).toBe(false)
    // The stale track id is not shown as the row's secondary label.
    expect(wrapper.find('[data-test="task-id"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('re-reads the list when the window becomes visible again', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([task()] as never)
    const wrapper = mountView()
    await flushPromises()
    vi.mocked(DownloadService.ListTasks).mockClear()

    window.dispatchEvent(new Event('focus'))
    await flushPromises()

    expect(DownloadService.ListTasks).toHaveBeenCalledTimes(1)
    wrapper.unmount()

    window.dispatchEvent(new Event('focus'))
    await flushPromises()
    expect(DownloadService.ListTasks).toHaveBeenCalledTimes(1)
  })

  it('shows the empty state for the selected tab', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-test="tasks-empty"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('tasks.empty_all')

    await wrapper.get('[data-test="tasks-tab-done"]').trigger('click')
    expect(wrapper.text()).toContain('tasks.empty_done')
    wrapper.unmount()
  })

  it('opens on the all tab and groups rows by the other tabs', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([
      task({ id: 'a', state: 'active' }),
      task({ id: 'd', state: 'done' }),
      task({ id: 'f', state: 'failed', error: '403' }),
    ] as never)

    const wrapper = mountView()
    await flushPromises()

    // "All" is the default and holds every run exactly once.
    expect(wrapper.get('[data-test="tasks-tab-all"]').text()).toContain('3')
    expect(wrapper.find('[data-job-id="a"]').exists()).toBe(true)
    expect(wrapper.find('[data-job-id="d"]').exists()).toBe(true)
    expect(wrapper.find('[data-job-id="f"]').exists()).toBe(true)

    expect(wrapper.get('[data-test="tasks-tab-active"]').text()).toContain('1')
    expect(wrapper.get('[data-test="tasks-tab-done"]').text()).toContain('1')
    expect(wrapper.get('[data-test="tasks-tab-failed"]').text()).toContain('1')

    await wrapper.get('[data-test="tasks-tab-done"]').trigger('click')
    expect(wrapper.find('[data-job-id="d"]').exists()).toBe(true)
    expect(wrapper.find('[data-job-id="a"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('labels a failed request and offers a retry', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([
      task({ id: 'r', state: 'failed', error: 'disk full' }),
    ] as never)

    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tasks-tab-failed"]').trigger('click')

    expect(wrapper.get('[data-test="task-kind"]').text()).toBe('tasks.kind_request')
    expect(wrapper.text()).toContain('disk full')
    expect(wrapper.find('[data-test="download-retry"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('shows the request phase as its status text', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([
      task({ id: 'r', state: 'active', phase: 'promoting' }),
    ] as never)

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="download-status"]').text()).toBe('tasks.phase_promoting')
    wrapper.unmount()
  })

  it('lists stream-cache sessions as their own kind in every state', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([
      task({ id: 'c-active', kind: 'cache', state: 'active' }),
      task({ id: 'c-done', kind: 'cache', state: 'done' }),
      task({ id: 'c-failed', kind: 'cache', state: 'failed', error: 'no source' }),
    ] as never)

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="tasks-tab-all"]').text()).toContain('3')
    expect(wrapper.get('[data-test="tasks-tab-active"]').text()).toContain('1')
    expect(wrapper.get('[data-test="tasks-tab-done"]').text()).toContain('1')
    expect(wrapper.get('[data-test="tasks-tab-failed"]').text()).toContain('1')
    const kinds = wrapper.findAll('[data-test="task-kind"]')
    expect(kinds).toHaveLength(3)
    for (const kind of kinds) expect(kind.text()).toBe('tasks.kind_cache')

    // A cache session belongs to playback: no cancel or retry button.
    expect(wrapper.find('[data-test="download-cancel"]').exists()).toBe(false)
    await wrapper.get('[data-test="tasks-tab-failed"]').trigger('click')
    expect(wrapper.text()).toContain('no source')
    expect(wrapper.find('[data-test="download-retry"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows the title as the main label without a bare track id', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([
      task({ id: 't1', track_id: 'ebdbf6a9-a70c-4caf-94af-67a13bd0a7b7', title: 'QuomodocunquizE' }),
    ] as never)

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="task-title"]').text()).toBe('QuomodocunquizE')
    expect(wrapper.find('[data-test="task-id"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('labels a legacy request by its identity, never by the old track id', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([
      task({ id: 't1', track_id: 'legacy-uuid', title: '42' }),
    ] as never)

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="task-title"]').text()).toBe('42')
    expect(wrapper.find('[data-test="task-id"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('cancels an active request from its row action', async () => {
    vi.mocked(DownloadService.ListTasks).mockResolvedValue([task({ id: 'request-9', state: 'active' })] as never)
    vi.mocked(DownloadService.CancelTask).mockResolvedValue(undefined)

    const wrapper = mountView()
    await flushPromises()

    const cancel = wrapper.find('[data-test="download-cancel"]')
    expect(cancel.exists()).toBe(true)
    await cancel.trigger('click')
    await flushPromises()

    expect(DownloadService.CancelTask).toHaveBeenCalledWith('request-9')
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
