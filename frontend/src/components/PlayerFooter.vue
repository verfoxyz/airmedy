<script setup lang="ts">
import { ref, computed } from 'vue'
import {
  Play,
  Pause,
  SkipBack,
  SkipForward,
  Repeat,
  Repeat1,
  Shuffle,
  Volume2,
  VolumeX,
  Info,
  Music,
  ListMusic,
  Mic2,
  PictureInPicture2,
  AlertCircle,
  ThumbsDown,
} from '@lucide/vue'
import LazyImg from '@/components/LazyImg.vue'
import { usePlayerStore } from '../stores/player'
import { RepeatMode } from '../../bindings/airmedy/internal/domain/models'
import { formatTime, getTrackDisplayArtist, getTrackDisplayTitle } from '@airmedy/utils'
import { Slider } from '@airmedy/ui'
import * as WindowService from '../../bindings/airmedy/internal/infra/wails/windowservice'
import { useI18n } from 'vue-i18n'
import TrackContextMenu from './TrackContextMenu.vue'
import { MarqueeText } from '@airmedy/ui'
import PlayerControlButton from './player/PlayerControlButton.vue'
import PlayerQuickSettingsMenu from './player/PlayerQuickSettingsMenu.vue'
import { useAppStore } from '../stores/app'
import PlayerFavoriteButton from './player/PlayerFavoriteButton.vue'
import { useLastFmRadioStore } from '../stores/lastfmRadio'
import { DISLIKE_KIND_TRACK, useDislikeStore } from '../stores/dislike'

const { t } = useI18n()
const store = usePlayerStore()
const appStore = useAppStore()
const lastfmRadioStore = useLastFmRadioStore()
const dislikeStore = useDislikeStore()
const trackContextMenu = ref<InstanceType<typeof TrackContextMenu> | null>(null)
const quickSettingsMenu = ref<InstanceType<typeof PlayerQuickSettingsMenu> | null>(null)

function openArtworkContextMenu(e: MouseEvent) {
  if (!store.currentTrack) return
  trackContextMenu.value?.open(e, store.currentTrack, { excludePlayNext: true, excludeAddToQueue: true })
}

function openQuickSettingsMenu(e: MouseEvent) {
  const target = e.target as HTMLElement
  if (target.closest('button, [data-player-footer-interactive="true"]')) return
  quickSettingsMenu.value?.open(e)
}

const isSeeking = ref(false)
const seekValue = ref(0)

const displayPosition = computed(() =>
  isSeeking.value ? (seekValue.value / 100) * store.duration : store.position,
)

const trackTitle = computed(() => store.currentTrack ? (getTrackDisplayTitle(store.currentTrack) || t('player.not_playing')) : t('player.not_playing'))
const trackArtist = computed(() => {
  if (!store.currentTrack) return t('player.select_track')
  // A remote row has no artist entities, only the text snapshot from search.
  return getTrackDisplayArtist(store.currentTrack) || t('library.unknown_artist')
})

const repeatIcon = computed(() =>
  store.repeatMode === RepeatMode.RepeatModeOne ? Repeat1 : Repeat,
)
const repeatActive = computed(
  () =>
    store.repeatMode === RepeatMode.RepeatModeOne ||
    store.repeatMode === RepeatMode.RepeatModeAll,
)

function onSeekStart() {
  isSeeking.value = true
}

async function onSeekEnd() {
  await store.seek((seekValue.value / 100) * store.duration)
  isSeeking.value = false
}

/**
 * dislikeCurrent excludes the playing radio row and moves on. The skip runs
 * even when the write fails, so one bad database write cannot stall playback.
 */
async function dislikeCurrent() {
  const track = store.currentTrack
  if (!track) return
  const artist = getTrackDisplayArtist(track)
  const title = getTrackDisplayTitle(track)
  if (!artist || !title) {
    await store.next()
    return
  }
  try {
    await dislikeStore.requestDislike({
      kind: DISLIKE_KIND_TRACK,
      matchKey: '',
      title,
      artist,
      onConfirmed: () => {
        void store.next()
      },
    })
  } catch (error) {
    console.error('Failed to dislike the playing radio track', error)
    await store.next()
  }
}

</script>

<template>
  <div
    class="h-[72px] bg-background border-t border-foreground/[0.06] flex items-center justify-between px-6 gap-6 select-none"
    @contextmenu.prevent="openQuickSettingsMenu">
    <!-- Track Info -->
    <div class="flex items-center justify-start gap-3 w-1/4 min-w-[200px]">
      <button
        type="button"
        data-player-footer-interactive="true"
        data-test="player-footer-artwork"
        class="footer-artwork h-12 w-12 shrink-0 overflow-hidden rounded-lg shadow-lg ring-1 ring-foreground/10 disabled:pointer-events-none disabled:opacity-40"
        :aria-label="t('player.fullscreen')"
        :title="t('player.fullscreen')"
        :disabled="!store.currentTrack"
        @click="store.setPlayerMode('fullscreen')"
        @contextmenu.prevent="openArtworkContextMenu"
      >
        <LazyImg v-if="store.artworkUrlSm" :src="store.artworkUrlSm" :alt="trackTitle"
          class="w-full h-full object-cover" />
        <div v-else class="w-full h-full bg-foreground/5 flex items-center justify-center" data-test="player-footer-artwork-placeholder">
          <Music class="w-5 h-5 text-dimmer" />
        </div>
      </button>
      <div class="flex flex-col min-w-0">
        <MarqueeText :text="trackTitle" fade content-class="font-medium text-sm leading-tight" />
        <MarqueeText :text="trackArtist" fade content-class="text-xs text-dim leading-tight mt-0.5" />
      </div>
      <PlayerFavoriteButton v-if="appStore.showPlayerFavoriteButton" class="-ml-1" />
      <button
        v-if="lastfmRadioStore.active && store.currentTrack"
        type="button"
        data-test="player-dislike"
        class="flex h-7 w-7 shrink-0 items-center justify-center rounded-full text-foreground opacity-40 transition-opacity duration-300 hover:bg-foreground/[0.04] hover:opacity-70"
        :aria-label="t('player.dislike_track')"
        :title="t('player.dislike_track')"
        @click="dislikeCurrent"
      >
        <ThumbsDown class="h-4 w-4" />
      </button>
    </div>

    <!-- Playback Controls -->
    <div class="flex-1 flex flex-col items-center gap-2 max-w-[600px]">
      <div class="flex items-center gap-5">
        <PlayerControlButton
          class="transition-opacity"
          :class="store.shuffle ? 'text-primary opacity-100' : 'text-dim hover:text-subdued'"
          :active="store.shuffle"
          :show-indicator="appStore.showPlayerIndicator"
          @click="store.setShuffle(!store.shuffle)"
          :title="t('player.shuffle')"
        >
          <Shuffle class="w-4 h-4" />
        </PlayerControlButton>
        <button class="text-subdued hover:text-foreground transition-colors" @click="store.previous()"
          :title="t('player.previous')">
          <SkipBack class="w-5 h-5 fill-current" />
        </button>
        <button
          class="w-8 h-8 bg-primary rounded-full flex items-center justify-center hover:scale-105 transition-transform"
          @click="store.togglePlayPause()" :title="store.isPlaying ? t('player.pause') : t('player.play')">
          <Pause v-if="store.isPlaying" class="w-4 h-4 fill-current text-primary-foreground" />
          <Play v-else class="w-4 h-4 fill-current text-primary-foreground ml-0.5" />
        </button>
        <button class="text-subdued hover:text-foreground transition-colors" @click="store.next()"
          :title="t('player.next')">
          <SkipForward class="w-5 h-5 fill-current" />
        </button>
        <PlayerControlButton
          class="transition-colors"
          :class="repeatActive ? 'text-primary' : 'text-dim hover:text-subdued'"
          :active="repeatActive"
          :show-indicator="appStore.showPlayerIndicator"
          @click="store.cycleRepeat()"
          :title="t('player.repeat')"
        >
          <component :is="repeatIcon" class="w-4 h-4" />
        </PlayerControlButton>
      </div>

      <!-- Seek bar, or the last playback failure with a retry action. A stream
           failure is retried by resolving the row again, never by switching
           source or downloading it. -->
      <div
        v-if="store.playError"
        class="w-full flex items-center justify-center gap-2 text-[11px] text-destructive"
        data-test="playback-error"
        role="alert"
      >
        <AlertCircle class="w-3.5 h-3.5 shrink-0" />
        <span class="truncate">{{ t('player.play_failed') }}</span>
        <button
          type="button"
          class="underline underline-offset-2 transition-opacity hover:opacity-80"
          data-test="playback-retry"
          @click="store.retryPlayback()"
        >
          {{ t('player.retry_playback') }}
        </button>
      </div>
      <div v-else class="w-full flex items-center gap-2">
        <span class="text-[10px] text-subdued tabular-nums w-8 text-right">
          {{ formatTime(displayPosition) }}
        </span>
        <Slider :model-value="isSeeking ? seekValue : store.progressPercent" :min="0" :max="100" :step="0.1"
          class="flex-1" @update:model-value="(v) => (seekValue = v)" @mousedown="onSeekStart" @mouseup="onSeekEnd"
          @touchstart="onSeekStart" @touchend="onSeekEnd" />
        <span class="text-[10px] text-subdued tabular-nums w-8">
          {{ formatTime(store.duration) }}
        </span>
      </div>
    </div>

    <!-- Volume & Options -->
    <div class="flex items-center justify-end gap-4 w-1/4 min-w-[200px]">
      <div data-player-footer-interactive="true" class="flex items-center gap-2 w-28">
        <button class="text-dim hover:text-subdued transition-colors flex-shrink-0"
          @click="store.setMuted(!store.muted)" :title="store.muted ? t('player.unmute') : t('player.mute')">
          <VolumeX v-if="store.muted" class="w-4 h-4" />
          <Volume2 v-else class="w-4 h-4" />
        </button>
        <Slider :model-value="store.muted ? 0 : store.volume" :min="0" :max="1" :step="0.01" :scrollable="true" class="flex-1"
          @update:model-value="(v) => store.setVolume(v)" />
      </div>
      <button class="transition-colors"
        :class="store.isLyricsOpen ? 'text-primary' : 'text-dim hover:text-subdued'"
        @click="store.toggleLyrics()" :title="t('player.lyrics')">
        <Mic2 class="w-4 h-4" />
      </button>
      <button class="transition-colors"
        :class="store.isQueueOpen ? 'text-primary' : 'text-dim hover:text-subdued'"
        @click="store.toggleQueue()" :title="t('player.queue')">
        <ListMusic class="w-4 h-4" />
      </button>
      <button class="text-dim hover:text-subdued transition-colors"
        @click="WindowService.ToggleMiniPlayer()" :title="t('player.mini_player')">
        <PictureInPicture2 class="w-4 h-4" />
      </button>
      <button
        type="button"
        class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-dim transition-colors hover:bg-foreground/[0.04] hover:text-foreground disabled:pointer-events-none disabled:opacity-40"
        data-test="player-more-info"
        :aria-label="t('player.more_info')"
        :title="t('player.more_info')"
        :disabled="!store.currentTrack"
        @click="store.openTrackInfo(store.currentTrack)"
      >
        <Info class="h-4 w-4" aria-hidden="true" />
      </button>
    </div>
  </div>

  <TrackContextMenu ref="trackContextMenu" />
  <PlayerQuickSettingsMenu ref="quickSettingsMenu" />
</template>

<style scoped>
.footer-artwork {
  scale: 1;
  transition: scale 0.3s cubic-bezier(0.4, 0, 0.2, 1);
  will-change: scale;
}

@media (hover: hover) {
  .footer-artwork:hover {
    scale: 1.05;
  }
}

.footer-artwork:active {
  scale: 0.95;
}
</style>
