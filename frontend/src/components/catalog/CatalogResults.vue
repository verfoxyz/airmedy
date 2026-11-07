<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertTriangle, Check, Heart, ListPlus, Loader2, Music, Download } from '@lucide/vue'
import ContextMenu from '@/components/ContextMenu.vue'
import { usePlaylistsStore } from '@/stores/playlists'
import { useContextMenu } from '@/composables/useContextMenu'
import type { ContextMenuItem } from '@/composables/useContextMenu'
import * as CatalogService from '../../../bindings/airmedy/internal/infra/wails/catalogservice'
import type {
  CatalogCoverRequestDTO,
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
  /** Selected source ids; an empty list means "全部". */
  activeSources: string[]
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
  (e: 'toggle-source', id: string): void
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
// The host downloads and caches remote covers; the browser renders only a
// host-local asset URL. A row asks for its cover only when it enters the viewport
// (200px early), so a long page never fetches images the user did not see. The
// image queue keeps at most MAX_CONCURRENT_COVERS preloads in flight, and a row
// renders only after its preload finished.

const MAX_CONCURRENT_COVERS = 4
const COVER_BATCH_LIMIT = 10

const rootEl = ref<HTMLElement | null>(null)
/** The list element whose rows are observed for lazy covers. */
const rowsEl = ref<HTMLElement | null>(null)
/** Covers that finished preloading and may be rendered. */
const loadedCovers = reactive(new Set<string>())
/** Covers that failed (or errored after being rendered), so the row keeps the placeholder. */
const failedCovers = reactive(new Set<string>())
/** Covers already requested or loaded, so repeated visibility does not re-request them. */
const requestedCovers = reactive(new Set<string>())
/** Identity -> host-local asset URL returned by GetCatalogCovers. */
const coverURLs = reactive(new Map<string, string>())

interface CoverTask {
  key: string
  url: string
}

let coverQueue: CoverTask[] = []
let coverInFlight = 0
const inFlightImages = new Set<HTMLImageElement>()
let coverObserver: IntersectionObserver | null = null
let observedRows = new Map<Element, CatalogRowDTO>()
let pendingRows: CatalogRowDTO[] = []
let flushScheduled = false
let coverRpcInFlight = false
let lastFirstKey = ''
let disposed = false

function coverKey(row: CatalogRowDTO): string {
  return `${row.provider}/${row.external_id}`
}

/** coverLoaded gates the <img>; until preloading finishes the placeholder stays. */
function coverLoaded(row: CatalogRowDTO): boolean {
  // `cover_url` is the source's "this row has artwork" flag; a local URL without
  // it belongs to a page fetched before the user hid result covers.
  return !!row.cover_url && !!coverURLs.get(coverKey(row)) && loadedCovers.has(coverKey(row))
}

function coverSrc(row: CatalogRowDTO): string {
  return coverURLs.get(coverKey(row)) ?? ''
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

/** findScrollParent returns the list's scroller; the viewport would ignore the
 *  clipping by the overflow container. */
function findScrollParent(el: HTMLElement | null): Element | null {
  let node = el?.parentElement ?? null
  while (node) {
    const overflowY = window.getComputedStyle(node).overflowY
    if (overflowY === 'auto' || overflowY === 'scroll' || overflowY === 'overlay') return node
    node = node.parentElement
  }
  return null
}

/** onCoverVisible queues the rows that just entered the viewport. */
function onCoverVisible(entries: IntersectionObserverEntry[]) {
  for (const entry of entries) {
    if (!entry.isIntersecting) continue
    const row = observedRows.get(entry.target)
    if (!row) continue
    coverObserver?.unobserve(entry.target)
    observedRows.delete(entry.target)
    if (!row.cover_url || !row.external_id) continue

    const key = coverKey(row)
    if (requestedCovers.has(key) || failedCovers.has(key) || loadedCovers.has(key)) continue

    requestedCovers.add(key)
    pendingRows.push(row)
  }
  if (pendingRows.length > 0) scheduleFlush()
}

/** observeCoverRows re-arms every row that still needs its artwork. */
function observeCoverRows() {
  coverObserver?.disconnect()
  coverObserver = null
  observedRows = new Map()
  if (disposed) return

  if (typeof IntersectionObserver === 'undefined') return
  const root = findScrollParent(rootEl.value)
  coverObserver = new IntersectionObserver(onCoverVisible, {
    root,
    rootMargin: '0px 0px 200px 0px',
    threshold: 0,
  })

  const elements = rowsEl.value?.querySelectorAll<HTMLElement>('[data-row-index]') ?? []
  for (const element of elements) {
    const index = Number(element.dataset.rowIndex)
    const row = props.rows[index]
    if (!row || !row.cover_url || !row.external_id) continue
    const key = coverKey(row)
    if (requestedCovers.has(key) || failedCovers.has(key) || loadedCovers.has(key)) continue
    observedRows.set(element, row)
    coverObserver.observe(element)
  }
}

/** scheduleFlush coalesces the rows visible in one turn into one RPC. */
function scheduleFlush() {
  if (flushScheduled || coverRpcInFlight || disposed) return
  flushScheduled = true
  queueMicrotask(() => {
    flushScheduled = false
    void flushCovers()
  })
}

/** flushCovers asks the host for at most COVER_BATCH_LIMIT queued rows. */
async function flushCovers() {
  if (disposed || coverRpcInFlight) return
  const rows = pendingRows.splice(0, COVER_BATCH_LIMIT)
  if (rows.length === 0) return

  const requests: CatalogCoverRequestDTO[] = rows.map((row) => ({
    provider: row.provider,
    external_id: row.external_id,
    url: row.cover_url_large || row.cover_url,
  }))

  coverRpcInFlight = true
  try {
    const answers = ((await CatalogService.GetCatalogCovers(requests)) ?? []) as Array<{
      provider?: string
      external_id?: string
      url?: string
      error?: string
    }>
    if (disposed) return

    for (const answer of answers) {
      if (!answer || !answer.provider || !answer.external_id) continue
      const key = `${answer.provider}/${answer.external_id}`
      if (!answer.url || answer.error) {
        failedCovers.add(key)
        continue
      }
      coverURLs.set(key, answer.url)
      coverQueue.push({ key, url: answer.url })
    }
    pumpCovers()
  } catch (error) {
    // The call itself failed: keep placeholders and do not retry this page.
    for (const request of requests) {
      failedCovers.add(`${request.provider}/${request.external_id}`)
    }
    console.error('Catalog covers failed', error)
  } finally {
    coverRpcInFlight = false
    if (!disposed && pendingRows.length > 0) scheduleFlush()
  }
}

/** resetCoverState drops everything tied to the previous result page. */
function resetCoverState() {
  loadedCovers.clear()
  failedCovers.clear()
  requestedCovers.clear()
  coverURLs.clear()
  coverQueue = []
  pendingRows = []
  observedRows = new Map()
}

const isAllSources = computed(() => props.activeSources.length === 0)
const showList = computed(() => props.hasQuery && !props.loading && props.rows.length > 0)
const showEmpty = computed(
  () => props.hasQuery && !props.loading && props.errorKind === '' && props.rows.length === 0,
)

// A changed first row or a shorter list means a new search page; an append
// re-arms only the rows that still lack artwork.
watch(
  () => props.rows,
  (next, previous) => {
    void nextTick(() => {
      if (disposed) return
      const firstKey = next.length > 0 ? coverKey(next[0]) : ''
      if (firstKey !== lastFirstKey || next.length < (previous?.length ?? 0)) {
        resetCoverState()
        lastFirstKey = firstKey
      }
      observeCoverRows()
    })
  },
)

// Rows live inside `showList`; observe them whenever that block appears.
watch(showList, () => {
  void nextTick(observeCoverRows)
})

onMounted(() => {
  lastFirstKey = props.rows.length > 0 ? coverKey(props.rows[0]) : ''
  void nextTick(observeCoverRows)
})

onUnmounted(() => {
  disposed = true
  coverObserver?.disconnect()
  coverObserver = null
  observedRows = new Map()
  pendingRows = []
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

/** subtitle lists the artists; source names are rendered as separate badges. */
function subtitle(row: CatalogRowDTO): string {
  return (row.artists ?? []).filter(Boolean).join(' · ')
}

/** rowSources falls back to the row's provider for a pre-merge payload. */
function rowSources(row: CatalogRowDTO): string[] {
  return row.sources && row.sources.length > 0 ? row.sources : [row.provider]
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
        @click="emit('toggle-source', '')"
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
        :class="activeSources.includes(source.id)
          ? 'bg-foreground/[0.10] text-foreground'
          : 'text-subdued hover:bg-foreground/[0.04] hover:text-foreground'"
        @click="emit('toggle-source', source.id)"
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
    <div v-if="showList" ref="rowsEl" class="space-y-1">
      <p class="pb-2 text-xs font-semibold uppercase tracking-wide text-dimmer">
        {{ $t('cloud_search.results') }}
      </p>
      <div
        v-for="(row, index) in rows"
        :key="`${row.provider}/${row.external_id}`"
        data-test="catalog-row"
        :data-row-index="index"
        :data-identity="`${row.provider}/${row.external_id}`"
        class="group flex items-center gap-3 rounded-xl p-2 transition-colors hover:bg-foreground/[0.04]"
      >
        <div
          class="flex h-10 w-10 flex-shrink-0 items-center justify-center overflow-hidden rounded-lg bg-foreground/[0.06] ring-1 ring-foreground/[0.06]"
        >
          <img
            v-if="coverLoaded(row)"
            :src="coverSrc(row)"
            :alt="row.title"
            data-test="catalog-cover"
            class="catalog-cover-fade h-full w-full object-cover"
            @error="onCoverError(row)"
          />
          <Music v-else class="h-5 w-5 text-dimmer" data-test="catalog-cover-placeholder" />
        </div>

        <div class="min-w-0 flex-1">
          <p class="truncate text-sm font-semibold text-foreground">{{ row.title }}</p>
          <div class="flex min-w-0 items-center gap-1.5">
            <p class="truncate text-xs text-dim">{{ subtitle(row) }}</p>
            <span
              v-for="source in rowSources(row)"
              :key="source"
              data-test="catalog-row-source"
              class="shrink-0 rounded-full bg-foreground/[0.06] px-1.5 py-0.5 text-[10px] font-medium text-dimmer"
            >{{ sourceName(source) }}</span>
          </div>
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