<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { FileDown, Mic2, X } from '@lucide/vue'
import { usePlayerStore } from '../stores/player'
import { useAppStore, type LyricsSecondaryMode } from '../stores/app'
import { useRomanizationStore } from '../stores/romanization'
import { useI18n } from 'vue-i18n'
import { useLyrics } from '../composables/useLyrics'
import { useLyricTranslation } from '../composables/useLyricTranslation'
import { useRomanization } from '../composables/useRomanization'
import { useExportLyrics } from '../composables/useExportLyrics'
import { lyricsMotionClasses, useLyricsScrollMotion } from '../composables/useLyricsScrollMotion'
import KaraokeText from './KaraokeText.vue'

const { t } = useI18n()
const store = usePlayerStore()
const appStore = useAppStore()
const romanizationStore = useRomanizationStore()
const exportLyrics = useExportLyrics().open

const lyricsContent = computed(() => store.lyrics?.content)
const { isSynced, syncedLines, plainLines } = useLyrics(lyricsContent)
const mainLines = computed(() => isSynced.value ? syncedLines.value.map(line => line.text) : plainLines.value.map(line => line.primary))
const { supported, secondary: romanizationSecondary } = useRomanization(
  mainLines, computed(() => !store.lyricsLoading),
)

// A stored translation is aligned to the rendered lines by timestamp or line
// order. Older/local bilingual lyrics still carry their translation inside the
// line, so those parsed secondaries are the fallback when nothing is stored.
const lineTimes = computed(() => isSynced.value ? syncedLines.value.map(line => line.time) : [])
const lineCount = computed(() => isSynced.value ? syncedLines.value.length : plainLines.value.length)
const translatedLines = useLyricTranslation(
  computed(() => store.lyrics?.translation_content),
  lineCount,
  lineTimes,
)
const inlineSecondary = computed(() =>
  (isSynced.value ? syncedLines.value : plainLines.value).map(line => line.secondary),
)
const translationAvailable = computed(() =>
  translatedLines.value.some(Boolean) || inlineSecondary.value.some(Boolean),
)
const romanizationAvailable = computed(() => supported.value)

// The stored setting is the user's choice. A choice that this track cannot
// satisfy (no translation / unsupported script) renders no secondary line but
// is not overwritten, so it comes back for a track that can satisfy it.
const mode = computed<LyricsSecondaryMode>(() => {
  if (appStore.lyricsSecondaryMode === 'translation' && translationAvailable.value) return 'translation'
  if (appStore.lyricsSecondaryMode === 'romanization' && romanizationAvailable.value) return 'romanization'
  return 'none'
})
const secondaryLines = computed<(string | undefined)[]>(() => {
  if (mode.value === 'translation') {
    return translatedLines.value.some(Boolean) ? translatedLines.value : inlineSecondary.value
  }
  if (mode.value === 'romanization') return romanizationSecondary.value
  return []
})
const secondaryOptions = computed(() => [
  { id: 'none' as const, label: t('player.secondary_none'), disabled: false },
  { id: 'translation' as const, label: t('player.secondary_translation'), disabled: !translationAvailable.value },
  { id: 'romanization' as const, label: t('player.secondary_romanization'), disabled: !romanizationAvailable.value },
])
const showSecondaryControl = computed(() => translationAvailable.value || romanizationAvailable.value)

function selectSecondaryMode(next: LyricsSecondaryMode) {
  const option = secondaryOptions.value.find(item => item.id === next)
  if (!option || option.disabled) return
  void appStore.updateLyricsSecondaryMode(next)
}

function onSecondaryKeydown(event: KeyboardEvent) {
  if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
  event.preventDefault()
  const enabledOptions = secondaryOptions.value.filter(item => !item.disabled)
  if (enabledOptions.length === 0) return
  const current = enabledOptions.findIndex(item => item.id === mode.value)
  const delta = event.key === 'ArrowRight' ? 1 : -1
  const next = enabledOptions[(Math.max(current, 0) + delta + enabledOptions.length) % enabledOptions.length]
  selectSecondaryMode(next.id)
}

// Choosing romanization must also start the conversion; choosing anything else
// stops the background work for the current session.
watch(mode, (current) => {
  if (current === 'romanization' && !romanizationStore.enabled) void romanizationStore.setEnabled(true)
}, { immediate: true })

const activeIndex = computed(() => {
  const pos = store.position
  const idx = [...syncedLines.value].reverse().findIndex(line => line.time <= pos)
  return idx !== -1 ? syncedLines.value.length - 1 - idx : -1
})

const scrollContainer = ref<HTMLElement | null>(null)
const lineRefs = ref<HTMLElement[]>([])
const rootEl = ref<HTMLElement | null>(null)
const isBrowsing = ref(false)
let scrollFrame: number | undefined
let resizeObserver: ResizeObserver | null = null
let waitingForLayout = false
let hasPositionedInitialLine = false
let previousActiveIndex = -1
const { scrollTo, stop: stopScrollAnimation } = useLyricsScrollMotion()

// Reset stale refs when lines change so indexes stay aligned.
watch(() => syncedLines.value, () => {
  lineRefs.value = []
  isBrowsing.value = false
  hasPositionedInitialLine = false
  previousActiveIndex = -1
})

function isVisible(container: HTMLElement, el: HTMLElement) {
  return el.offsetTop < container.scrollTop + container.clientHeight
    && el.offsetTop + el.clientHeight > container.scrollTop
}

function scrollToActive(index: number, animated: boolean): boolean {
  if (index === -1) return false
  const container = scrollContainer.value
  const el = lineRefs.value[index]
  // The drawer animates in from zero width/height. Wait until it has a real
  // layout; otherwise offset measurements are invalid on first open.
  if (!container || !el || container.clientHeight === 0 || container.clientWidth === 0) return false
  scrollTo(container, el.offsetTop - container.clientHeight / 2 + el.clientHeight / 2, animated)
  hasPositionedInitialLine = true
  return true
}

function scheduleScrollToActive(index: number, previousIndex = previousActiveIndex) {
  stopScrollAnimation()
  nextTick(() => {
    if (scrollFrame !== undefined) cancelAnimationFrame(scrollFrame)
    scrollFrame = requestAnimationFrame(() => {
      scrollFrame = undefined
      const container = scrollContainer.value
      const previous = lineRefs.value[previousIndex]
      const animated = !!(hasPositionedInitialLine && container && previous && isVisible(container, previous))
      waitingForLayout = index !== -1 && !scrollToActive(index, animated)
    })
  })
}

function enterBrowseMode() {
  isBrowsing.value = true
}

function seekAndResume(time: number, index: number) {
  isBrowsing.value = false
  // The listener selected a visible line, so animate that exact line into the
  // active position before the player position update arrives.
  hasPositionedInitialLine = true
  previousActiveIndex = index
  scheduleScrollToActive(index, index)
  store.seek(time)
}

// flush:'post' → DOM patched before measuring offsets.
watch(activeIndex, (newIndex) => {
  const previousIndex = previousActiveIndex
  if (!isBrowsing.value) scheduleScrollToActive(newIndex, previousIndex)
  previousActiveIndex = newIndex
}, { flush: 'post', immediate: true })

// The desktop drawer is CSS-hidden instead of unmounted, so restore auto-scroll
// whenever it becomes visible again.
watch(() => store.isLyricsOpen, (isOpen, wasOpen) => {
  if (!isOpen || wasOpen) return
  isBrowsing.value = false
  hasPositionedInitialLine = false
  scheduleScrollToActive(activeIndex.value)
})

onMounted(() => {
  scheduleScrollToActive(activeIndex.value)
  if (typeof ResizeObserver !== 'undefined') {
    resizeObserver = new ResizeObserver(() => {
      if (waitingForLayout && !isBrowsing.value) scheduleScrollToActive(activeIndex.value)
    })
    if (scrollContainer.value) resizeObserver.observe(scrollContainer.value)
    else if (rootEl.value) resizeObserver.observe(rootEl.value)
  }
})

onUnmounted(() => {
  if (scrollFrame !== undefined) cancelAnimationFrame(scrollFrame)
  stopScrollAnimation()
  resizeObserver?.disconnect()
  resizeObserver = null
})
</script>

<template>
  <div ref="rootEl" class="h-full w-full bg-background flex flex-col">
    <!-- Header -->
    <div class="flex items-center justify-between px-4 py-3 border-b border-foreground/[0.06] select-none">
      <div class="flex items-center gap-2 font-semibold flex-shrink-0">
        <Mic2 class="w-4 h-4 text-primary" />
        <div class="max-w-[100px] truncate">{{ t('player.lyrics') }}</div>
      </div>

      <div class="flex items-center gap-1 flex-shrink-0">
        <div v-if="showSecondaryControl" data-test="drawer-secondary-modes" role="radiogroup"
          :aria-label="t('player.lyrics_secondary')"
          class="flex items-center rounded-full bg-foreground/[0.06] p-0.5 text-[11px]"
          @keydown="onSecondaryKeydown">
          <button v-for="option in secondaryOptions" :key="option.id" type="button" role="radio"
            :data-test="`drawer-secondary-${option.id}`"
            :aria-checked="mode === option.id" :disabled="option.disabled"
            :tabindex="mode === option.id ? 0 : -1"
            class="rounded-full px-2 py-1 font-medium transition-colors disabled:opacity-40"
            :class="mode === option.id ? 'bg-foreground/[0.12] text-foreground' : 'text-dim hover:text-foreground'"
            @click="selectSecondaryMode(option.id)">
            {{ option.label }}
          </button>
        </div>
        <button v-if="lyricsContent && store.currentTrack?.path && !store.currentTrack.path.startsWith('remote://')" type="button" data-test="drawer-export-lyrics"
          class="p-1.5 rounded-full hover:bg-foreground/8 transition-colors text-dim hover:text-foreground"
          :title="t('export_lyrics.title')" :aria-label="t('export_lyrics.title')"
          @click="exportLyrics(store.currentTrack, isSynced)">
          <FileDown class="w-4 h-4" />
        </button>
        <button
          class="p-1.5 rounded-full hover:bg-foreground/8 transition-colors text-dim hover:text-foreground"
          @click="store.toggleLyrics()">
          <X class="w-4 h-4" />
        </button>
      </div>
    </div>

    <!-- Body -->
    <div class="flex-1 overflow-hidden">
      <!-- Loading skeleton -->
      <div v-if="store.lyricsLoading" class="px-4 py-8 space-y-4">
        <div v-for="(width, i) in ['w-3/4', 'w-1/2', 'w-5/6', 'w-2/3', 'w-1/3', 'w-4/5', 'w-1/2', 'w-2/3']" :key="i"
          class="h-4 rounded bg-foreground/[0.06] animate-pulse" :class="width" />
      </div>

      <!-- No lyrics -->
      <div v-else-if="!lyricsContent"
        class="h-full flex flex-col items-center justify-center text-muted-foreground gap-3 px-6 text-center">
        <Mic2 class="w-10 h-10 opacity-20" />
        <p class="text-sm">{{ t('player.lyrics_not_available') }}</p>
      </div>

      <!-- Synced view -->
      <div v-else-if="isSynced" ref="scrollContainer" class="h-full overflow-y-auto px-4 py-6 scrollbar-hide"
        @wheel.passive="enterBrowseMode" @pointerdown="enterBrowseMode">
        <div class="space-y-6">
          <div v-for="(line, index) in syncedLines" :key="index" ref="lineRefs"
            class="cursor-pointer opacity-100 select-none leading-snug py-1 origin-left"
            :class="[
              lyricsMotionClasses,
              isBrowsing
                ? 'text-foreground opacity-100'
                : index === activeIndex
                  ? 'text-foreground opacity-100'
                  : index < activeIndex
                    ? 'text-foreground/40 opacity-60 hover:text-foreground/50'
                    : 'text-foreground/40 opacity-50 hover:text-foreground/50',

              !isBrowsing && (index === activeIndex || (activeIndex > 0 && index === activeIndex - 1) || (index === activeIndex + 1)) ? 'transform-gpu' : ''
            ]"
            :style="line.words && store.isLyricsOpen && !isBrowsing && index === activeIndex ? { transition: 'none' } : undefined"
            @pointerdown.stop @click="seekAndResume(line.time, index)">
            <div data-test="lyric-primary" class="text-[19pt] font-bold"><KaraokeText :line="line" :position="store.isLyricsOpen && !isBrowsing && index === activeIndex ? store.position : undefined" /></div>
            <div v-if="secondaryLines[index]" data-test="lyric-secondary" class="text-[14pt] opacity-50 mt-0.5">{{ secondaryLines[index] }}</div>
          </div>
        </div>
      </div>

      <!-- Plain view: no timestamps, so nothing can follow playback. Say so
           instead of letting the lines look like a synced lyric that is stuck. -->
      <div v-else class="h-full overflow-y-auto px-4 py-6 scrollbar-hide">
        <p class="mb-4 text-xs text-dimmer" data-test="lyrics-unsynced">
          {{ t('player.lyrics_unsynced') }}
        </p>
        <div class="space-y-3.5">
          <div v-for="(line, index) in plainLines" :key="index" class="leading-relaxed select-text">
            <p data-test="lyric-primary" class="text-sm text-foreground/80">{{ line.primary }}</p>
            <p v-if="secondaryLines[index]" data-test="lyric-secondary" class="text-xs text-foreground/40 mt-0.5">{{ secondaryLines[index] }}</p>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped></style>
