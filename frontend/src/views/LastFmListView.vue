<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { Events } from '@wailsio/runtime'
import { MoreVertical, Music, Play, RefreshCw, Shuffle, Clock, Search } from '@lucide/vue'
import { DetailsButton, Input } from '@airmedy/ui'
import ContextMenu from '@/components/ContextMenu.vue'
import DetailPageLayout from '@/components/DetailPageLayout.vue'
import LazyImg from '@/components/LazyImg.vue'
import TopTrackRow from '@/components/lastfm/TopTrackRow.vue'
import { useContextMenu } from '@/composables/useContextMenu'
import { foldUnicode } from '@airmedy/utils'
import {
  formatLastFmDuration,
  isPlayableLastFmRow,
  lastFmCoverClass,
  lastFmCovers,
  lastFmRowToCatalogRow,
  lastFmTotalDuration,
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
  () => kind.value === 'top_tracks' || kind.value === 'similar',
)
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
 * sends an empty kind, a single-list refresh names its kind.
 */
function onRecommendationsMatched(event: Events.WailsEvent) {
  const update = (event.data ?? null) as TopTrackUpdate | null
  if (!update) return
  if (update.kind && update.kind !== kind.value) return
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
  if (update.kind && update.kind !== kind.value) return
  const incoming = update.items ?? []
  if (!list.value) {
    pendingAdded.push(...incoming)
    streamingItems.value = appendItems(streamingItems.value, incoming)
    return
  }
  list.value = { ...list.value, items: appendItems(list.value.items, incoming) }
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
    await LastFmService.RefreshRecommendationItem(kind.value, item.match_key)
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
    await LastFmService.RefreshRecommendationList(kind.value)
  } catch (error) {
    console.error('Failed to refresh a Last.fm recommendation list', error)
    loadError.value = 'load_failed'
  } finally {
    refreshing.value = false
    await load()
  }
}

/**
 * playAll starts the list's playable rows, in order or shuffled.
 *
 * The header works on the rows already loaded, so it never re-fetches the
 * list; only rows with a source identity or a local copy are resolved.
 */
async function playAll(shuffle: boolean) {
  if (playing.value) return
  playing.value = true
  try {
    const ids: string[] = []
    for (const row of filteredItems.value) {
      if (!isPlayableLastFmRow(row)) continue
      const id = await catalogStore.preparePlayback(lastFmRowToCatalogRow(row))
      if (id) ids.push(id)
    }
    if (ids.length === 0) return
    if (shuffle) shuffleIds(ids)
    await playerStore.playTrackIds(ids)
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
  void load()
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
})
</script>

<template>
  <DetailPageLayout
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
      <p v-if="catalogStore.actionError" class="px-6 pt-2 text-sm text-dim">
        {{ $t(`cloud_search.${catalogStore.actionError}`) }}
      </p>

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
      </div>
    </template>
  </DetailPageLayout>

  <ContextMenu
    :visible="rowMenu.visible.value"
    :x="rowMenu.x.value"
    :y="rowMenu.y.value"
    :items="rowMenu.items.value"
    @close="rowMenu.close()"
  />
</template>
