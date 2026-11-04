import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

// The generated Wails bindings are build output; stub the module the component
// imports so the test exercises the page's own behaviour.
const api = vi.hoisted(() => ({
  ListLxSources: vi.fn(),
  ImportLxSource: vi.fn(),
  SetLxSourceEnabled: vi.fn(),
  RenameLxSource: vi.fn(),
  RemoveLxSource: vi.fn(),
  ReorderLxSources: vi.fn(),
}))

vi.mock('../../../bindings/airmedy/internal/infra/wails/lxsourcesservice', () => api)
vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => (params ? `${key}:${JSON.stringify(params)}` : key) }),
}))
// ConfirmDialog pulls the Modal primitive in; stub it so the test drives the
// page's confirm path without Radix internals.
vi.mock('../ConfirmDialog.vue', () => ({
  default: {
    props: ['open', 'title', 'message'],
    emits: ['confirm', 'cancel', 'update:open'],
    template: '<div v-if="open" data-test="lx-sources-confirm"><button data-test="lx-sources-confirm-yes" @click="$emit(\'confirm\')" /></div>',
  },
}))

import LxSourcesSettings from './LxSourcesSettings.vue'

function source(overrides: Record<string, unknown> = {}) {
  return {
    id: 'a1',
    name: 'ikun',
    file_name: 'a1.js',
    enabled: true,
    position: 0,
    providers: ['wy', 'kg'],
    error: '',
    ...overrides,
  }
}

function state(overrides: Record<string, unknown> = {}) {
  return {
    sources: [],
    directory: '/data/airmedy/plugins/lx/sources',
    plugin_running: true,
    plugin_error: '',
    ...overrides,
  }
}

function mountPage(back = vi.fn()) {
  return mount(LxSourcesSettings, {
    props: { back },
    global: {
      stubs: {
        // The real Switch needs Radix internals; a plain button keeps the test
        // about the page's wiring.
        Switch: {
          props: ['modelValue', 'disabled'],
          emits: ['update:modelValue'],
          template: '<button :data-test="$attrs[\'data-test\']" :disabled="disabled" @click="$emit(\'update:modelValue\', !modelValue)" />',
        },
      },
    },
  })
}

describe('LxSourcesSettings', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    api.ListLxSources.mockResolvedValue(state())
    api.ImportLxSource.mockResolvedValue(null)
    api.SetLxSourceEnabled.mockResolvedValue(undefined)
    api.RemoveLxSource.mockResolvedValue(undefined)
    api.ReorderLxSources.mockResolvedValue(undefined)
  })

  it('renders the empty state with the storage directory', async () => {
    const wrapper = mountPage()
    await flushPromises()

    expect(wrapper.find('[data-test="lx-sources-empty"]').exists()).toBe(true)
    // The path comes from the backend: it differs per platform, so it cannot live
    // in a locale file.
    expect(wrapper.text()).toContain('/data/airmedy/plugins/lx/sources')
    expect(wrapper.find('[data-test="lx-sources-list"]').exists()).toBe(false)
  })

  it('renders one row per source with its platforms', async () => {
    api.ListLxSources.mockResolvedValue(state({ sources: [source()] }))
    const wrapper = mountPage()
    await flushPromises()

    const row = wrapper.find('[data-test="lx-source-row-a1"]')
    expect(row.exists()).toBe(true)
    expect(row.find('[data-test="lx-source-name"]').text()).toBe('ikun')
    // The platforms are the plugin's answer, which is what tells the user which
    // source will serve a given track.
    expect(row.findAll('[data-test="lx-source-providers"] span').map(tag => tag.text())).toEqual(['wy', 'kg'])
  })

  it('shows why a source failed to load', async () => {
    api.ListLxSources.mockResolvedValue(state({
      sources: [source({ id: 'bad', name: 'broken', providers: [], error: 'source script threw during load' })],
    }))
    const wrapper = mountPage()
    await flushPromises()

    expect(wrapper.find('[data-test="lx-source-no-providers"]').text()).toContain('source script threw during load')
  })

  it('imports a source and reloads the list', async () => {
    api.ImportLxSource.mockResolvedValue(source({ id: 'new' }))
    api.ListLxSources
      .mockResolvedValueOnce(state())
      .mockResolvedValueOnce(state({ sources: [source({ id: 'new' })] }))

    const wrapper = mountPage()
    await flushPromises()
    expect(wrapper.find('[data-test="lx-sources-empty"]').exists()).toBe(true)

    await wrapper.find('[data-test="lx-sources-add"]').trigger('click')
    await flushPromises()

    expect(api.ImportLxSource).toHaveBeenCalled()
    expect(wrapper.find('[data-test="lx-source-row-new"]').exists()).toBe(true)
  })

  it('does not reload when the user cancelled the picker', async () => {
    api.ImportLxSource.mockResolvedValue(null)
    const wrapper = mountPage()
    await flushPromises()
    await wrapper.find('[data-test="lx-sources-add"]').trigger('click')
    await flushPromises()

    // A cancelled picker is not a failure: no error notice and no pointless reload.
    expect(wrapper.find('[data-test="lx-sources-action-error"]').exists()).toBe(false)
    expect(api.ListLxSources).toHaveBeenCalledTimes(1)
  })

  it('shows the backend error when an import is rejected', async () => {
    api.ImportLxSource.mockRejectedValue(new Error('this file cannot be used as an lx source: syntax error'))
    const wrapper = mountPage()
    await flushPromises()
    await wrapper.find('[data-test="lx-sources-add"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-test="lx-sources-action-error"]').text()).toContain('cannot be used as an lx source')
  })

  it('toggles a source and persists the change', async () => {
    api.ListLxSources.mockResolvedValue(state({ sources: [source()] }))
    const wrapper = mountPage()
    await flushPromises()

    await wrapper.find('[data-test="lx-source-toggle-a1"]').trigger('click')
    await flushPromises()

    expect(api.SetLxSourceEnabled).toHaveBeenCalledWith('a1', false)
  })

  it('asks for confirmation before removing a source', async () => {
    api.ListLxSources.mockResolvedValue(state({ sources: [source()] }))
    const wrapper = mountPage()
    await flushPromises()

    await wrapper.find('[data-test="lx-source-remove-a1"]').trigger('click')
    await flushPromises()

    // The delete is destructive, so it must not happen on the first click.
    expect(wrapper.find('[data-test="lx-sources-confirm"]').exists()).toBe(true)
    expect(api.RemoveLxSource).not.toHaveBeenCalled()

    await wrapper.find('[data-test="lx-sources-confirm-yes"]').trigger('click')
    await flushPromises()
    expect(api.RemoveLxSource).toHaveBeenCalledWith('a1')
  })

  it('reorders with the up/down handles and persists the whole order', async () => {
    const first = source({ id: 'a', name: 'A', position: 0 })
    const second = source({ id: 'b', name: 'B', position: 1 })
    api.ListLxSources.mockResolvedValue(state({ sources: [first, second] }))
    const wrapper = mountPage()
    await flushPromises()

    // The first row cannot move up.
    expect(wrapper.find('[data-test="lx-source-up-a"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="lx-source-up-a"]').attributes('disabled')).toBeDefined()

    await wrapper.find('[data-test="lx-source-down-a"]').trigger('click')
    await flushPromises()

    // The whole order is sent, so the stored positions cannot drift from the view.
    expect(api.ReorderLxSources).toHaveBeenCalledWith(['b', 'a'])
  })

  it('explains why platforms are unknown when the plugin is off', async () => {
    api.ListLxSources.mockResolvedValue(state({
      sources: [source({ providers: [] })],
      plugin_running: false,
      plugin_error: 'the lx plugin is not running (enable it in Settings → Plugins)',
    }))
    const wrapper = mountPage()
    await flushPromises()

    const notice = wrapper.find('[data-test="lx-sources-plugin-off"]')
    expect(notice.exists()).toBe(true)
    expect(notice.text()).toContain('not running')
    // The rows are still listed: the list is manageable with the plugin off.
    expect(wrapper.find('[data-test="lx-source-row-a1"]').exists()).toBe(true)
  })

  it('goes back to the plugins list', async () => {
    const back = vi.fn()
    const wrapper = mountPage(back)
    await flushPromises()
    await wrapper.find('[data-test="lx-sources-back"]').trigger('click')
    expect(back).toHaveBeenCalled()
  })

  it('shows the order hint only when there is something to order', async () => {
    const wrapper = mountPage()
    await flushPromises()
    expect(wrapper.find('[data-test="lx-sources-order-hint"]').exists()).toBe(false)

    api.ListLxSources.mockResolvedValue(state({ sources: [source()] }))
    const withSources = mountPage()
    await flushPromises()
    expect(withSources.find('[data-test="lx-sources-order-hint"]').exists()).toBe(true)
  })
})
