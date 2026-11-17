<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Loader2, ThumbsDown, UserX, X } from '@lucide/vue'
import { Switch } from '@airmedy/ui'
import { DISLIKE_KIND_ARTIST, useDislikeStore } from '@/stores/dislike'
import { useAppStore } from '@/stores/app'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import SettingSection from './SettingSection.vue'

/**
 * The local recommendation blocklist.
 *
 * A dislike hides a song or an artist from Discover and Last.fm radio; the
 * library itself is untouched, which is why removal brings the row back on the
 * next refresh instead of restoring a file.
 */
const { t } = useI18n()
const dislikeStore = useDislikeStore()
const appStore = useAppStore()
const togglingPurge = ref(false)
// Turning the destructive setting on is confirmed first; the switch stays off
// until the user accepts, so a cancel writes nothing.
const enableConfirmOpen = ref(false)

const items = computed(() => dislikeStore.items)

async function applyPurge(enabled: boolean) {
  togglingPurge.value = true
  try {
    await appStore.updateDislikePurgeFiles(enabled)
  } catch (error) {
    console.error('Failed to save the dislike purge setting', error)
  } finally {
    togglingPurge.value = false
  }
}

/** Enabling asks first; disabling is immediate. */
function setPurge(enabled: boolean) {
  if (enabled) {
    enableConfirmOpen.value = true
    return
  }
  void applyPurge(false)
}

function cancelEnablePurge() {
  enableConfirmOpen.value = false
}

async function confirmEnablePurge() {
  enableConfirmOpen.value = false
  await applyPurge(true)
}

onMounted(() => dislikeStore.init())

function rowTitle(kind: string, title: string, artist: string): string {
  return kind === DISLIKE_KIND_ARTIST ? artist : title
}

function rowSubtitle(kind: string, artist: string): string {
  return kind === DISLIKE_KIND_ARTIST ? t('settings.dislikes.kind_artist') : artist
}

async function remove(id: string) {
  try {
    await dislikeStore.remove(id)
  } catch (error) {
    console.error('Failed to remove a dislike', error)
  }
}
</script>

<template>
  <SettingSection :icon="ThumbsDown" :label="$t('settings.dislikes.title')" variant="panel">
    <template #header-extra>
      <label class="flex items-center gap-2 text-xs font-medium text-dim">
        <span>{{ $t('settings.dislike_purge_files') }}</span>
        <Switch
          :model-value="appStore.dislikePurgeFiles"
          :disabled="togglingPurge"
          data-test="dislike-purge-toggle"
          @update:model-value="(value) => setPurge(Boolean(value))"
        />
      </label>
    </template>

    <p class="mb-4 text-sm text-dim">{{ $t('settings.dislikes.description') }}</p>
    <p
      v-if="appStore.dislikePurgeFiles"
      class="mb-4 rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive"
      data-test="dislike-purge-hint"
    >
      {{ $t('settings.dislike_purge_files_hint') }}
    </p>
    <p
      v-if="dislikeStore.purgeError"
      class="mb-4 text-xs text-destructive"
      role="alert"
      data-test="dislike-purge-error"
    >
      {{ dislikeStore.purgeError }}
    </p>

    <div
      v-if="dislikeStore.loading && items.length === 0"
      class="flex items-center justify-center py-8 text-dim"
      data-test="dislike-loading"
    >
      <Loader2 class="h-4 w-4 animate-spin" aria-hidden="true" />
    </div>

    <p
      v-else-if="items.length === 0"
      class="py-8 text-center text-sm text-dimmer"
      data-test="dislike-empty"
    >
      {{ $t('settings.dislikes.empty') }}
    </p>

    <div v-else class="-mx-6 -mb-6 divide-y divide-foreground/[0.06]">
      <div
        v-for="item in items"
        :key="item.id"
        class="flex items-center gap-3 px-6 py-3"
        data-test="dislike-row"
      >
        <component
          :is="item.kind === DISLIKE_KIND_ARTIST ? UserX : ThumbsDown"
          class="h-4 w-4 shrink-0 text-dim"
          aria-hidden="true"
        />
        <div class="min-w-0 flex-1">
          <p class="truncate text-sm text-foreground" :title="rowTitle(item.kind, item.title, item.artist)">
            {{ rowTitle(item.kind, item.title, item.artist) }}
          </p>
          <p class="truncate text-xs text-dim" :title="rowSubtitle(item.kind, item.artist)">
            {{ rowSubtitle(item.kind, item.artist) }}
          </p>
        </div>
        <button
          type="button"
          class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-dim transition-colors hover:bg-foreground/[0.04] hover:text-foreground"
          data-test="dislike-remove"
          :aria-label="$t('settings.dislikes.remove')"
          :title="$t('settings.dislikes.remove')"
          :disabled="dislikeStore.saving"
          @click="remove(item.id)"
        >
          <X class="h-4 w-4" aria-hidden="true" />
        </button>
      </div>
    </div>
    <ConfirmDialog
      :open="enableConfirmOpen"
      :title="$t('settings.dislike_enable_purge_title')"
      :message="$t('settings.dislike_enable_purge_body')"
      :confirm-label="$t('common.confirm')"
      :cancel-label="$t('common.cancel')"
      danger
      data-test="dislike-enable-confirm"
      @confirm="confirmEnablePurge"
      @cancel="cancelEnablePurge"
    />
  </SettingSection>

</template>
