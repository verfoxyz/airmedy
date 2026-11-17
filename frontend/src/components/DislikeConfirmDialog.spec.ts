import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  dislike: vi.fn(),
  preview: vi.fn(),
  undislike: vi.fn(),
  on: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/lastfmservice', () => ({
  ListDislikes: mocks.list,
  Dislike: mocks.dislike,
  PreviewDislike: mocks.preview,
  Undislike: mocks.undislike,
}))

vi.mock('@wailsio/runtime', () => ({
  Events: { On: mocks.on },
  Create: {
    Nullable: (fn: any) => (v: any) => (v == null ? null : fn(v)),
    Array: (fn: any) => (arr: any[]) => (arr ?? []).map(fn),
    Struct: (ctor: any) => (v: any) => (v == null ? null : new ctor(v)),
    Map: () => (val: any) => val,
    Any: (val: any) => val,
  },
}))

import DislikeConfirmDialog from './DislikeConfirmDialog.vue'
import { useDislikeStore } from '@/stores/dislike'
import { useAppStore } from '@/stores/app'

const entry = {
  id: 'd1', kind: 'track', match_key: 'drop\x00song',
  title: 'Song', artist: 'Drop', created_at: '2026-11-17T00:00:00Z',
}

function mountDialog() {
  const pinia = createPinia()
  setActivePinia(pinia)
  return mount(DislikeConfirmDialog, {
    global: {
      plugins: [pinia],
      mocks: { $t: (key: string) => key },
      stubs: {
        Modal: {
          props: ['open', 'title'],
          template: '<div v-if="open" data-test="modal"><slot /><slot name="footer" /></div>',
        },
      },
    },
  })
}

describe('DislikeConfirmDialog', () => {
  beforeEach(() => {
    mocks.list.mockReset()
    mocks.dislike.mockReset()
    mocks.preview.mockReset()
    mocks.undislike.mockReset()
    mocks.on.mockReset()
    mocks.on.mockReturnValue(() => {})
    mocks.list.mockResolvedValue([])
    mocks.dislike.mockResolvedValue({ entry, deleted_tracks: 1, purge_error: '' })
  })

  it('is closed without a pending dislike', async () => {
    const wrapper = mountDialog()
    await flushPromises()
    expect(wrapper.find('[data-test="modal"]').exists()).toBe(false)
  })

  it('offers a plain mark confirmation while the purge setting is off', async () => {
    const wrapper = mountDialog()
    const store = useDislikeStore()
    const app = useAppStore()
    app.dislikePurgeFiles = false
    store.pending = { kind: 'track', matchKey: 'drop\x00song', title: 'Song', artist: 'Drop' }
    store.pendingCount = 2
    await flushPromises()

    expect(wrapper.find('[data-test="dislike-mark-body"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="dislike-purge-warning"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="dislike-purge-count"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="dislike-confirm"]').text()).toBe('common.confirm')

    await wrapper.find('[data-test="dislike-confirm"]').trigger('click')
    await flushPromises()

    expect(mocks.dislike).toHaveBeenCalledWith('track', 'drop\x00song', 'Song', 'Drop', false)
    expect(store.pending).toBeNull()
  })

  it('offers delete-and-mark only while the purge setting is on', async () => {
    const wrapper = mountDialog()
    const store = useDislikeStore()
    useAppStore().dislikePurgeFiles = true
    store.pending = { kind: 'track', matchKey: 'drop\x00song', title: 'Song', artist: 'Drop' }
    store.pendingCount = 3
    await flushPromises()

    expect(wrapper.find('[data-test="dislike-purge-warning"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="dislike-purge-count"]').text()).toBe('settings.dislike_purge_count')
    expect(wrapper.find('[data-test="dislike-confirm"]').text()).toBe('settings.dislike_purge_confirm')

    await wrapper.find('[data-test="dislike-confirm"]').trigger('click')
    await flushPromises()

    expect(mocks.dislike).toHaveBeenCalledWith('track', 'drop\x00song', 'Song', 'Drop', true)
  })

  it('requires the acknowledgement before an artist purge', async () => {
    const wrapper = mountDialog()
    const store = useDislikeStore()
    useAppStore().dislikePurgeFiles = true
    store.pending = { kind: 'artist', matchKey: '', title: '', artist: 'Drop' }
    store.pendingCount = 4
    await flushPromises()

    const confirm = wrapper.find('[data-test="dislike-confirm"]')
    expect(confirm.attributes('disabled')).toBeDefined()

    await wrapper.find('[data-test="dislike-purge-acknowledge"]').setValue(true)
    expect(wrapper.find('[data-test="dislike-confirm"]').attributes('disabled')).toBeUndefined()

    await wrapper.find('[data-test="dislike-confirm"]').trigger('click')
    await flushPromises()

    expect(mocks.dislike).toHaveBeenCalledWith('artist', '', '', 'Drop', true)
  })
})
