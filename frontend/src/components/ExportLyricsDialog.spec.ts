import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import en from '../locales/en.json'

const api = vi.hoisted(() => ({
  ExportLyrics: vi.fn(),
  ExportLyricsAs: vi.fn(),
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/lyricsservice', () => ({
  ExportLyrics: (...args: unknown[]) => api.ExportLyrics(...args),
  ExportLyricsAs: (...args: unknown[]) => api.ExportLyricsAs(...args),
}))

import ExportLyricsDialog from './ExportLyricsDialog.vue'
import { useExportLyrics } from '@/composables/useExportLyrics'

const track = { id: 'track-1', path: 'C:/Music/song.flac' } as never

function mountDialog() {
  return mount(ExportLyricsDialog, {
    global: {
      plugins: [createI18n({ legacy: false, locale: 'en', messages: { en } })],
      stubs: { Modal: { template: '<div><slot /></div>' } },
    },
  })
}

describe('ExportLyricsDialog', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    useExportLyrics().close()
  })

  it('offers overwrite and save-as instead of silently overwriting', async () => {
    api.ExportLyrics.mockResolvedValueOnce({ path: 'C:/Music/song.lrc', exists: true })
    await useExportLyrics().open(track)
    const wrapper = mountDialog()
    await flushPromises()

    expect(wrapper.find('[data-test="export-overwrite"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="export-save-as"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="export-done"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('overwrites when that option is chosen', async () => {
    api.ExportLyrics
      .mockResolvedValueOnce({ path: 'C:/Music/song.lrc', exists: true })
      .mockResolvedValueOnce({ path: 'C:/Music/song.lrc', exists: false })
    await useExportLyrics().open(track)
    const wrapper = mountDialog()
    await flushPromises()

    await wrapper.get('[data-test="export-overwrite"]').trigger('click')
    await flushPromises()

    expect(api.ExportLyrics).toHaveBeenLastCalledWith('track-1', true)
    expect(wrapper.get('[data-test="export-done"]').text()).toContain('C:/Music/song.lrc')
    wrapper.unmount()
  })

  it('reports the saved path after save-as', async () => {
    api.ExportLyrics.mockResolvedValueOnce({ path: 'C:/Music/song.lrc', exists: true })
    api.ExportLyricsAs.mockResolvedValueOnce('D:/Lyrics/custom.lrc')
    await useExportLyrics().open(track)
    const wrapper = mountDialog()
    await flushPromises()

    await wrapper.get('[data-test="export-save-as"]').trigger('click')
    await flushPromises()

    expect(api.ExportLyricsAs).toHaveBeenCalledWith('track-1')
    expect(wrapper.get('[data-test="export-done"]').text()).toContain('D:/Lyrics/custom.lrc')
    wrapper.unmount()
  })

  it('notes that plain lyrics have no timeline after a successful export', async () => {
    api.ExportLyrics.mockResolvedValueOnce({ path: 'C:/Music/song.lrc', exists: false })
    await useExportLyrics().open(track, false)
    const wrapper = mountDialog()
    await flushPromises()

    expect(wrapper.find('[data-test="export-unsynced"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('shows the backend reason when there is nothing to export', async () => {
    api.ExportLyrics.mockRejectedValueOnce(new Error('lyrics are not available for this track'))
    await useExportLyrics().open(track)
    const wrapper = mountDialog()
    await flushPromises()

    expect(wrapper.get('[data-test="export-error"]').text()).toContain('lyrics are not available for this track')
    wrapper.unmount()
  })
})
