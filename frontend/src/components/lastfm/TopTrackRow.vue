<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { AlertTriangle, Heart, Music, Play } from '@lucide/vue'
import { useI18n } from 'vue-i18n'
import LazyImg from '@/components/LazyImg.vue'
import type { TopTrackItemDTO } from '../../../bindings/airmedy/internal/infra/wails/models'

/**
 * One row of the MVP Last.fm top-tracks page.
 *
 * The row owns only rendering: the page passes the item and receives play/save
 * requests. Its state chip is one of matching / matched+source / no match.
 */
const props = defineProps<{
  item: TopTrackItemDTO
}>()

const emit = defineEmits<{
  (e: 'play', item: TopTrackItemDTO): void
  (e: 'save', item: TopTrackItemDTO): void
}>()

const { t } = useI18n()

// Last.fm still returns its default star image for many tracks. It is not
// artwork, so treat it as absent and let the placeholder render instead of a
// blank or broken block.
const LASTFM_PLACEHOLDER_IMAGE = '2a96cbd8b46e442fc41c2b86b821562f'

const coverSrc = computed(() => {
  const url = (props.item.cover_url ?? '').trim()
  return url === '' || url.includes(LASTFM_PLACEHOLDER_IMAGE) ? '' : url
})

const imageFailed = ref(false)
watch(coverSrc, () => {
  imageFailed.value = false
})

const artistLine = computed(
  () => (props.item.artists ?? []).filter(Boolean).join(' · ') || props.item.artist,
)
const matching = computed(() => props.item.matching === true)
const noMatch = computed(() => !matching.value && props.item.no_source === true)
const matched = computed(
  () => !matching.value && !noMatch.value && (!!props.item.provider || !!props.item.local_track_id),
)
// A promoted row keeps its remote identity, so "already in library" must win
// over "from {source}": the song is on disk and there is nothing to download.
const inLibrary = computed(
  () => props.item.state === 'in_library' || !!props.item.local_track_id,
)
const matchedLabel = computed(() =>
  inLibrary.value
    ? t('lastfm.in_library')
    : t('lastfm.matched_from', { source: props.item.provider }),
)
const canPlay = computed(() => matched.value)
const canSave = computed(
  () => matched.value && !inLibrary.value && props.item.downloadable && !!props.item.provider,
)
</script>

<template>
  <div
    class="group flex h-14 items-center gap-3 px-2 transition-colors hover:bg-foreground/[0.04]"
    data-test="top-track-row"
  >
    <div class="h-8 w-8 flex-shrink-0 overflow-hidden rounded bg-foreground/[0.06] ring-1 ring-foreground/[0.06]">
      <LazyImg
        v-if="coverSrc && !imageFailed"
        :src="coverSrc"
        :alt="item.title"
        class="h-full w-full object-cover"
        @error="imageFailed = true"
      />
      <div v-else class="flex h-full w-full items-center justify-center text-dimmer">
        <Music class="h-4 w-4" aria-hidden="true" />
      </div>
    </div>

    <div class="min-w-0 flex-1">
      <p class="truncate text-sm font-medium text-foreground" :title="item.title">{{ item.title }}</p>
      <p class="truncate text-xs text-dim" :title="artistLine">{{ artistLine }}</p>
      <p v-if="item.album" class="truncate text-[11px] text-dimmer">{{ item.album }}</p>
    </div>

    <span
      v-if="item.play_count > 0"
      class="shrink-0 text-xs tabular-nums text-dim"
      data-test="top-track-plays"
    >
      {{ t('lastfm.plays_count', { count: item.play_count }) }}
    </span>

    <span
      v-if="matching"
      class="shrink-0 rounded-full bg-foreground/[0.06] px-2.5 py-1 text-xs text-dim"
      data-test="top-track-status"
      data-state="matching"
    >
      {{ t('lastfm.matching') }}
    </span>
    <span
      v-else-if="noMatch"
      class="shrink-0 rounded-full bg-foreground/[0.06] px-2.5 py-1 text-xs text-dim"
      data-test="top-track-status"
      data-state="no-match"
    >
      {{ t('lastfm.no_match') }}
    </span>
    <span
      v-else
      class="shrink-0 rounded-full bg-primary/10 px-2.5 py-1 text-xs font-medium text-primary"
      data-test="top-track-status"
      data-state="matched"
    >
      {{ matchedLabel }}
    </span>

    <span
      v-if="item.suspicious"
      class="inline-flex shrink-0 items-center gap-1 rounded-full bg-foreground/[0.06] px-2.5 py-1 text-xs text-dim"
      :title="t('common.suspicious_match_hint')"
      data-test="top-track-suspicious"
    >
      <AlertTriangle class="h-3.5 w-3.5" aria-hidden="true" />
      {{ t('common.suspicious_match') }}
    </span>

    <div v-if="canPlay || canSave" class="flex shrink-0 items-center gap-1">
      <button
        v-if="canPlay"
        type="button"
        class="flex h-8 w-8 items-center justify-center rounded-full text-foreground/60 transition-colors hover:bg-foreground/[0.06] hover:text-foreground/80"
        data-test="top-track-play"
        :aria-label="t('lastfm.play')"
        :title="t('lastfm.play')"
        @click.stop="emit('play', item)"
      >
        <Play class="h-4 w-4" />
      </button>
      <button
        v-if="canSave"
        type="button"
        class="flex h-8 w-8 items-center justify-center rounded-full text-foreground/60 transition-colors hover:bg-foreground/[0.06] hover:text-foreground/80"
        data-test="top-track-save"
        :aria-label="t('lastfm.save')"
        :title="t('lastfm.save')"
        @click.stop="emit('save', item)"
      >
        <Heart class="h-4 w-4" />
      </button>
    </div>
  </div>
</template>
