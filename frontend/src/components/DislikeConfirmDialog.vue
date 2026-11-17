<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { AlertTriangle } from '@lucide/vue'
import { Modal } from '@airmedy/ui'
import { DISLIKE_KIND_ARTIST, useDislikeStore } from '@/stores/dislike'
import { useAppStore } from '@/stores/app'

/**
 * Confirmation for a dislike that would delete local files.
 *
 * The purge setting is destructive and irreversible, so it always goes through
 * this dialog. Marking only is offered beside it; an artist-level purge needs
 * the explicit acknowledgement because it can remove many files at once.
 */
const store = useDislikeStore()
const appStore = useAppStore()
const acknowledged = ref(false)

const purgeOn = computed(() => appStore.dislikePurgeFiles)
const isArtist = computed(() => store.pending?.kind === DISLIKE_KIND_ARTIST)
const titleKey = computed(() =>
  purgeOn.value ? 'settings.dislike_purge_confirm_title' : 'settings.dislike_mark_confirm_title',
)
const bodyKey = computed(() =>
  purgeOn.value ? 'settings.dislike_purge_confirm_body' : 'settings.dislike_mark_confirm_body',
)
const label = computed(() => {
  const pending = store.pending
  if (!pending) return ''
  if (!pending.artist) return pending.title
  return (pending.title || pending.artist) + ' · ' + pending.artist
})
// The acknowledgement is only needed for the destructive artist-level purge.
const canConfirm = computed(() => !purgeOn.value || !isArtist.value || acknowledged.value)

watch(
  () => store.pending,
  () => {
    acknowledged.value = false
  },
)

function confirm() {
  if (!canConfirm.value) return
  void store.confirmPending(purgeOn.value)
}
</script>

<template>
  <Modal
    :open="!!store.pending"
    :title="$t(titleKey)"
    @close="store.dismissPending()"
  >
    <div class="space-y-4">
      <div
        v-if="purgeOn"
        class="flex items-start gap-2 rounded-xl border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive"
        data-test="dislike-purge-warning"
      >
        <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" />
        <span>{{ $t(bodyKey) }}</span>
      </div>
      <p v-else class="text-sm text-dim" data-test="dislike-mark-body">{{ $t(bodyKey) }}</p>

      <p class="text-sm font-medium text-foreground" data-test="dislike-purge-target">{{ label }}</p>

      <p v-if="purgeOn" class="text-sm text-dim" data-test="dislike-purge-count">
        {{ $t('settings.dislike_purge_count', { count: store.pendingCount }) }}
      </p>

      <label v-if="purgeOn && isArtist" class="flex items-start gap-2 text-sm text-foreground">
        <input
          v-model="acknowledged"
          type="checkbox"
          class="mt-0.5 h-4 w-4 accent-primary"
          data-test="dislike-purge-acknowledge"
        />
        <span>{{ $t('settings.dislike_purge_confirm_artist', { count: store.pendingCount }) }}</span>
      </label>

      <p v-if="store.purgeError" class="text-xs text-destructive" role="alert" data-test="dislike-purge-error">
        {{ store.purgeError }}
      </p>
    </div>

    <template #footer>
      <div class="flex justify-end gap-2">
        <button
          type="button"
          class="rounded-lg px-3 py-1.5 text-sm font-medium text-dim transition-colors hover:bg-foreground/5 hover:text-foreground"
          data-test="dislike-cancel"
          :disabled="store.saving"
          @click="store.dismissPending()"
        >
          {{ $t('common.cancel') }}
        </button>
        <button
          type="button"
          class="rounded-lg bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 disabled:cursor-not-allowed disabled:opacity-40"
          data-test="dislike-confirm"
          :disabled="!canConfirm || store.saving || store.pendingLoading"
          @click="confirm"
        >
          {{ purgeOn ? $t('settings.dislike_purge_confirm') : $t('common.confirm') }}
        </button>
      </div>
    </template>
  </Modal>
</template>
