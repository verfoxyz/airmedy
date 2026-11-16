<script setup lang="ts">
import { computed, nextTick, onActivated, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { Events } from '@wailsio/runtime'
import { ArrowLeft, ChevronLeft, ChevronRight, MoreVertical, Music, Play, RefreshCw, Shuffle, Clock, Search } from '@lucide/vue'
import { DetailsButton, IconButton, Input } from '@airmedy/ui'
import ContextMenu from '@/components/ContextMenu.vue'
import DetailPageLayout from '@/components/DetailPageLayout.vue'
import LazyImg from '@/components/LazyImg.vue'
import TopTrackRow from '@/components/lastfm/TopTrackRow.vue'
import { useContextMenu } from '@/composables/useContextMenu'
import { foldUnicode } from '@airmedy/utils'
import {
  formatLastFmDuration,
  formatLastFmWeekRange,
  lastFmCoverClass,
  lastFmCovers,
  lastFmRowToCatalogRow,
  lastFmTotalDuration,
  planLastFmPlayback,
} from '@/lib/lastfmTracks'
import { useCatalogStore } from '@/stores/catalog'
import { usePlayerStore } from '@/stores/player'
import * as LastFmService from '../../bindings/airmedy/internal/infra/wails/lastfmservice'
import type {
  RecommendationListDTO,
  TopTrackItemDTO,
} from '../../bindings/airmedy/internal/infra/wails/models'

/**
 * One persisted Last.fm recommendation list.
 *
 * The rows come from the database; a refresh replaces this list only, and the
 * shared match stream fills in the provider identities.
 */
const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const catalogStore = useCatalogStore()
const playerStore = usePlayerStore()

const kind = computed(() => String(route.params.kind ?? ''))
const known = computed(
  () =>
    kind.value === 'top_tracks' ||
    kind.value === 'similar' ||
    kind.value === 'weekly_echo' ||
    kind.value === 'discover',
)
const isWeeklyEcho = computed(() => kind.value === 'weekly_echo')
const isDiscover = computed(() => kind.value === 'discover')
const list = ref<RecommendationListDTO | null>(null)
// Rows streamed while the first GetRecommendationList is still building the
// list; they render immediately instead of waiting for the whole build.
const streamingItems = ref<TopTrackItemDTO[]>([])
const loading = ref(false)
/** loadError is a machine key the template words. */
const loadError = ref('')
const refreshing = ref(false)
const refreshingItem = ref('')
const playing = ref(false)
const loadingMore = ref(false)
/** playNotice is a machine key (plus count) the template words. */
const playNoticeKey = ref('')
const playNoticeCount = ref(0)
const playNotice = computed(() => {
  if (!playNoticeKey.value) return ''
  if (playNoticeKey.value === 'play_skipped') {
    return t('lastfm.play_skipped', { count: playNoticeCount.value })
  }
  return t(`lastfm.${playNoticeKey.value}`)
})
const rowMenu = useContextMenu()
let offMatch: (() => void) | null = null
let offAdded: (() => void) | null = null
// Events can beat the list response; keep them until there are rows to update.
let pendingItems: TopTrackItemDTO[] = []
let pendingAdded: TopTrackItemDTO[] = []
let pendingDone = false
// A request token makes the response of a previous kind walk away instead of
// overwriting the list the user just opened.
let loadToken = 0
// Rows the backend re-queued after a playback failure. A Done frame from an
// earlier worker run must not finish these, or the failover would look like a
// permanent "no match" before the new source answers.
const requeued = new Set<string>()

const items = computed(() => list.value?.items ?? streamingItems.value)
const title = computed(() => (list.value?.title ? t(list.value.title) : t('lastfm.title')))
const isEmpty = computed(() => !!list.value && items.value.length === 0)
const covers = computed(() => lastFmCovers(items.value.map((item) => item.cover_url)))
const totalDuration = computed(() => lastFmTotalDuration(items.value))
const durationLabel = computed(() => formatLastFmDuration(totalDuration.value, t))
const weekRangeLabel = computed(() =>
  formatLastFmWeekRange(list.value?.week_from, list.value?.week_to),
)

const searchQuery = ref('')
/** filteredItems narrows the loaded rows by title and artist, like album detail. */
const filteredItems = computed(() => {
  if (!searchQuery.value) return items.value
  const query = foldUnicode(searchQuery.value)
  return items.value.filter((item) => {
    const artists = [item.artist, ...(item.artists ?? [])].filter(Boolean).join(' ')
    return (
      foldUnicode(item.title || '').includes(query) ||
      foldUnicode(artists).includes(query)
    )
  })
})

async function load() {
  if (!known.value) {
    // Only an unknown kind on this route is an error; a missing kind usually
    // means the user is already navigating away, and redirecting would win
    // over that navigation.
    if (route.name === 'lastfm-list') void router.replace('/lastfm')
    return
  }
  const token = ++loadToken
  loading.value = true
  loadError.value = ''
  pendingItems = []
  pendingAdded = []
  pendingDone = false
  requeued.clear()
  try {
    const loaded = await LastFmService.GetRecommendationList(kind.value)
    if (token !== loadToken) return
    let rows = loaded.items
    if (pendingAdded.length > 0) rows = appendItems(rows, pendingAdded)
    if (pendingItems.length > 0) {
      rows = applyItems(rows, pendingItems, pendingDone)
    } else if (pendingDone) {
      rows = finishItems(rows)
    }
    list.value = { ...loaded, items: rows }
    pendingItems = []
    pendingAdded = []
    pendingDone = false
    streamingItems.value = []
  } catch (error) {
    if (token !== loadToken) return
    console.error('Failed to load a Last.fm recommendation list', error)
    loadError.value = 'load_failed'
  } finally {
    if (token === loadToken) loading.value = false
  }
}

interface TopTrackUpdate {
  kind?: string
  items?: TopTrackItemDTO[]
  done?: boolean
}

/**
 * applyItems merges one batch by match key and, on done, finishes stragglers.
 */
function applyItems(
  rows: TopTrackItemDTO[],
  updates: TopTrackItemDTO[],
  done: boolean,
): TopTrackItemDTO[] {
  const byKey = new Map<string, TopTrackItemDTO>()
  for (const update of updates) {
    if (!update.match_key) continue
    byKey.set(update.match_key, update)
    // A matching update without a provider is the failover signal: the row is
    // being searched again and must survive the next Done frame.
    if (update.matching && !update.provider) requeued.add(update.match_key)
    else requeued.delete(update.match_key)
  }
  const next = rows.map((row) => {
    const update = row.match_key ? byKey.get(row.match_key) : undefined
    return update ? { ...row, ...update } : row
  })
  return done ? finishItems(next) : next
}

/** appendItems adds progressively built rows once, in arrival order. */
function appendItems(
  rows: TopTrackItemDTO[],
  incoming: TopTrackItemDTO[],
): TopTrackItemDTO[] {
  const seen = new Set<string>()
  for (const row of rows) {
    if (row.match_key) seen.add(row.match_key)
  }
  const next = [...rows]
  for (const item of incoming) {
    if (!item.match_key || seen.has(item.match_key)) continue
    seen.add(item.match_key)
    next.push(item)
    if (next.length >= 50) break
  }
  return next
}

function finishItems(rows: TopTrackItemDTO[]): TopTrackItemDTO[] {
  return rows.map((row) =>
    row.matching && !(row.match_key && requeued.has(row.match_key))
      ? { ...row, matching: false, matched: false, no_source: true }
      : row,
  )
}

/**
 * onRecommendationsMatched applies batches for this list only. A combined run
 * sends an empty kind, a single-list refresh names its kind, and a weekly row
 * carries its stored "weekly_echo:<from>" suffix.
 */
function onRecommendationsMatched(event: Events.WailsEvent) {
  const update = (event.data ?? null) as TopTrackUpdate | null
  if (!update) return
  if (!belongsToThisList(update.kind)) return
  const updates = update.items ?? []
  if (!list.value) {
    pendingItems.push(...updates)
    pendingDone = pendingDone || update.done === true
    return
  }
  list.value = {
    ...list.value,
    items: applyItems(list.value.items, updates, update.done === true),
  }
}

/** onItemsAdded appends the rows a similar-list seed just produced. */
function onItemsAdded(event: Events.WailsEvent) {
  const update = (event.data ?? null) as TopTrackUpdate | null
  if (!update) return
  if (!belongsToThisList(update.kind)) return
  const incoming = update.items ?? []
  if (!list.value) {
    pendingAdded.push(...incoming)
    streamingItems.value = appendItems(streamingItems.value, incoming)
    return
  }
  list.value = { ...list.value, items: appendItems(list.value.items, incoming) }
}

/** belongsToThisList accepts the route kind and the weekly row's stored kind. */
function belongsToThisList(updateKind?: string): boolean {
  if (!updateKind) return true
  return updateKind === kind.value || updateKind.startsWith(`${kind.value}:`)
}

function onRowContextMenu(event: MouseEvent, item: TopTrackItemDTO) {
  rowMenu.open(event, [
    {
      label: t('lastfm.refresh_item'),
      icon: RefreshCw,
      disabled: item.matching === true,
      action: () => {
        void refreshItem(item)
      },
    },
  ])
}

/** refreshItem re-runs source matching for one row; other rows keep their data. */
async function refreshItem(item: TopTrackItemDTO) {
  if (item.matching || !item.match_key || refreshingItem.value === item.match_key) return
  refreshingItem.value = item.match_key
  loadError.value = ''
  try {
    await LastFmService.RefreshRecommendationItem(item.kind || kind.value, item.match_key)
  } catch (error) {
    console.error('Failed to refresh a Last.fm recommendation row', error)
    loadError.value = 'load_failed'
  } finally {
    refreshingItem.value = ''
  }
}

async function refresh() {
  if (refreshing.value) return
  refreshing.value = true
  loadError.value = ''
  // The backend clears a similar list before streaming it; mirror that so the
  // new rows can appear progressively instead of inside the old list.
  if (kind.value === 'similar' && list.value) {
    list.value = { ...list.value, items: [] }
  }
  try {
    if (isWeeklyEcho.value) {
      // Re-fetch the week the user is looking at, not always the latest one.
      await LastFmService.RefreshWeeklyEchoWeek(list.value?.week_from ?? 0)
    } else {
      await LastFmService.RefreshRecommendationList(kind.value)
    }
  } catch (error) {
    console.error('Failed to refresh a Last.fm recommendation list', error)
    loadError.value = 'load_failed'
  } finally {
    refreshing.value = false
    await load()
  }
}

/** goWeek opens one stored chart week through the weekly navigation. */
async function goWeek(from: number) {
  if (!from || loading.value) return
  const token = ++loadToken
  loading.value = true
  loadError.value = ''
  playNoticeKey.value = ''
  try {
    const loaded = await LastFmService.GetWeeklyEchoWeek(from)
    if (token !== loadToken) return
    list.value = loaded
    streamingItems.value = []
    pendingItems = []
    pendingAdded = []
    pendingDone = false
    requeued.clear()
  } catch (error) {
    if (token !== loadToken) return
    console.error('Failed to load a Last.fm chart week', error)
    loadError.value = 'load_failed'
  } finally {
    if (token === loadToken) loading.value = false
  }
}

/** loadMore appends the next discover page from the stored candidate pool. */
async function loadMore() {
  if (loadingMore.value || !list.value || !list.value.has_more) return
  loadingMore.value = true
  loadError.value = ''
  try {
    list.value = await LastFmService.LoadMoreRecommendationItems(kind.value)
  } catch (error) {
    console.error('Failed to load more Last.fm recommendations', error)
    loadError.value = 'load_failed'
  } finally {
    loadingMore.value = false
  }
}

/**
 * playAll starts the list's playable rows, in order or shuffled.
 *
 * The header works on the rows already loaded, so it never re-fetches the
 * list; only rows with a source identity or a local copy are resolved. A row
 * that fails to resolve is skipped and worded, never silently dropped.
 */
async function playAll(shuffle: boolean) {
  if (playing.value) return
  playing.value = true
  playNoticeKey.value = ''
  try {
    const plan = await planLastFmPlayback(
      filteredItems.value,
      (row) => catalogStore.preparePlayback(row),
    )
    if (plan.ids.length === 0) {
      playNoticeKey.value = plan.matching ? 'play_matching' : 'play_unavailable'
      return
    }
    if (shuffle) shuffleIds(plan.ids)
    await playerStore.playTrackIds(plan.ids)
    if (plan.skipped > 0) {
      playNoticeKey.value = 'play_skipped'
      playNoticeCount.value = plan.skipped
    }
  } catch (error) {
    console.error('Failed to play a Last.fm recommendation list', error)
  } finally {
    playing.value = false
  }
}

function shuffleIds(ids: string[]) {
  for (let index = ids.length - 1; index > 0; index--) {
    const swap = Math.floor(Math.random() * (index + 1))
    ;[ids[index], ids[swap]] = [ids[swap], ids[index]]
  }
}

/** refreshAllLists rebuilds every persisted list and reloads this one. */
async function refreshAllLists() {
  if (refreshing.value) return
  refreshing.value = true
  loadError.value = ''
  try {
    await LastFmService.RefreshAllRecommendationLists()
  } catch (error) {
    console.error('Failed to refresh all Last.fm recommendation lists', error)
    loadError.value = 'load_failed'
  } finally {
    refreshing.value = false
    await load()
  }
}

/** openHeaderMore offers the list-level refresh actions from the detail header. */
function openHeaderMore(event: MouseEvent) {
  rowMenu.open(event, [
    {
      label: t('lastfm.refresh_list'),
      icon: RefreshCw,
      action: () => {
        void refresh()
      },
    },
    {
      label: t('lastfm.refresh_all'),
      icon: RefreshCw,
      action: () => {
        void refreshAllLists()
      },
    },
  ])
}

const layout = ref<{ scrollContainerRef: HTMLElement | null } | null>(null)
// The sentinel sits under the hero title; once it leaves the page's scroll
// container the title is no longer readable, so the sticky bar takes over.
const titleSentinel = ref<HTMLElement | null>(null)
const showStickyBar = ref(false)
const stickySearchOpen = ref(false)
const stickySearchInput = ref<{ focus: () => void } | null>(null)
let titleObserver: IntersectionObserver | null = null

function goBack() {
  router.back()
}

/**
 * observeTitleSentinel watches the title inside the page's own scroller. The
 * hero only exists once loading finished, and the scroller is recreated for
 * each list, so the observer is rebuilt whenever either side changes.
 */
function observeTitleSentinel() {
  titleObserver?.disconnect()
  titleObserver = null
  const sentinel = titleSentinel.value
  const root = layout.value?.scrollContainerRef ?? null
  if (!sentinel || !root) {
    showStickyBar.value = false
    return
  }
  titleObserver = new IntersectionObserver(
    ([entry]) => {
      showStickyBar.value = !entry.isIntersecting
    },
    { root, rootMargin: '-8px 0px 0px 0px', threshold: 0 },
  )
  titleObserver.observe(sentinel)
}

watch(
  [titleSentinel, () => layout.value?.scrollContainerRef],
  observeTitleSentinel,
  { flush: 'post' },
)

// A hidden bar must not reopen with the search input already expanded.
watch(showStickyBar, (visible) => {
  if (!visible) stickySearchOpen.value = false
})

async function openStickySearch() {
  stickySearchOpen.value = true
  await nextTick()
  stickySearchInput.value?.focus()
}

async function playTrack(item: TopTrackItemDTO) {
  try {
    const trackId = await catalogStore.preparePlayback(lastFmRowToCatalogRow(item))
    if (!trackId) return
    await playerStore.playTrackIds([trackId])
  } catch (error) {
    console.error('Failed to play a Last.fm recommendation', error)
  }
}

function saveTrack(item: TopTrackItemDTO) {
  void catalogStore.save(lastFmRowToCatalogRow(item), { favorite: true })
}

onMounted(() => {
  offMatch = Events.On('lastfm:recommendations-matched', onRecommendationsMatched)
  offAdded = Events.On('lastfm:recommendation-items-added', onItemsAdded)
  observeTitleSentinel()
  void load()
})

// KeepAlive detaches the scroller, so the observer's last result no longer
// describes the page. Rebuild it once the subtree is attached again and let the
// fresh interaction decide whether the bar belongs on screen.
onActivated(() => {
  showStickyBar.value = false
  stickySearchOpen.value = false
  observeTitleSentinel()
})

// The same route name is reused for every list, so a card-to-card navigation
// only changes the parameter. Reload and drop the previous list.
watch(kind, () => {
  if (!known.value) {
    // Leaving this route drops :kind too. Only an unknown kind while this
    // route is still active should fall back to the cards page — redirecting
    // on the way out would cancel the navigation the user just started.
    if (route.name === 'lastfm-list') void router.replace('/lastfm')
    return
  }
  list.value = null
  streamingItems.value = []
  loadError.value = ''
  playNoticeKey.value = ''
  pendingItems = []
  pendingAdded = []
  pendingDone = false
  requeued.clear()
  void load()
})

onUnmounted(() => {
  offMatch?.()
  offMatch = null
  offAdded?.()
  offAdded = null
  titleObserver?.disconnect()
  titleObserver = null
})
</script>

<template>
  <div class="relative h-full">
    <DetailPageLayout
      ref="layout"
      :loading="loading && !list && items.length === 0"
      :title="title"
      body-class="px-2"
      :body-fill-height="false"
    >
    <template #top-right>
      <div class="relative w-64">
        <Search class="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-dim" />
        <Input
          v-model="searchQuery"
          type="text"
          :placeholder="$t('sidebar.search')"
          class="pl-10 pr-4"
          clearable
        />
      </div>
    </template>

    <template #title>
      <h1
        class="text-2xl @sm:text-3xl @md:text-4xl @lg:text-5xl font-bold tracking-tight line-clamp-2 leading-snug text-foreground"
        data-test="lastfm-list-title"
      >
        {{ title }}
      </h1>
      <span ref="titleSentinel" class="block h-px -mt-2" aria-hidden="true" />
    </template>

    <template #artwork>
      <div
        class="h-48 w-48 shrink-0 overflow-hidden rounded-lg bg-foreground/5 shadow-2xl ring-1 ring-foreground/[0.08]"
        data-test="lastfm-list-cover"
      >
        <div v-if="covers.length" class="grid h-full w-full grid-cols-2 grid-rows-2 gap-0.5">
          <div
            v-for="(url, index) in covers"
            :key="index"
            :class="lastFmCoverClass(covers, index)"
            class="overflow-hidden bg-foreground/[0.06]"
          >
            <LazyImg
              v-if="url"
              :src="url"
              :alt="title"
              class="h-full w-full object-cover"
            />
          </div>
        </div>
        <div v-else class="flex h-full w-full items-center justify-center text-foreground opacity-30">
          <Music class="h-24 w-24" aria-hidden="true" />
        </div>
      </div>
    </template>

    <template #metadata>
      <div class="flex items-center gap-2" data-test="lastfm-list-count">
        <Music class="h-4 w-4" />
        <span>{{ items.length }}</span>
      </div>
      <div v-if="totalDuration > 0" class="flex items-center gap-2">
        <Clock class="h-4 w-4" />
        <span data-test="lastfm-list-duration">{{ durationLabel }}</span>
      </div>
    </template>

    <template #actions>
      <DetailsButton
        :icon="Play"
        :label="$t('common.play')"
        :filled-icon="true"
        data-test="lastfm-list-play"
        :disabled="playing || filteredItems.length === 0"
        @click="playAll(false)"
      />
      <div class="flex gap-2">
        <DetailsButton
          :icon="Shuffle"
          variant="outline"
          data-test="lastfm-list-shuffle"
          :disabled="playing || filteredItems.length === 0"
          @click="playAll(true)"
        />
        <DetailsButton
          :icon="MoreVertical"
          variant="outline"
          data-test="lastfm-list-more"
          @click="openHeaderMore"
        />
      </div>
    </template>

    <template #body>
      <p v-if="loadError" class="px-6 pt-4 text-sm text-dim" data-test="lastfm-list-error">
        {{ $t(`lastfm.${loadError}`) }}
      </p>
      <p
        v-if="playNotice"
        class="px-6 pt-2 text-sm text-dim"
        role="status"
        data-test="lastfm-play-notice"
      >
        {{ playNotice }}
      </p>
      <p v-if="catalogStore.actionError" class="px-6 pt-2 text-sm text-dim">
        {{ $t(`cloud_search.${catalogStore.actionError}`) }}
      </p>

      <!-- Weekly chart navigation -->
      <div
        v-if="isWeeklyEcho && list && list.week_from > 0"
        class="mt-4 flex items-center gap-2 px-6"
        data-test="lastfm-week-nav"
      >
        <IconButton
          data-test="lastfm-week-prev"
          :aria-label="$t('lastfm.week_prev')"
          :title="$t('lastfm.week_prev')"
          :disabled="!list.prev_from || loading"
          @click="goWeek(list.prev_from)"
        >
          <ChevronLeft class="h-4 w-4" aria-hidden="true" />
        </IconButton>
        <span class="text-sm text-dim" data-test="lastfm-week-range">{{ weekRangeLabel }}</span>
        <IconButton
          data-test="lastfm-week-next"
          :aria-label="$t('lastfm.week_next')"
          :title="$t('lastfm.week_next')"
          :disabled="!list.next_from || loading"
          @click="goWeek(list.next_from)"
        >
          <ChevronRight class="h-4 w-4" aria-hidden="true" />
        </IconButton>
      </div>

      <!-- Empty -->
      <div
        v-if="isEmpty && !loadError"
        class="mt-16 flex flex-col items-center justify-center text-center"
        data-test="lastfm-list-empty"
      >
        <div class="mb-6 flex h-20 w-20 items-center justify-center rounded-3xl bg-foreground/[0.03] ring-1 ring-foreground/[0.05]">
          <Music class="h-10 w-10 text-foreground opacity-30" aria-hidden="true" />
        </div>
        <p class="text-xl font-semibold text-dim">{{ $t('lastfm.empty_title') }}</p>
        <p class="mt-2 text-dimmer">{{ $t('lastfm.list_empty') }}</p>
        <button
          type="button"
          class="mt-6 rounded-full bg-foreground/[0.06] px-5 py-2 text-sm font-medium text-foreground transition-colors hover:bg-foreground/[0.10]"
          @click="refresh"
        >
          {{ $t('lastfm.refresh_list') }}
        </button>
      </div>

      <div v-else class="mt-4">
        <TopTrackRow
          v-for="item in filteredItems"
          :key="item.match_key"
          :item="item"
          @play="playTrack"
          @save="saveTrack"
          @contextmenu.prevent="onRowContextMenu($event, item)"
        />

        <div v-if="isDiscover && list" class="flex justify-center py-6">
          <button
            type="button"
            class="rounded-full bg-foreground/[0.06] px-5 py-2 text-sm font-medium text-foreground transition-colors hover:bg-foreground/[0.10] disabled:opacity-40"
            data-test="lastfm-load-more"
            :disabled="loadingMore || !list.has_more"
            @click="loadMore"
          >
            {{ list.has_more ? $t('lastfm.load_more') : $t('lastfm.no_more') }}
          </button>
        </div>
      </div>
    </template>
  </DetailPageLayout>

    <!-- Sticky toolbar, revealed once the hero title scrolls out of the page -->
    <div
      class="absolute inset-x-0 top-0 z-30 h-14 transition-all duration-200 ease-out"
      :class="showStickyBar ? 'translate-y-0 opacity-100' : 'pointer-events-none -translate-y-full opacity-0'"
      data-test="lastfm-sticky-bar"
      :data-visible="showStickyBar"
    >
      <div
        role="toolbar"
        :aria-label="title"
        class="flex h-full items-center gap-2 bg-glass px-4 ring-1 ring-border-glass backdrop-blur-xl"
      >
        <IconButton
          data-test="lastfm-sticky-back"
          :aria-label="$t('common.back')"
          :title="$t('common.back')"
          @click="goBack"
        >
          <ArrowLeft class="h-4 w-4" aria-hidden="true" />
        </IconButton>

        <h2 class="min-w-0 flex-1 truncate text-sm font-medium text-foreground" :title="title">
          {{ title }}
        </h2>

        <DetailsButton
          :icon="Play"
          :label="$t('common.play')"
          :filled-icon="true"
          data-test="lastfm-sticky-play"
          :disabled="playing || filteredItems.length === 0"
          @click="playAll(false)"
        />
        <DetailsButton
          :icon="MoreVertical"
          variant="outline"
          data-test="lastfm-sticky-more"
          :aria-label="$t('lastfm.more_actions')"
          :title="$t('lastfm.more_actions')"
          @click="openHeaderMore"
        />

        <Input
          v-if="stickySearchOpen"
          ref="stickySearchInput"
          v-model="searchQuery"
          type="text"
          :placeholder="$t('sidebar.search')"
          class="w-[220px]"
          clearable
          data-test="lastfm-sticky-search-input"
          @keydown.esc="stickySearchOpen = false"
          @blur="stickySearchOpen = false"
        />
        <IconButton
          v-else
          data-test="lastfm-sticky-search"
          :active="!!searchQuery"
          :aria-label="$t('sidebar.search')"
          :title="$t('sidebar.search')"
          @click="openStickySearch"
        >
          <Search class="h-4 w-4" aria-hidden="true" />
        </IconButton>
      </div>
    </div>

    <ContextMenu
      :visible="rowMenu.visible.value"
      :x="rowMenu.x.value"
      :y="rowMenu.y.value"
      :items="rowMenu.items.value"
      @close="rowMenu.close()"
    />
  </div>
</template>
