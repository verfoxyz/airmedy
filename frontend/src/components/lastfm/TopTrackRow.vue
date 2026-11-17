<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { AlertTriangle, Heart, MoreVertical, Music, Play, Star, Volume2 } from '@lucide/vue'
import { useI18n } from 'vue-i18n'
import LazyImg from '@/components/LazyImg.vue'
import { LASTFM_ROW_GRID } from '@/lib/lastfmRowColumns'
import type { TopTrackItemDTO } from '../../../bindings/airmedy/internal/infra/wails/models'

/**
 * One row of the Last.fm recommendation table.
 *
 * The row renders the columns (#, title, artist, status, actions) and emits
 * what the page should do; it never resolves a source itself. Rows keep a fixed
 * 56px height so the header and every column stay aligned while the page
 * scrolls; the artist column collapses into a subtitle on narrow containers.
 */
const props = defineProps<{
  item: TopTrackItemDTO
  index: number
  current?: boolean
}>()

const emit = defineEmits<{
  (e: 'play', item: TopTrackItemDTO): void
  (e: 'save', item: TopTrackItemDTO): void
  (e: 'favorite', item: TopTrackItemDTO): void
  (e: 'artist', item: TopTrackItemDTO): void
  (e: 'menu', event: MouseEvent, item: TopTrackItemDTO): void
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
const coverFailed = ref(false)
watch(coverSrc, () => {
  coverFailed.value = false
})

const artistLine = computed(
  () => (props.item.artists ?? []).filter(Boolean).join('、') || props.item.artist,
)
const matching = computed(() => props.item.matching === true)
const noSource = computed(() => !matching.value && props.item.no_source === true)
const matched = computed(
  () => !matching.value && !noSource.value && (!!props.item.provider || !!props.item.local_track_id),
)
// A promoted row keeps its remote identity; "already on disk" always wins.
const inLibrary = computed(
  () => props.item.state === 'in_library' || !!props.item.local_track_id,
)
const suspicious = computed(() => props.item.suspicious === true)
const canPlay = computed(() => matched.value)
const canSave = computed(
  () => matched.value && !inLibrary.value && props.item.downloadable && !!props.item.provider,
)
const canOpenArtist = computed(() => !!props.item.local_track_id)

const statusKey = computed(() => {
  if (matching.value) return 'matching'
  if (noSource.value) return 'no-match'
  if (suspicious.value) return 'suspicious'
  if (inLibrary.value) return 'in_library'
  return 'matched'
})

const statusLabel = computed(() => {
  switch (statusKey.value) {
    case 'matching':
      return t('lastfm.matching')
    case 'no-match':
      return t('lastfm.no_match')
    case 'suspicious':
      return t('common.suspicious_match')
    case 'in_library':
      return t('lastfm.in_library')
    default:
      return t('lastfm.matched_from', { source: props.item.provider })
  }
})

const statusClass = computed(() => {
  switch (statusKey.value) {
    case 'matching':
    case 'no-match':
      return 'bg-foreground/[0.06] text-dim'
    case 'suspicious':
      return 'bg-foreground/[0.06] text-dim ring-1 ring-foreground/[0.10]'
    default:
      return 'bg-primary/10 text-primary'
  }
})

function play() {
  if (!canPlay.value) return
  emit('play', props.item)
}

function onDoubleClick() {
  play()
}

function onMenu(event: MouseEvent) {
  emit('menu', event, props.item)
}
</script>

<template>
  <div
    class="group grid h-14 items-center gap-2 px-2 text-sm transition-colors hover:bg-foreground/[0.04]"
    :class="LASTFM_ROW_GRID"
    data-test="top-track-row"
    :data-state="statusKey"
    @dblclick="onDoubleClick"
  >
    <!-- # / playing state -->
    <div class="flex items-center justify-center text-xs tabular-nums text-dim" data-test="top-track-index">
      <Volume2 v-if="current" class="h-4 w-4 text-primary" aria-hidden="true" />
      <span v-else>{{ index + 1 }}</span>
    </div>

    <!-- Title (+ cover, artist subtitle when the artist column is hidden) -->
    <div class="flex min-w-0 items-center gap-2">
      <div
        class="h-10 w-10 shrink-0 overflow-hidden rounded-md bg-foreground/[0.06] ring-1 ring-foreground/[0.06]"
      >
        <LazyImg
          v-if="coverSrc && !coverFailed"
          :src="coverSrc"
          :alt="item.title"
          class="h-full w-full object-cover"
          data-test="top-track-cover"
          @error="coverFailed = true"
        />
        <div
          v-else
          class="flex h-full w-full items-center justify-center text-dimmer"
          data-test="top-track-cover-placeholder"
        >
          <Music class="h-4 w-4" aria-hidden="true" />
        </div>
      </div>

      <div class="min-w-0 flex-1">
      <div class="flex min-w-0 items-baseline gap-2">
        <button
          v-if="canPlay"
          type="button"
          class="block min-w-0 max-w-full truncate text-left font-medium text-foreground transition-colors hover:text-primary"
          :title="item.title"
          data-test="top-track-title"
          @click="play"
        >
          {{ item.title }}
        </button>
        <p v-else class="min-w-0 truncate font-medium text-foreground" :title="item.title" data-test="top-track-title">
          {{ item.title }}
        </p>
        <span
          v-if="item.play_count > 0"
          class="shrink-0 text-[11px] tabular-nums text-dim"
          data-test="top-track-plays"
        >
          {{ t('lastfm.plays_count', { count: item.play_count }) }}
        </span>
      </div>
      <p class="truncate text-xs text-dim @md:hidden" data-test="top-track-artist-mobile">
        {{ artistLine }}
      </p>
      </div>
    </div>

    <!-- Artist -->
    <div class="hidden min-w-0 @md:block">
      <button
        v-if="canOpenArtist"
        type="button"
        class="block max-w-full truncate text-left text-dim transition-colors hover:text-foreground"
        :title="artistLine"
        data-test="top-track-artist"
        @click="emit('artist', item)"
      >
        {{ artistLine }}
      </button>
      <span v-else class="block truncate text-dim" :title="artistLine" data-test="top-track-artist">
        {{ artistLine }}
      </span>
    </div>

    <!-- Match status -->
    <div class="min-w-0">
      <span
        class="inline-flex max-w-full truncate rounded-full px-2 py-0.5 text-[11px] font-medium"
        :class="statusClass"
        :title="suspicious ? t('common.suspicious_match_hint') : undefined"
        data-test="top-track-status"
        :data-state="statusKey"
      >
        <AlertTriangle v-if="statusKey === 'suspicious'" class="mr-1 h-3 w-3 shrink-0" aria-hidden="true" />
        {{ statusLabel }}
      </span>
    </div>

    <!-- Actions -->
    <div class="flex items-center justify-end gap-1 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100">
      <button
        type="button"
        class="flex h-8 w-8 items-center justify-center rounded-full text-foreground/60 transition-colors hover:bg-foreground/[0.06] hover:text-foreground/80 disabled:pointer-events-none disabled:opacity-30"
        data-test="top-track-play"
        :aria-label="t('lastfm.play')"
        :title="canPlay ? t('lastfm.play') : t('lastfm.matching')"
        :disabled="!canPlay"
        @click.stop="play"
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
      <button
        v-else-if="inLibrary"
        type="button"
        class="flex h-8 w-8 items-center justify-center rounded-full text-foreground/60 transition-colors hover:bg-foreground/[0.06] hover:text-foreground/80"
        data-test="top-track-favorite"
        :aria-label="t('context_menu.add_to_favorites')"
        :title="t('context_menu.add_to_favorites')"
        @click.stop="emit('favorite', item)"
      >
        <Star class="h-4 w-4" />
      </button>
      <button
        type="button"
        class="flex h-8 w-8 items-center justify-center rounded-full text-foreground/60 transition-colors hover:bg-foreground/[0.06] hover:text-foreground/80"
        data-test="top-track-more"
        :aria-label="t('lastfm.more_actions')"
        :title="t('lastfm.more_actions')"
        @click.stop="onMenu"
      >
        <MoreVertical class="h-4 w-4" />
      </button>
    </div>
  </div>
</template>
