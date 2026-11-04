import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

// The generated Wails bindings are build output; stub the module the component
// imports so the test exercises the page's own behaviour.
const api = vi.hoisted(() => ({
  ListPlugins: vi.fn(),
  GetPluginsDirectory: vi.fn().mockResolvedValue('/data/airmedy/plugins'),
  SetPluginEnabled: vi.fn(),
  GetPluginStatus: vi.fn(),
  RescanPlugins: vi.fn(),
}))

// The component imports the generated bindings by relative path, and the module
// does not exist in a checkout without a `wails3 generate bindings` run. Mock it
// by the resolved path so resolution succeeds before the factory is consulted.
vi.mock('../../../bindings/airmedy/internal/infra/wails/pluginservice', () => api)
vi.mock('vue-i18n', () => ({
  // The network hint is a template the row fills in itself, so the mock has to
  // keep the placeholder instead of returning the bare key.
  useI18n: () => ({
    t: (key: string) => (key === 'settings.plugins.network_hosts' ? 'hosts: {hosts}' : key),
  }),
}))

import PluginsSettings from './PluginsSettings.vue'

function plugin(overrides: Record<string, unknown> = {}) {
  return {
    id: 'lx.ikun',
    name: 'ikun 音乐源',
    version: '2.0.0',
    author: 'Airmedy',
    capabilities: ['stream.resolve'],
    providers: ['wy', 'kg'],
    network: [],
    enabled: false,
    running: false,
    status: 'idle',
    invalid: false,
    last_error: '',
    ...overrides,
  }
}

function mountSettings() {
  return mount(PluginsSettings, {
    global: {
      stubs: {
        // The real Switch needs Radix internals; a plain button keeps the test
        // about the page's wiring rather than the primitive.
        Switch: {
          props: ['modelValue', 'disabled'],
          emits: ['update:modelValue'],
          template: '<button :data-test="$attrs[\'data-test\']" :disabled="disabled" @click="$emit(\'update:modelValue\', !modelValue)" />',
        },
      },
    },
  })
}

describe('PluginsSettings', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    api.ListPlugins.mockResolvedValue([])
    api.RescanPlugins.mockResolvedValue([])
    api.SetPluginEnabled.mockResolvedValue(undefined)
  })

  it('renders the empty state with the plugin directory', async () => {
    const wrapper = mountSettings()
    await flushPromises()

    expect(wrapper.find('[data-test="plugins-empty"]').exists()).toBe(true)
    // The empty state must name the real location, and it comes from the
    // backend: the path differs per platform, so it cannot live in a locale file.
    expect(api.GetPluginsDirectory).toHaveBeenCalled()
    expect(wrapper.text()).toContain('/data/airmedy/plugins')
    expect(wrapper.find('[data-test="plugins-list"]').exists()).toBe(false)
  })

  // Acceptance case 7: the list is flat, one plugin per row, and every row has
  // its own switch. A nested row or a missing switch is the old host model
  // leaking back in.
  it('renders one flat row with one switch per plugin', async () => {
    api.ListPlugins.mockResolvedValue([
      plugin(),
      plugin({ id: 'lx.other', name: 'Another source', providers: ['tx'] }),
    ])

    const wrapper = mountSettings()
    await flushPromises()

    const rows = wrapper.findAll('[data-test^="plugin-row-"]')
    expect(rows).toHaveLength(2)
    // No nesting metadata at all: the attribute the old model used is gone.
    for (const row of rows) {
      expect(row.attributes('data-nested')).toBeUndefined()
    }

    expect(wrapper.find('[data-test="plugin-toggle-lx.ikun"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="plugin-toggle-lx.other"]').exists()).toBe(true)
  })

  it('shows name, version, author, capability and provider tags', async () => {
    api.ListPlugins.mockResolvedValue([
      plugin({ capabilities: ['stream.resolve', 'auth'], providers: ['wy', 'kg', 'kw'] }),
    ])

    const wrapper = mountSettings()
    await flushPromises()

    const row = wrapper.find('[data-test="plugin-row-lx.ikun"]')
    expect(row.find('[data-test="plugin-name"]').text()).toBe('ikun 音乐源')
    expect(row.text()).toContain('v2.0.0')
    expect(row.find('[data-test="plugin-author"]').text()).toBe('Airmedy')
    expect(row.find('[data-test="plugin-capabilities"]').text()).toContain('stream.resolve')
    expect(row.find('[data-test="plugin-capabilities"]').text()).toContain('auth')

    const providers = row.find('[data-test="plugin-providers"]')
    expect(providers.text()).toContain('wy')
    expect(providers.text()).toContain('kg')
    expect(providers.text()).toContain('kw')
  })

  it('omits the provider block for a plugin that serves no source', async () => {
    api.ListPlugins.mockResolvedValue([plugin({ id: 'lx.lyrics', providers: [] })])

    const wrapper = mountSettings()
    await flushPromises()

    // A lyrics-only plugin is valid; it just has no routing tags to show.
    expect(wrapper.find('[data-test="plugin-providers"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="plugin-toggle-lx.lyrics"]').exists()).toBe(true)
  })

  it('toggles a plugin and reloads the list', async () => {
    api.ListPlugins.mockResolvedValue([plugin()])

    const wrapper = mountSettings()
    await flushPromises()

    await wrapper.find('[data-test="plugin-toggle-lx.ikun"]').trigger('click')
    await flushPromises()

    expect(api.SetPluginEnabled).toHaveBeenCalledWith('lx.ikun', true)
    // The list is re-read so the running state comes from the backend rather
    // than being guessed from the click.
    expect(api.ListPlugins).toHaveBeenCalledTimes(2)
  })

  it('surfaces a failed toggle without losing the list', async () => {
    api.ListPlugins.mockResolvedValue([plugin()])
    api.SetPluginEnabled.mockRejectedValue(new Error('too many plugins are running'))

    const wrapper = mountSettings()
    await flushPromises()

    await wrapper.find('[data-test="plugin-toggle-lx.ikun"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-test="plugins-action-error"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="plugin-row-lx.ikun"]').exists()).toBe(true)
  })

  it('shows the failure status and reveals the error on demand', async () => {
    api.ListPlugins.mockResolvedValue([
      plugin({
        status: 'failed',
        last_error: 'plugin "lx.ikun" handshake: connection refused',
      }),
    ])

    const wrapper = mountSettings()
    await flushPromises()

    expect(wrapper.find('[data-test="plugin-status-lx.ikun"]').text())
      .toBe('settings.plugins.status_failed')

    // The error is collapsed by default: a long stack trace must not push the
    // rest of the list off screen.
    expect(wrapper.find('[data-test="plugin-error-lx.ikun"]').exists()).toBe(false)

    await wrapper.find('[data-test="plugin-error-toggle-lx.ikun"]').trigger('click')
    expect(wrapper.find('[data-test="plugin-error-lx.ikun"]').text())
      .toContain('connection refused')
  })

  it('marks a conflicted plugin unavailable and disables its switch', async () => {
    api.ListPlugins.mockResolvedValue([
      plugin({
        invalid: true,
        status: 'failed',
        last_error: 'plugin provider conflict: provider "wy" is also declared by lx.other',
      }),
    ])

    const wrapper = mountSettings()
    await flushPromises()

    expect(wrapper.find('[data-test="plugin-status-lx.ikun"]').text())
      .toBe('settings.plugins.status_invalid')
    const toggle = wrapper.find('[data-test="plugin-toggle-lx.ikun"]')
    expect(toggle.attributes('disabled')).toBeDefined()
  })

  it('never renders the removed host-disabled wording', async () => {
    api.ListPlugins.mockResolvedValue([plugin({ enabled: false, status: 'idle' })])

    const wrapper = mountSettings()
    await flushPromises()

    // The key is deleted from every locale; if a component still asked for it,
    // vue-i18n would render the raw path and this would catch it.
    expect(wrapper.text()).not.toContain('host_disabled')
    expect(wrapper.find('[data-test="plugin-status-lx.ikun"]').text())
      .toBe('settings.plugins.status_idle')
  })

  // The row declares what the plugin says it will reach. It is a statement from
  // the manifest, not an enforced sandbox, so the badge must be visible while
  // the plugin is still disabled — that is when the user decides.
  /**
   * The KV badge is the one permission the host *enforces*, so the row has to
   * show it even before the plugin is enabled — that is when the user is deciding
   * whether to grant it a store.
   */
  it('marks a plugin that may store data, even while disabled', async () => {
    api.ListPlugins.mockResolvedValue([
      plugin({ id: 'lx', enabled: false, kv: true }),
      plugin({ id: 'lx.quiet', kv: false }),
    ])
    const wrapper = mountSettings()
    await flushPromises()

    expect(wrapper.find('[data-test="plugin-storage-lx"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="plugin-storage-lx.quiet"]').exists()).toBe(false)
  })

  it('marks a plugin that declares network access, even while disabled', async () => {
    api.ListPlugins.mockResolvedValue([
      plugin({ id: 'lx', enabled: false, network: ['api.example.com', '*.qq.com'] }),
      plugin({ id: 'lx.quiet', network: [] }),
    ])

    const wrapper = mountSettings()
    await flushPromises()

    const badge = wrapper.find('[data-test="plugin-network-lx"]')
    expect(badge.exists()).toBe(true)
    expect(badge.attributes('data-network')).toBe('api.example.com,*.qq.com')
    expect(badge.attributes('title')).toContain('api.example.com')
    // A plugin that declares nothing gets no badge: an icon on every row would
    // carry no information.
    expect(wrapper.find('[data-test="plugin-network-lx.quiet"]').exists()).toBe(false)
  })

  // `*` is a different claim from a host list, so it gets its own wording instead
  // of being listed as if it were one more host.
  it('reads the wildcard as any address', async () => {
    api.ListPlugins.mockResolvedValue([plugin({ id: 'lx', network: ['*', 'api.example.com'] })])

    const wrapper = mountSettings()
    await flushPromises()

    const title = wrapper.find('[data-test="plugin-network-lx"]').attributes('title') ?? ''
    expect(title).toContain('settings.plugins.network_any')
    // Naming hosts next to `*` would understate what the manifest allows.
    expect(title).not.toContain('api.example.com')
  })

  it('names the declared hosts when the plugin lists them', async () => {
    api.ListPlugins.mockResolvedValue([plugin({ id: 'lx', network: ['api.example.com', 'cdn.qq.com'] })])

    const wrapper = mountSettings()
    await flushPromises()

    const title = wrapper.find('[data-test="plugin-network-lx"]').attributes('title') ?? ''
    expect(title).toContain('api.example.com, cdn.qq.com')
    expect(title).not.toContain('settings.plugins.network_any')
  })

  // The Lx plugin is the one plugin whose row opens a sub-page: the user manages
  // its source list there. Every other row stays a leaf.
  it('opens the lx source list from the lx row only', async () => {
    api.ListPlugins.mockResolvedValue([
      plugin({ id: 'lx', name: 'Lx 自定义源', providers: [] }),
      plugin({ id: 'lx.other', name: 'Another source', providers: ['tx'] }),
    ])

    const wrapper = mountSettings()
    await flushPromises()

    expect(wrapper.find('[data-test="plugin-manage-lx"]').exists()).toBe(true)
    // A non-lx plugin has no sub-list, so it must not offer the affordance.
    expect(wrapper.find('[data-test="plugin-manage-lx.other"]').exists()).toBe(false)

    await wrapper.find('[data-test="plugin-manage-lx"]').trigger('click')
    expect(wrapper.emitted('openLxSources')).toHaveLength(1)
  })

  it('rescans the plugin directory', async () => {
    api.ListPlugins.mockResolvedValue([])
    api.RescanPlugins.mockResolvedValue([plugin({ name: 'Newly copied' })])

    const wrapper = mountSettings()
    await flushPromises()

    await wrapper.find('[data-test="plugins-rescan"]').trigger('click')
    await flushPromises()

    expect(api.RescanPlugins).toHaveBeenCalled()
    expect(wrapper.find('[data-test="plugin-name"]').text()).toBe('Newly copied')
  })

  it('shows a load error instead of an empty list when listing fails', async () => {
    api.ListPlugins.mockRejectedValue(new Error('bridge unavailable'))

    const wrapper = mountSettings()
    await flushPromises()

    expect(wrapper.find('[data-test="plugins-load-error"]').text()).toContain('bridge unavailable')
    expect(wrapper.find('[data-test="plugins-empty"]').exists()).toBe(false)
  })
})