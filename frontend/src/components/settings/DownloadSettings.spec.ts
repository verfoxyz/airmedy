import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import DownloadSettings from './DownloadSettings.vue'
import { useAppStore } from '@/stores/app'

const api = vi.hoisted(() => ({ SelectFolder: vi.fn().mockResolvedValue('') }))
vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))
// Spread the real module: the generated bindings import its type helpers
// (Create.Nullable & friends) while the component only needs Events stubbed.
vi.mock('@wailsio/runtime', async (importOriginal) => ({
  ...await importOriginal<typeof import('@wailsio/runtime')>(),
  Events: { On: vi.fn(() => () => {}) },
}))
vi.mock('../../../bindings/airmedy/internal/infra/wails/libraryservice', () => api)

// The store persists through SettingsService; stub it so the component test only
// checks the UI wiring.
const saveSettings = vi.hoisted(() => vi.fn().mockResolvedValue(undefined))
vi.mock('../../../bindings/airmedy/internal/infra/wails/settingsservice', () => ({
  GetSettings: vi.fn().mockResolvedValue(null),
  SaveSettings: saveSettings,
}))

function mountSettings() {
  return mount(DownloadSettings, {
    global: {
      plugins: [createPinia()],
      stubs: {
        // The real Switch needs Radix internals; a plain button keeps the test
        // about the page's wiring rather than the primitive.
        Switch: {
          props: ['modelValue', 'disabled'],
          emits: ['update:modelValue'],
          template: `<button v-bind="$attrs" :disabled="disabled" @click="$emit('update:modelValue', !modelValue)" />`,
        },
      },
    },
  })
}

/** toggle clicks one switch, which is how the stubbed primitive reports a change. */
async function toggle(wrapper: ReturnType<typeof mountSettings>, test: string) {
  await wrapper.find(`[data-test="${test}"]`).trigger('click')
  await flushPromises()
}

describe('DownloadSettings', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    api.SelectFolder.mockResolvedValue('')
  })

  it('renders the audio preferences and the content switches', () => {
    const wrapper = mountSettings()
    expect(wrapper.find('[data-test="download-dir"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="download-quality"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="download-naming-order"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="download-auto"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="stream-cache-mb"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="stream-quality"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="download-with-lyrics"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="download-with-translation"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="download-with-cover"]').exists()).toBe(true)
  })

  it('clamps the stream cache size to the documented range', async () => {
    const wrapper = mountSettings()
    const store = useAppStore()
    const input = wrapper.find('[data-test="stream-cache-mb"]')

    await input.setValue('999999')
    await input.trigger('change')
    await flushPromises()
    expect(store.streamCacheMB).toBe(20480)

    await input.setValue('1')
    await input.trigger('change')
    await flushPromises()
    expect(store.streamCacheMB).toBe(256)
  })

  it('warns when streaming and download qualities differ', async () => {
    const wrapper = mountSettings()
    const store = useAppStore()

    store.streamQuality = '320k'
    store.downloadQuality = 'flac'
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-test="stream-quality-warning"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('settings.download.stream_quality_mismatch')

    store.streamQuality = 'flac'
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-test="stream-quality-warning"]').exists()).toBe(false)
  })

  // A translation of lyrics that are never fetched cannot exist. The switch
  // stays visible so the option is discoverable, but it is disabled and the row
  // explains why — the backend collapses the pair the same way.
  it('disables the translation switch until lyrics are on', async () => {
    const wrapper = mountSettings()
    const translation = () => wrapper.find('[data-test="download-with-translation"]')

    expect(translation().attributes('disabled')).toBeDefined()

    await toggle(wrapper, 'download-with-lyrics')
    expect(translation().attributes('disabled')).toBeUndefined()
  })

  it('clears the translation choice when lyrics are turned off', async () => {
    const wrapper = mountSettings()
    await toggle(wrapper, 'download-with-lyrics')
    await toggle(wrapper, 'download-with-translation')
    await toggle(wrapper, 'download-with-lyrics')

    // The stored value must follow the UI: leaving it set would persist a
    // contradiction the settings page itself refuses to show.
    const saved = saveSettings.mock.calls.at(-1)?.[0] as Record<string, unknown>
    expect(saved.download_with_lyric_translation).toBe(false)
  })

  it('persists the three content switches', async () => {
    const wrapper = mountSettings()
    await toggle(wrapper, 'download-with-lyrics')
    await toggle(wrapper, 'download-with-cover')

    const saved = saveSettings.mock.calls.at(-1)?.[0] as Record<string, unknown>
    expect(saved.download_with_lyrics).toBe(true)
    expect(saved.download_with_cover).toBe(false)
  })

  it('shows the default hint while no directory is set', () => {
    const wrapper = mountSettings()
    const input = wrapper.find('[data-test="download-dir"]')
    // An empty value means "use the default"; the hint names it so the user is
    // not left guessing where files will land.
    expect(input.attributes('placeholder')).toBe('settings.download.dir_placeholder')
    expect(input.attributes('value')).toBe('')
    expect(wrapper.find('[data-test="download-reset"]').exists()).toBe(false)
  })

  it('saves the directory the picker returned', async () => {
    api.SelectFolder.mockResolvedValue('/music/downloads')
    const wrapper = mountSettings()

    await wrapper.find('[data-test="download-browse"]').trigger('click')
    await flushPromises()

    expect(api.SelectFolder).toHaveBeenCalled()
    expect(saveSettings).toHaveBeenCalledWith(expect.objectContaining({ download_dir: '/music/downloads' }))
  })

  it('keeps the setting untouched when the picker is cancelled', async () => {
    api.SelectFolder.mockResolvedValue('')
    const wrapper = mountSettings()

    await wrapper.find('[data-test="download-browse"]').trigger('click')
    await flushPromises()

    expect(saveSettings).not.toHaveBeenCalled()
  })
})
