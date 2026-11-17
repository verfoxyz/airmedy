<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { Loader2, ThumbsDown, UserX, X } from '@lucide/vue'
import { DISLIKE_KIND_ARTIST, useDislikeStore } from '@/stores/dislike'
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

const items = computed(() => dislikeStore.items)

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
    <p class="mb-4 text-sm text-dim">{{ $t('settings.dislikes.description') }}</p>

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
  </SettingSection>
</template>
