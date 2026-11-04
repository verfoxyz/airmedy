import { ref } from 'vue'
import type { TrackDTO } from '../../bindings/airmedy/internal/domain/models'

// Module-level state, like useFindLyricsDialog: the menu opens the dialog and
// the single dialog instance in App.vue reads the same state.
const isVisible = ref(false)
const targetTrack = ref<TrackDTO | null>(null)
const targetPlaylistId = ref<string | null>(null)

export function useDeleteTrackDialog() {
  /**
   * Opens the delete dialog. playlistId is the playlist the row was opened from,
   * or undefined outside a playlist (and for smart playlists, whose membership
   * is derived rather than stored).
   */
  function open(track: TrackDTO, playlistId?: string) {
    targetTrack.value = track
    targetPlaylistId.value = playlistId ?? null
    isVisible.value = true
  }

  function close() {
    isVisible.value = false
    targetTrack.value = null
    targetPlaylistId.value = null
  }

  return {
    isVisible,
    targetTrack,
    targetPlaylistId,
    open,
    close,
  }
}
