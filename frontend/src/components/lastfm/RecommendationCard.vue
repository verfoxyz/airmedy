<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Check, Heart, Music, Play } from '@lucide/vue'
import { useI18n } from 'vue-i18n'
import LazyImg from '@/components/LazyImg.vue'
import type { RecommendationTrackDTO } from '../../../bindings/airmedy/internal/infra/wails/models'

/**
 * One recommendation card.
 *
 * The row shape is the catalogue row shape, so the parent hands the same object
 * to the shared play/save actions the cloud search uses. This component only
 * renders and emits; it owns no request.
 */
const props = defineProps<{
  track: RecommendationTrackDTO
}>()

const emit = defineEmits<{
  (e: 'play', track: RecommendationTrackDTO): void
  (e: 'save', track: RecommendationTrackDTO): void
}>()

const { t } = useI18n()

const imageFailed = ref(false)
watch(() => props.track.cover_url, () => {
  imageFailed.value = false
})

const artistLine = computed(() => (props.track.artists ?? []).filter(Boolean).join(' · '))
const inLibrary = computed(() => props.track.state === 'in_library')
const canPlay = computed(() => props.track.downloadable || !!props.track.local_track_id || inLibrary.value)
const canSave = computed(() => props.track.downloadable && (!inLibrary.value || !!props.track.local_track_id))
</script>

<template>
  <div class="group min-w-0" data-test="recommendation-card">
    <div class="relative aspect-square overflow-hidden rounded-xl bg-foreground/[0.06] ring-1 ring-foreground/[0.06]">
      <LazyImg
        v-if="track.cover_url && !imageFailed"
        :src="track.cover_url"
        :alt="track.title"
        class="h-full w-full object-cover transition-transform duration-500 group-hover:scale-105"
        @error="imageFailed = true"
      />
      <div v-else class="flex h-full w-full items-center justify-center text-dimmer">
        <Music class="h-8 w-8" aria-hidden="true" />
      </div>

      <!-- Imported files and finished downloads are already playable. -->
      <span
        v-if="inLibrary"
        class="absolute left-2 top-2 inline-flex items-center gap-1 rounded-full bg-background/80 px-2 py-0.5 text-[10px] font-medium text-dim"
        data-test="recommendation-in-library"
      >
        <Check class="h-3 w-3" aria-hidden="true" />
        {{ t('lastfm.in_library') }}
      </span>

      <!-- Actions appear on hover, like every other list row. -->
      <div class="absolute inset-x-2 bottom-2 flex items-center justify-end gap-1 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100">
        <button
          v-if="canPlay"
          type="button"
          class="flex h-9 w-9 items-center justify-center rounded-full bg-background/85 text-foreground/70 transition-colors hover:bg-background hover:text-foreground"
          data-test="recommendation-play"
          :aria-label="t('lastfm.play')"
          :title="t('lastfm.play')"
          @click.stop="emit('play', track)"
        >
          <Play class="h-4 w-4" />
        </button>
        <button
          v-if="canSave"
          type="button"
          class="flex h-9 w-9 items-center justify-center rounded-full bg-background/85 text-foreground/70 transition-colors hover:bg-background hover:text-foreground"
          data-test="recommendation-save"
          :aria-label="t('lastfm.save')"
          :title="t('lastfm.save')"
          @click.stop="emit('save', track)"
        >
          <Heart class="h-4 w-4" />
        </button>
      </div>
    </div>

    <p class="mt-2 truncate text-sm font-semibold text-foreground" :title="track.title">{{ track.title }}</p>
    <p class="truncate text-xs text-dim" :title="artistLine">{{ artistLine }}</p>
    <p v-if="track.provider" class="mt-0.5 truncate text-[10px] uppercase tracking-wide text-dimmer">
      {{ track.provider }}
    </p>
  </div>
</template>
