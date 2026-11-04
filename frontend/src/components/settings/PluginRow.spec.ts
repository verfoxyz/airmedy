import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

// The row is presentation only: labels arrive translated and the plugin object
// comes from the DTO. Stub the switch primitive so the test asserts the row's
// own markup rather than the primitive's internals.
vi.mock('@airmedy/ui', () => ({
  Switch: {
    name: 'Switch',
    props: ['modelValue', 'disabled'],
    template: '<button class="switch-stub" :data-disabled="disabled" @click="$emit(\'update:modelValue\', !modelValue)" />',
  },
}))

import PluginRow from './PluginRow.vue'
import type { PluginInfo, PluginRowLabels } from './pluginTypes'

const labels: PluginRowLabels = {
  running: 'Running',
  idle: 'Idle',
  failed: 'Failed',
  invalid: 'Unavailable',
  error: 'Show error',
  hideError: 'Hide error',
  networkLabel: 'Network access',
  networkHosts: 'May contact {hosts}',
  networkAny: 'any address',
  storageLabel: 'Stores data',
}

function plugin(overrides: Partial<PluginInfo> = {}): PluginInfo {
  return {
    id: 'lx',
    name: 'Lx 自定义源',
    version: '2.0.0',
    author: 'Airmedy',
    capabilities: ['stream.resolve'],
    providers: [],
    network: [],
    kv: false,
    enabled: false,
    running: false,
    status: 'idle',
    invalid: false,
    last_error: '',
    ...overrides,
  }
}

function mountRow(overrides: Partial<PluginInfo> = {}, busy = false) {
  return mount(PluginRow, {
    props: { plugin: plugin(overrides), labels, busy },
  })
}

describe('PluginRow', () => {
  it('renders the name, version and author', () => {
    const wrapper = mountRow({ author: 'Airmedy', version: '2.0.0' })

    expect(wrapper.find('[data-test="plugin-name"]').text()).toBe('Lx 自定义源')
    expect(wrapper.text()).toContain('v2.0.0')
    expect(wrapper.find('[data-test="plugin-author"]').text()).toBe('Airmedy')
  })

  it('maps the status to a translated label', () => {
    expect(mountRow().find('[data-test="plugin-status-lx"]').text()).toBe('Idle')
    expect(mountRow({ status: 'running', running: true }).find('[data-test="plugin-status-lx"]').text())
      .toBe('Running')
    expect(mountRow({ status: 'failed' }).find('[data-test="plugin-status-lx"]').text()).toBe('Failed')
  })

  // Invalid outranks the backend status: the manifest is unusable whatever the
  // process happens to be doing.
  it('reports an invalid plugin as unavailable even when it claims to run', () => {
    const wrapper = mountRow({ invalid: true, status: 'running' })

    expect(wrapper.find('[data-test="plugin-status-lx"]').text()).toBe('Unavailable')
  })

  it('disables the switch while busy', () => {
    expect(mountRow({}, true).find('[data-test="plugin-toggle-lx"]').attributes('data-disabled'))
      .toBe('true')
    expect(mountRow().find('[data-test="plugin-toggle-lx"]').attributes('data-disabled')).toBe('false')
  })

  // The declaration is a promise about reach, so it must be readable before the
  // user turns the plugin on; that is the decision it informs.
  it('shows the network badge on a disabled plugin', () => {
    const wrapper = mountRow({ enabled: false, network: ['api.example.com'] })

    const badge = wrapper.find('[data-test="plugin-network-lx"]')
    expect(badge.exists()).toBe(true)
    expect(badge.attributes('title')).toBe('Network access · May contact api.example.com')
  })

  it('omits the badge when the manifest declares no network', () => {
    expect(mountRow({ network: [] }).find('[data-test="plugin-network-lx"]').exists()).toBe(false)
  })

  // Inactive state is an opacity change, not a different colour, so the badge
  // keeps one meaning across themes while the row still reads as off.
  it('dims the badge on a disabled plugin', () => {
    expect(mountRow({ enabled: true, network: ['api.example.com'] })
      .find('[data-test="plugin-network-lx"]').classes()).toContain('opacity-100')
    expect(mountRow({ enabled: false, network: ['api.example.com'] })
      .find('[data-test="plugin-network-lx"]').classes()).toContain('opacity-40')
  })

  // `*` is a wider claim than any host list, so it reads as its own statement
  // instead of being listed alongside named hosts.
  it('reads the wildcard as any address', () => {
    const wrapper = mountRow({ network: ['*', 'api.example.com'] })

    expect(wrapper.find('[data-test="plugin-network-lx"]').attributes('title'))
      .toBe('Network access · any address')
  })

  it('lists every declared host', () => {
    const wrapper = mountRow({ network: ['api.example.com', 'cdn.qq.com'] })

    expect(wrapper.find('[data-test="plugin-network-lx"]').attributes('title'))
      .toBe('Network access · May contact api.example.com, cdn.qq.com')
  })

  // The error text is the only place the reason appears, so the toggle has to
  // reveal it without dropping the row.
  it('reveals the failure reason on demand', async () => {
    const wrapper = mountRow({ status: 'failed', last_error: 'handshake: connection refused' })

    expect(wrapper.find('[data-test="plugin-error-lx"]').exists()).toBe(false)

    await wrapper.find('[data-test="plugin-error-toggle-lx"]').trigger('click')

    expect(wrapper.find('[data-test="plugin-error-lx"]').text()).toBe('handshake: connection refused')
    expect(wrapper.find('[data-test="plugin-error-toggle-lx"]').text()).toBe('Hide error')
  })

  // Only the lx plugin owns a sub-page; every other row is a leaf and must not
  // offer an affordance that leads nowhere.
  it('opens a sub-page only when the row manages sources', () => {
    expect(mount(PluginRow, {
      props: { plugin: plugin(), labels, manageSources: true },
    }).find('[data-test="plugin-manage-lx"]').exists()).toBe(true)

    expect(mountRow().find('[data-test="plugin-manage-lx"]').exists()).toBe(false)
  })

  it('emits toggle with the requested value', async () => {
    const wrapper = mountRow({ enabled: false })

    await wrapper.find('[data-test="plugin-toggle-lx"]').trigger('click')

    expect(wrapper.emitted('toggle')).toEqual([[true]])
  })
})
