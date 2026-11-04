import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

// The settings page is an orchestrator: it switches on `activeCategory` and mounts
// one section. Stubbing the sections keeps the test about the switch itself.
const replace = vi.fn()
vi.mock('vue-router', () => ({
  useRoute: () => ({ name: 'settings', query: {} }),
  useRouter: () => ({ replace }),
}))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/components/settings/GeneralSettings.vue', () => ({ default: { template: '<div data-test="general" />' } }))
vi.mock('@/components/settings/LibrarySettings.vue', () => ({ default: { template: '<div data-test="library" />' } }))
vi.mock('@/components/settings/IntegrationsSettings.vue', () => ({ default: { template: '<div data-test="integrations" />' } }))
vi.mock('@/components/settings/PlaybackSettings.vue', () => ({ default: { template: '<div data-test="playback" />' } }))
vi.mock('@/components/settings/DownloadSettings.vue', () => ({ default: { template: '<div data-test="download" />' } }))
vi.mock('@/components/settings/AboutSettings.vue', () => ({ default: { template: '<div data-test="about" />' } }))
vi.mock('@/components/settings/RemoteServerSettings.vue', () => ({ default: { template: '<div data-test="remote" />' } }))
vi.mock('@/components/settings/MobileDevicesSettings.vue', () => ({ default: { template: '<div data-test="mobile-devices" />' } }))
// The two sections involved in the lx navigation keep their real behaviour for the
// event names but render nothing themselves.
vi.mock('@/components/settings/PluginsSettings.vue', () => ({
  default: {
    emits: ['openLxSources'],
    template: '<button data-test="open-lx" @click="$emit(\'openLxSources\')" />',
  },
}))
vi.mock('@/components/settings/LxSourcesSettings.vue', () => ({
  default: {
    props: ['back'],
    template: '<button data-test="lx-page" @click="back()" />',
  },
}))

import SettingsView from './SettingsView.vue'

function mountView(category = 'plugins') {
  return mount(SettingsView, {
    props: { category },
    global: { stubs: { 'router-view': true } },
  })
}

describe('SettingsView', () => {
  beforeEach(() => {
    replace.mockClear()
  })

  it('opens the lx source list from the plugins section', async () => {
    const wrapper = mountView('plugins')
    await flushPromises()
    expect(wrapper.find('[data-test="lx-page"]').exists()).toBe(false)

    await wrapper.find('[data-test="open-lx"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-test="lx-page"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="open-lx"]').exists()).toBe(false)
    // The category is persisted in the URL like every other settings section.
    expect(replace).toHaveBeenCalledWith('/settings/plugins-lx')
  })

  it('returns to the plugins list from the lx page', async () => {
    const wrapper = mountView('plugins-lx')
    await flushPromises()
    expect(wrapper.find('[data-test="lx-page"]').exists()).toBe(true)

    await wrapper.find('[data-test="lx-page"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-test="open-lx"]').exists()).toBe(true)
    expect(replace).toHaveBeenCalledWith('/settings/plugins')
  })

  it('does not render the lx page for an unrelated category', async () => {
    const wrapper = mountView('general')
    await flushPromises()
    expect(wrapper.find('[data-test="general"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="lx-page"]').exists()).toBe(false)
  })
})
