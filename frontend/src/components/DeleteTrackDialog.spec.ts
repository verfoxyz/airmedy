import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import en from '../locales/en.json'

const deleteTrackFile = vi.fn().mockResolvedValue(undefined)
const removeTrackFromPlaylist = vi.fn().mockResolvedValue(undefined)

vi.mock('../../bindings/airmedy/internal/infra/wails/libraryservice', () => ({
  DeleteTrackFile: (...args: unknown[]) => deleteTrackFile(...args),
}))

vi.mock('../../bindings/airmedy/internal/infra/wails/playlistservice', () => ({
  RemoveTrackFromPlaylist: (...args: unknown[]) => removeTrackFromPlaylist(...args),
}))

import DeleteTrackDialog from './DeleteTrackDialog.vue'
import { useDeleteTrackDialog } from '@/composables/useDeleteTrackDialog'

const localTrack = {
  id: 'track-1',
  path: 'C:/Music/song.flac',
  title: 'Song',
} as never

const remoteTrack = {
  id: 'remote-1',
  path: 'remote://wy/123',
  title: 'Remote',
} as never

function mountDialog() {
  return mount(DeleteTrackDialog, {
    global: {
      plugins: [createI18n({ legacy: false, locale: 'en', messages: { en } })],
      stubs: { Modal: { template: '<div><slot /></div>' } },
    },
  })
}

describe('DeleteTrackDialog', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    useDeleteTrackDialog().close()
  })

  it('removes only the playlist membership when that option is chosen', async () => {
    useDeleteTrackDialog().open(localTrack, 'playlist-1')
    const wrapper = mountDialog()
    await wrapper.vm.$nextTick()

    await wrapper.get('[data-test="delete-option-playlist"]').trigger('click')

    expect(removeTrackFromPlaylist).toHaveBeenCalledWith('playlist-1', 'track-1', '')
    expect(deleteTrackFile).not.toHaveBeenCalled()
    expect(useDeleteTrackDialog().isVisible.value).toBe(false)
    wrapper.unmount()
  })

  it('deletes the file and closes when the destructive option is chosen', async () => {
    useDeleteTrackDialog().open(localTrack, 'playlist-1')
    const wrapper = mountDialog()
    await wrapper.vm.$nextTick()

    await wrapper.get('[data-test="delete-option-file"]').trigger('click')

    expect(deleteTrackFile).toHaveBeenCalledWith('track-1')
    expect(removeTrackFromPlaylist).not.toHaveBeenCalled()
    expect(useDeleteTrackDialog().isVisible.value).toBe(false)
    wrapper.unmount()
  })

  it('shows the backend reason and stays open when deleting fails', async () => {
    deleteTrackFile.mockRejectedValueOnce(new Error('the file is in use by another program'))
    useDeleteTrackDialog().open(localTrack, 'playlist-1')
    const wrapper = mountDialog()
    await wrapper.vm.$nextTick()

    await wrapper.get('[data-test="delete-option-file"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="delete-error"]').text()).toContain('the file is in use by another program')
    expect(useDeleteTrackDialog().isVisible.value).toBe(true)
    wrapper.unmount()
  })

  it('disables deleting a remote track and hides the playlist option outside playlists', async () => {
    useDeleteTrackDialog().open(remoteTrack)
    const wrapper = mountDialog()
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-test="delete-option-playlist"]').exists()).toBe(false)
    expect(wrapper.get('[data-test="delete-option-file"]').attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })
})
