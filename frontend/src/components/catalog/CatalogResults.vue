<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertTriangle, Check, Heart, ListPlus, Loader2, Music, Download } from '@lucide/vue'
import ContextMenu from '@/components/ContextMenu.vue'
import { usePlaylistsStore } from '@/stores/playlists'
import { useContextMenu } from '@/composables/useContextMenu'
import type { ContextMenuItem } from '@/composables/useContextMenu'
import type {
  CatalogFailureDTO,
  CatalogRowDTO,
  CatalogSourceDTO,
} from '../../../bindings/airmedy/internal/infra/wails/models'

/**
 * The cloud branch of the search page.
 *
 * Presentational: it renders the merged page the store assembled and emits what
 * the user asked for. Loading, searching and saving live in the store, so this
 * component is fully testable by props.
 */
const props = defineProps<{
  sources: CatalogSourceDTO[]
  activeSource: string
  rows: CatalogRowDTO[]
  failures: CatalogFailureDTO[]
  hasMore: boolean
  loading: boolean
  loadingMore: boolean
  errorKind: string
  actionError: string
  hasQuery: boolean
}>()

const emit = defineEmits<{
  (e: 'update:source', id: string): void
  (e: 'load-more'): void
  (e: 'favorite', row: CatalogRowDTO): void
  (e: 'add-to-playlist', row: CatalogRowDTO, playlistId: string): void
  (e: 'force-download', row: CatalogRowDTO): void
}>()

const { t } = useI18n()
const playlistsStore = usePlaylistsStore()
const playlistMenu = useContextMenu()

// --- Cover thumbnails -------------------------------------------------------
//
// Native `loading="lazy"` is only a hint: Chromium starts a whole screen ahead,
// so a 50-row page fires 20–30 requests at once and the slow ones compete with
// the text. Covers are therefore loaded deterministically: a visible row asks
// the queue for its image (IntersectionObserver, 200px pre-fetch margin), and at
// most MAX_CONCURRENT_COVERS images are in flight. An image is rendered only
// after it finished preloading, so a row never flashes a half-drawn picture.

const MAX_CONCURRENT_COVERS = 4
const COVER_ROOT_MARGIN = '200px 0px'

const rootEl = ref<HTMLElement | null>(null)
/** Covers that finished preloading and may be rendered. */
const loadedCovers = reactive(new Set<string>())
/** Covers that failed (or errored after being rendered), so the row keeps the placeholder. */
const failedCovers = reactive(new Set<string>())
/** Covers already queued or loaded, so scrolling back does not re-request them. */
const requestedCovers = reactive(new Set<string>())

interface CoverTask {
  key: string
  url: string
}

/** Placeholder element of each row, keyed by `coverKey`, observed for visibility. */
const coverElements = new Map<string, Element>()
/** Element → request, populated when (and only when) it is observed. */
const observedCovers = new Map<Element, CoverTask>()
let coverQueue: CoverTask[] = []
let coverInFlight = 0
const inFlightImages = new Set<HTMLImageElement>()
let coverObserver: IntersectionObserver | null = null
let disposed = false

function coverKey(row: CatalogRowDTO): string {
  return `${row.provider}/${row.external_id}`
}

/** coverLoaded gates the <img>; until preloading finishes the placeholder stays. */
function coverLoaded(row: CatalogRowDTO): boolean {
  return !!row.cover_url && loadedCovers.has(coverKey(row))
}

function onCoverError(row: CatalogRowDTO) {
  // The browser cache can drop a preloaded image; fall back to the placeholder.
  const key = coverKey(row)
  loadedCovers.delete(key)
  failedCovers.add(key)
}

function pumpCovers() {
  while (!disposed && coverInFlight < MAX_CONCURRENT_COVERS && coverQueue.length > 0) {
    const task = coverQueue.shift()!
    if (loadedCovers.has(task.key) || failedCovers.has(task.key)) continue

    coverInFlight++
    const image = new Image()
    inFlightImages.add(image)
    let settled = false

    const finish = (ok: boolean) => {
      if (settled) return
      settled = true
      image.onload = null
      image.onerror = null
      inFlightImages.delete(image)
      coverInFlight--
      if (!disposed) {
        if (ok) loadedCovers.add(task.key)
        else failedCovers.add(task.key)
        pumpCovers()
      }
    }

    image.onload = () => finish(true)
    image.onerror = () => finish(false)
    image.src = task.url
  }
}

function requestCover(key: string, url: string) {
  if (disposed || !url || requestedCovers.has(key) || failedCovers.has(key)) return
  requestedCovers.add(key)
  coverQueue.push({ key, url })
  pumpCovers()
}

/** setCoverRef collects the placeholder element of each row for the observer. */
function setCoverRef(key: string, el: unknown) {
  const node = el as Element | null
  if (node && node.nodeType === 1) coverElements.set(key, node)
  else coverElements.delete(key)
}

/** findScrollParent returns the list's scroller; observing the viewport would
 *  ignore the clipping by the overflow container. */
function findScrollParent(el: HTMLElement | null): Element | null {
  let node = el?.parentElement ?? null
  while (node) {
    const overflowY = window.getComputedStyle(node).overflowY
    if (overflowY === 'auto' || overflowY === 'scroll' || overflowY === 'overlay') return node
    node = node.parentElement
  }
  return null
}

function onCoverVisible(entries: IntersectionObserverEntry[]) {
  for (const entry of entries) {
    if (!entry.isIntersecting) continue
    const task = observedCovers.get(entry.target)
    if (!task) continue
    coverObserver?.unobserve(entry.target)
    observedCovers.delete(entry.target)
    requestCover(task.key, task.url)
  }
}

/** rebuildCoverObserver re-arms every row the current page added; loaded and
 *  failed rows are skipped so an append never re-requests them. */
function rebuildCoverObserver() {
  coverObserver?.disconnect()
  observedCovers.clear()
  if (!coverObserver || disposed) return

  for (const [key, element] of coverElements) {
    const row = rowsByKey.value.get(key)
    if (!row?.cover_url || requestedCovers.has(key) || failedCovers.has(key)) continue
    observedCovers.set(element, { key, url: row.cover_url })
    coverObserver.observe(element)
  }
}

const rowsByKey = computed(() => {
  const map = new Map<string, CatalogRowDTO>()
  for (const row of props.rows) map.set(coverKey(row), row)
  return map
})

const isAllSources = computed(() => props.activeSource === '')
const showList = computed(() => props.hasQuery && !props.loading && props.rows.length > 0)
const showEmpty = computed(
  () => props.hasQuery && !props.loading && props.errorKind === '' && props.rows.length === 0,
)

// Re-arm the observer after every page append or source switch; refs are
// refreshed by the DOM update before this post-flush callback runs.
watch([() => props.rows, showList], () => {
  void nextTick(rebuildCoverObserver)
})

onMounted(() => {
  if (typeof IntersectionObserver === 'undefined') return
  coverObserver = new IntersectionObserver(onCoverVisible, {
    root: findScrollParent(rootEl.value),
    rootMargin: COVER_ROOT_MARGIN,
  })
  void nextTick(rebuildCoverObserver)
})

onUnmounted(() => {
  disposed = true
  coverObserver?.disconnect()
  coverObserver = null
  coverQueue = []
  for (const image of inFlightImages) {
    image.onload = null
    image.onerror = null
  }
  inFlightImages.clear()
  coverInFlight = 0
})

/** sourceName falls back to the id, so a plugin without a label still reads. */
function sourceName(id: string): string {
  const source = props.sources.find((s) => s.id === id)
  return source?.name || id
}

function failureText(failure: CatalogFailureDTO): string {
  switch (failure.kind) {
    case 'timeout':
      return t('cloud_search.failure_timeout', { source: sourceName(failure.provider) })
    case 'source_disabled':
      return t('cloud_search.failure_source_disabled', { source: sourceName(failure.provider) })
    default:
      return t('cloud_search.failure_upstream', { source: sourceName(failure.provider) })
  }
}

function subtitle(row: CatalogRowDTO): string {
  return [...(row.artists ?? []), sourceName(row.provider)].filter(Boolean).join(' · ')
}

/** stateKey maps a backend row state onto a chip, or null for "nothing yet". */
function stateKey(row: CatalogRowDTO): 'in_library' | 'saved' | 'downloading' | 'failed' | null {
  if (row.state === 'in_library') return 'in_library'
  if (row.state === 'saved') return 'saved'
  if (row.state === 'downloading') return 'downloading'
  if (row.state === 'failed') return 'failed'
  return null
}

function blockedReason(row: CatalogRowDTO): string {
  if (row.blocked_reason === 'no_resolver') return t('cloud_search.blocked_no_resolver')
  return t('cloud_search.not_downloadable')
}

/** canAct is false when the source cannot be fetched or the row is already owned. */
function canAct(row: CatalogRowDTO): boolean {
  // An imported file is "in library" through local_track_id but the user can
  // still favorite it or add it to a playlist (the backend reuses that row).
  return row.downloadable && (row.state !== 'in_library' || !!row.local_track_id)
}

function openPlaylistMenu(event: MouseEvent, row: CatalogRowDTO) {
  event.preventDefault()
  event.stopPropagation()
  const items: ContextMenuItem[] = playlistsStore.manualPlaylists.map((playlist) => ({
    label: playlist.name || playlist.id,
    action: () => emit('add-to-playlist', row, playlist.id ?? ''),
  }))
  if (items.length === 0) {
    items.push({ label: t('cloud_search.no_playlists'), disabled: true })
  }
  playlistMenu.open(event, items)
}

defineExpose({
  failureText,
  subtitle,
  canAct,
  coverQueueSize: () => coverQueue.length,
  coverInFlightCount: () => coverInFlight,
})
</script>

<template>
  <div ref="rootEl" class="space-y-6">
    <!-- Source bar: 全部 + one button per source the plugin reported. -->
    <div v-if="sources.length > 0" class="flex flex-wrap items-center gap-2">
      <button
        type="button"
        data-test="catalog-source"
        data-source-id=""
        class="px-3 py-1.5 rounded-full text-xs font-medium transition-colors"
        :class="isAllSources
          ? 'bg-foreground/[0.10] text-foreground'
          : 'text-subdued hover:bg-foreground/[0.04] hover:text-foreground'"
        @click="emit('update:source', '')"
      >
        {{ $t('cloud_search.all_sources') }}
      </button>
      <button
        v-for="source in sources"
        :key="source.id"
        type="button"
        data-test="catalog-source"
        :data-source-id="source.id"
        class="px-3 py-1.5 rounded-full text-xs font-medium transition-colors"
        :class="activeSource === source.id
          ? 'bg-foreground/[0.10] text-foreground'
          : 'text-subdued hover:bg-foreground/[0.04] hover:text-foreground'"
        @click="emit('update:source', source.id)"
      >
        {{ source.name || source.id }}
      </button>
    </div>

    <!-- Hint: a cloud row is not a preview. -->
    <p v-if="hasQuery && !loading && rows.length === 0 && errorKind === ''" class="text-xs text-dimmer">
      {{ $t('cloud_search.hint') }}
    </p>

    <!-- Page-level states. Each is a machine kind, worded here. -->
    <div v-if="errorKind === 'no_sources'" class="rounded-xl bg-foreground/[0.03] p-6 ring-1 ring-foreground/[0.05]">
      <p class="text-sm font-semibold text-foreground">{{ $t('cloud_search.no_sources_title') }}</p>
      <p class="mt-1 text-xs text-dim">{{ $t('cloud_search.no_sources_description') }}</p>
    </div>
    <div
      v-else-if="errorKind === 'source_unavailable'"
      class="rounded-xl bg-foreground/[0.03] p-6 ring-1 ring-foreground/[0.05]"
    >
      <p class="text-sm font-semibold text-foreground">{{ $t('cloud_search.source_unavailable_title') }}</p>
      <p class="mt-1 text-xs text-dim">{{ $t('cloud_search.source_unavailable_description') }}</p>
    </div>
    <div
      v-else-if="errorKind === 'unavailable'"
      class="rounded-xl bg-foreground/[0.03] p-6 ring-1 ring-foreground/[0.05]"
    >
      <p class="text-sm font-semibold text-foreground">{{ $t('cloud_search.all_sources_failed') }}</p>
    </div>

    <!-- Loading skeleton -->
    <div v-if="loading" class="space-y-2">
      <div v-for="i in 5" :key="i" class="flex items-center gap-3 p-2 animate-pulse">
        <div class="w-12 h-12 rounded-lg bg-foreground/[0.06]" />
        <div class="flex-1 space-y-2">
          <div class="h-3 w-1/3 rounded bg-foreground/[0.06]" />
          <div class="h-3 w-1/4 rounded bg-foreground/[0.04]" />
        </div>
      </div>
    </div>

    <!-- One dead source is stated inline, without hiding the rows that worked. -->
    <div v-if="failures.length > 0 && !loading" class="flex flex-wrap gap-2">
      <span
        v-for="failure in failures"
        :key="failure.provider"
        class="inline-flex items-center gap-1.5 rounded-full bg-foreground/[0.04] px-3 py-1 text-xs text-dim"
      >
        <AlertTriangle class="h-3.5 w-3.5" />
        {{ failureText(failure) }}
      </span>
    </div>

    <!-- Action failure (a save that did not start). -->
    <p v-if="actionError" class="text-xs text-dim">{{ $t(`cloud_search.${actionError}`) }}</p>

    <!-- Results -->
    <div v-if="showList" class="space-y-1">
      <p class="pb-2 text-xs font-semibold uppercase tracking-wide text-dimmer">
        {{ $t('cloud_search.results') }}
      </p>
      <div
        v-for="row in rows"
        :key="`${row.provider}/${row.external_id}`"
        data-test="catalog-row"
        :data-identity="`${row.provider}/${row.external_id}`"
        class="group flex items-center gap-3 rounded-xl p-2 transition-colors hover:bg-foreground/[0.04]"
      >
        <div
          :ref="(el) => setCoverRef(coverKey(row), el)"
          class="flex h-10 w-10 flex-shrink-0 items-center justify-center overflow-hidden rounded-lg bg-foreground/[0.06] ring-1 ring-foreground/[0.06]"
        >
          <img
            v-if="coverLoaded(row)"
            :src="row.cover_url"
            :alt="row.title"
            data-test="catalog-cover"
            class="catalog-cover-fade h-full w-full object-cover"
            @error="onCoverError(row)"
          />
          <Music v-else class="h-5 w-5 text-dimmer" data-test="catalog-cover-placeholder" />
        </div>

        <div class="min-w-0 flex-1">
          <p class="truncate text-sm font-semibold text-foreground">{{ row.title }}</p>
          <p class="truncate text-xs text-dim">{{ subtitle(row) }}</p>
        </div>

        <span v-if="row.duration" class="text-xs tabular-nums text-dimmer">
          {{ Math.floor(row.duration / 60) }}:{{ String(row.duration % 60).padStart(2, '0') }}
        </span>

        <!-- Row state: in library / downloading / failed. -->
        <span
          v-if="stateKey(row) === 'in_library'"
          data-test="catalog-state"
          class="inline-flex items-center gap-1 rounded-full bg-foreground/[0.06] px-2.5 py-1 text-xs text-dim"
        >
          <Check class="h-3.5 w-3.5" />
          {{ $t('cloud_search.in_library') }}
        </span>
        <span
          v-else-if="stateKey(row) === 'saved'"
          data-test="catalog-state"
          class="inline-flex items-center gap-1 rounded-full bg-foreground/[0.06] px-2.5 py-1 text-xs text-dim"
        >
          <Check class="h-3.5 w-3.5" />
          {{ $t('cloud_search.saved') }}
        </span>
        <span
          v-else-if="stateKey(row) === 'downloading'"
          data-test="catalog-state"
          class="inline-flex items-center gap-1 rounded-full bg-foreground/[0.06] px-2.5 py-1 text-xs text-dim"
        >
          <Loader2 class="h-3.5 w-3.5 animate-spin" />
          {{ $t('cloud_search.downloading') }}
        </span>
        <span
          v-else-if="stateKey(row) === 'failed'"
          data-test="catalog-state"
          class="inline-flex items-center gap-1 rounded-full bg-foreground/[0.06] px-2.5 py-1 text-xs text-dim"
        >
          <AlertTriangle class="h-3.5 w-3.5" />
          {{ $t('cloud_search.failed') }}
        </span>
        <span v-else-if="!row.downloadable" class="text-xs text-dimmer">
          {{ blockedReason(row) }}
        </span>

        <!-- Actions appear on hover, like the rest of the app's list rows. -->
        <div
          v-if="canAct(row)"
          class="flex items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100"
        >
          <button
            type="button"
            class="flex h-8 w-8 items-center justify-center rounded-full text-foreground/60 transition-colors hover:bg-foreground/[0.06] hover:text-foreground/80"
            data-test="catalog-favorite"
            :aria-label="$t('cloud_search.favorite')"
            :title="$t('cloud_search.favorite')"
            @click.stop="emit('favorite', row)"
          >
            <Heart class="h-4 w-4" />
          </button>
          <button
            type="button"
            class="flex h-8 w-8 items-center justify-center rounded-full text-foreground/60 transition-colors hover:bg-foreground/[0.06] hover:text-foreground/80"
            data-test="catalog-playlist"
            :aria-label="$t('cloud_search.add_to_playlist')"
            :title="$t('cloud_search.add_to_playlist')"
            @click.stop="openPlaylistMenu($event, row)"
          >
            <ListPlus class="h-4 w-4" />
          </button>
          <!-- Explicit "download it anyway": the row is already in the library
               through an imported file, but the user may want the provider copy. -->
          <button
            v-if="row.local_track_id"
            type="button"
            class="flex h-8 w-8 items-center justify-center rounded-full text-foreground/60 transition-colors hover:bg-foreground/[0.06] hover:text-foreground/80"
            data-test="catalog-download-anyway"
            :aria-label="$t('cloud_search.download_anyway')"
            :title="$t('cloud_search.download_anyway')"
            @click.stop="emit('force-download', row)"
          >
            <Download class="h-4 w-4" />
          </button>
        </div>
      </div>

      <div v-if="hasMore" class="pt-4">
        <button
          type="button"
          data-test="catalog-load-more"
          class="rounded-full bg-foreground/[0.06] px-4 py-2 text-xs font-medium text-foreground transition-colors hover:bg-foreground/[0.10] disabled:opacity-50"
          :disabled="loadingMore"
          @click="emit('load-more')"
        >
          {{ loadingMore ? $t('cloud_search.loading_more') : $t('cloud_search.load_more') }}
        </button>
      </div>
    </div>

    <!-- Empty -->
    <div v-else-if="showEmpty" class="flex h-64 flex-col items-center justify-center text-center">
      <div class="mb-6 flex h-20 w-20 items-center justify-center rounded-3xl bg-foreground/[0.03] ring-1 ring-foreground/[0.05]">
        <Music class="h-10 w-10 text-foreground opacity-30" />
      </div>
      <p class="text-xl font-semibold text-dim">{{ $t('cloud_search.empty_title') }}</p>
      <p class="mt-2 text-dimmer">{{ $t('cloud_search.empty_description') }}</p>
    </div>

    <ContextMenu
      :visible="playlistMenu.visible.value"
      :x="playlistMenu.x.value"
      :y="playlistMenu.y.value"
      :items="playlistMenu.items.value"
      @close="playlistMenu.close()"
    />
  </div>
</template>

<style scoped>
/* Preloaded images fade in; the placeholder never animates, so scrolling does
   not flicker. Fixed 150ms keeps the row height stable. */
.catalog-cover-fade {
  animation: catalog-cover-fade-in 150ms ease-out both;
}

@keyframes catalog-cover-fade-in {
  from {
    opacity: 0;
  }
  to {
    opacity: 1;
  }
}
</style>