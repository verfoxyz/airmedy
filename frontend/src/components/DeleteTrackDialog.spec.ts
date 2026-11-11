import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import en from '../locales/en.json'

const deleteTrackFile = vi.fn().mockResolvedValue(undefined)
const removeTrackFromLibrary = vi.fn().mockResolvedValue(undefined)
const removeTrackFromPlaylist = vi.fn().mockResolvedValue(undefined)

vi.mock('../../bindings/airmedy/internal/infra/wails/libraryservice', () => ({
  DeleteTrackFile: (...args: unknown[]) => deleteTrackFile(...args),
  RemoveTrackFromLibrary: (...args: unknown[]) => removeTrackFromLibrary(...args),
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

  it('offers removing a remote track from the library instead of deleting a file', async () => {
    useDeleteTrackDialog().open(remoteTrack, 'playlist-1')
    const wrapper = mountDialog()
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-test="delete-option-file"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="delete-option-playlist"]').exists()).toBe(true)
    const option = wrapper.get('[data-test="delete-option-library"]')
    expect(option.text()).toContain('Remove from library')

    await option.trigger('click')

    expect(removeTrackFromLibrary).toHaveBeenCalledWith('remote-1')
    expect(deleteTrackFile).not.toHaveBeenCalled()
    expect(useDeleteTrackDialog().isVisible.value).toBe(false)
    wrapper.unmount()
  })

  it('keeps the file option for a local row and hides the library removal', async () => {
    useDeleteTrackDialog().open(localTrack)
    const wrapper = mountDialog()
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-test="delete-option-library"]').exists()).toBe(false)
    expect(wrapper.get('[data-test="delete-option-file"]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('shows the backend reason when the library removal fails', async () => {
    removeTrackFromLibrary.mockRejectedValueOnce(new Error('remove track: track not found'))
    useDeleteTrackDialog().open(remoteTrack)
    const wrapper = mountDialog()
    await wrapper.vm.$nextTick()

    await wrapper.get('[data-test="delete-option-library"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="delete-error"]').text()).toContain('track not found')
    expect(useDeleteTrackDialog().isVisible.value).toBe(true)
    wrapper.unmount()
  })
})
