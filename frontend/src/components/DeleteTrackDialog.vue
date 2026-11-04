<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertTriangle, ListX, Loader2, Trash2 } from '@lucide/vue'
import { Modal } from '@airmedy/ui'
import { useDeleteTrackDialog } from '@/composables/useDeleteTrackDialog'
import * as LibraryService from '../../bindings/airmedy/internal/infra/wails/libraryservice'
import * as PlaylistService from '../../bindings/airmedy/internal/infra/wails/playlistservice'

const { t } = useI18n()
const { isVisible, targetTrack, targetPlaylistId, close } = useDeleteTrackDialog()

const isWorking = ref(false)
const error = ref('')

/** A manual playlist row offers the membership-only option; favorites and
 *  smart playlists have nothing removable. */
const canRemoveFromPlaylist = computed(
  () => !!targetPlaylistId.value && targetPlaylistId.value !== 'favorites',
)

/** Remote rows are catalogue entries with no file on disk to unlink. */
const hasLocalFile = computed(() => {
  const path = targetTrack.value?.path ?? ''
  return !!path && !path.startsWith('remote://')
})

watch(isVisible, (open) => {
  if (open) {
    isWorking.value = false
    error.value = ''
  }
})

function errorText(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

async function removeFromPlaylist() {
  const track = targetTrack.value
  const playlistId = targetPlaylistId.value
  if (!track || !playlistId || isWorking.value) return
  isWorking.value = true
  error.value = ''
  try {
    await PlaylistService.RemoveTrackFromPlaylist(playlistId, track.id, '')
    close()
  } catch (e) {
    error.value = errorText(e)
  } finally {
    isWorking.value = false
  }
}

async function deleteFile() {
  const track = targetTrack.value
  if (!track || !hasLocalFile.value || isWorking.value) return
  isWorking.value = true
  error.value = ''
  try {
    await LibraryService.DeleteTrackFile(track.id)
    close()
  } catch (e) {
    // The backend reports the OS reason (file in use, permission denied); show
    // it verbatim so the user knows what to fix instead of a generic failure.
    error.value = errorText(e)
  } finally {
    isWorking.value = false
  }
}
</script>

<template>
  <Modal
    :open="isVisible"
    :title="t('delete_track.title')"
    width-class="w-[26rem]"
    @close="close"
  >
    <p class="mb-4 text-sm text-dim">{{ t('delete_track.description') }}</p>

    <div class="space-y-2">
      <button
        v-if="canRemoveFromPlaylist"
        data-test="delete-option-playlist"
        class="flex w-full items-start gap-3 rounded-xl border border-foreground/[0.06] px-4 py-3 text-left transition-colors hover:bg-foreground/[0.04] disabled:opacity-50"
        :disabled="isWorking"
        @click="removeFromPlaylist"
      >
        <ListX class="mt-0.5 h-4 w-4 shrink-0 text-dim" />
        <span class="min-w-0">
          <span class="block text-sm font-medium text-foreground">{{ t('delete_track.remove_from_playlist') }}</span>
          <span class="mt-0.5 block text-xs text-subdued">{{ t('delete_track.remove_hint') }}</span>
        </span>
      </button>

      <button
        data-test="delete-option-file"
        class="flex w-full items-start gap-3 rounded-xl border border-foreground/[0.06] px-4 py-3 text-left transition-colors hover:bg-destructive/10 disabled:opacity-50"
        :disabled="isWorking || !hasLocalFile"
        @click="deleteFile"
      >
        <Trash2 class="mt-0.5 h-4 w-4 shrink-0 text-destructive" />
        <span class="min-w-0">
          <span class="block text-sm font-medium text-destructive">{{ t('delete_track.delete_file') }}</span>
          <span class="mt-0.5 block text-xs text-subdued">
            {{ hasLocalFile ? t('delete_track.file_warning') : t('delete_track.no_local_file') }}
          </span>
        </span>
      </button>
    </div>

    <div v-if="error" class="mt-4 flex items-start gap-2 rounded-xl border border-destructive/20 bg-destructive/10 px-3 py-2 text-xs text-destructive" data-test="delete-error">
      <AlertTriangle class="mt-0.5 h-3.5 w-3.5 shrink-0" />
      <span class="min-w-0 break-words">{{ t('delete_track.failed', { reason: error }) }}</span>
    </div>

    <div class="mt-5 flex justify-end">
      <button
        data-test="delete-cancel"
        class="inline-flex items-center gap-2 rounded-lg px-4 py-2 text-sm text-dim transition-colors hover:bg-foreground/[0.04]"
        :disabled="isWorking"
        @click="close"
      >
        <Loader2 v-if="isWorking" class="h-3.5 w-3.5 animate-spin" />
        {{ t('common.cancel') }}
      </button>
    </div>
  </Modal>
</template>
