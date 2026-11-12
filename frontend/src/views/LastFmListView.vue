<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { Events } from '@wailsio/runtime'
import { ArrowLeft, ListMusic, Music, RefreshCw } from '@lucide/vue'
import { IconButton } from '@airmedy/ui'
import TopTrackRow from '@/components/lastfm/TopTrackRow.vue'
import { useCatalogStore } from '@/stores/catalog'
import { usePlayerStore } from '@/stores/player'
import * as LastFmService from '../../bindings/airmedy/internal/infra/wails/lastfmservice'
import type {
  CatalogRowDTO,
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
const loading = ref(false)
/** loadError is a machine key the template words. */
const loadError = ref('')
const refreshing = ref(false)
let offMatch: (() => void) | null = null
// Events can beat the list response; keep them until there are rows to update.
let pendingItems: TopTrackItemDTO[] = []
let pendingDone = false
// Rows the backend re-queued after a playback failure. A Done frame from an
// earlier worker run must not finish these, or the failover would look like a
// permanent "no match" before the new source answers.
const requeued = new Set<string>()

const items = computed(() => list.value?.items ?? [])
const title = computed(() => (list.value?.title ? t(list.value.title) : t('lastfm.title')))
const isEmpty = computed(() => !!list.value && items.value.length === 0)

async function load() {
  if (!known.value) {
    void router.replace('/lastfm')
    return
  }
  if (loading.value) return
  loading.value = true
  loadError.value = ''
  pendingItems = []
  pendingDone = false
  requeued.clear()
  try {
    list.value = await LastFmService.GetRecommendationList(kind.value)
    if (pendingItems.length > 0) {
      list.value = { ...list.value, items: applyItems(list.value.items, pendingItems, pendingDone) }
    } else if (pendingDone) {
      list.value = { ...list.value, items: finishItems(list.value.items) }
    }
    pendingItems = []
    pendingDone = false
  } catch (error) {
    console.error('Failed to load a Last.fm recommendation list', error)
    loadError.value = 'load_failed'
  } finally {
    loading.value = false
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

async function refresh() {
  if (refreshing.value) return
  refreshing.value = true
  loadError.value = ''
  try {
    await LastFmService.RefreshRecommendationList(kind.value)
    await load()
  } catch (error) {
    console.error('Failed to refresh a Last.fm recommendation list', error)
    loadError.value = 'load_failed'
  } finally {
    refreshing.value = false
  }
}

/**
 * toCatalogRow adapts one row to the shared catalogue action shape.
 *
 * The provider fields are only meaningful after a match; the row component
 * hides the actions until then.
 */
function toCatalogRow(item: TopTrackItemDTO): CatalogRowDTO {
  return {
    provider: item.provider,
    external_id: item.external_id,
    title: item.title,
    artists: item.artists?.length ? item.artists : [item.artist],
    album_name: item.album,
    duration: item.duration,
    sources: item.sources ?? [],
    cover_url: item.cover_url,
    cover_url_large: item.cover_url_large,
    downloadable: item.downloadable,
    blocked_reason: item.blocked_reason,
    state: item.state,
    local_track_id: item.local_track_id,
  }
}

async function playTrack(item: TopTrackItemDTO) {
  try {
    const trackId = await catalogStore.preparePlayback(toCatalogRow(item))
    if (!trackId) return
    await playerStore.playTrackIds([trackId])
  } catch (error) {
    console.error('Failed to play a Last.fm recommendation', error)
  }
}

function saveTrack(item: TopTrackItemDTO) {
  void catalogStore.save(toCatalogRow(item), { favorite: true })
}

function back() {
  void router.push('/lastfm')
}

onMounted(() => {
  offMatch = Events.On('lastfm:recommendations-matched', onRecommendationsMatched)
  void load()
})

onUnmounted(() => {
  offMatch?.()
  offMatch = null
})
</script>

<template>
  <div class="h-full overflow-y-auto p-8 custom-scrollbar select-none">
    <!-- Header -->
    <header class="flex items-start justify-between gap-4">
      <div class="min-w-0">
        <div class="flex items-center gap-3">
          <button
            type="button"
            class="flex h-10 w-10 items-center justify-center rounded-xl text-foreground/60 transition-colors hover:bg-foreground/[0.06] hover:text-foreground"
            :aria-label="$t('common.back')"
            :title="$t('common.back')"
            data-test="lastfm-back"
            @click="back"
          >
            <ArrowLeft class="h-5 w-5" aria-hidden="true" />
          </button>
          <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-primary/10 text-primary">
            <ListMusic class="h-5 w-5" aria-hidden="true" />
          </div>
          <h1 class="min-w-0 truncate text-4xl font-bold tracking-tight" data-test="lastfm-list-title">
            {{ title }}
          </h1>
        </div>
        <p class="mt-2 text-xl text-dim" data-test="lastfm-list-count">
          {{ items.length }}
        </p>
      </div>

      <IconButton
        data-test="lastfm-refresh"
        :aria-label="$t('lastfm.refresh_list')"
        :title="$t('lastfm.refresh_list')"
        :disabled="refreshing || loading"
        @click="refresh"
      >
        <RefreshCw class="h-4 w-4" :class="{ 'animate-spin': refreshing }" aria-hidden="true" />
      </IconButton>
    </header>

    <!-- Loading skeleton -->
    <div v-if="loading && !list" class="mt-12 space-y-2" data-test="lastfm-list-loading">
      <div v-for="index in 6" :key="index" class="flex items-center gap-3 p-2">
        <div class="h-10 w-10 animate-pulse rounded-lg bg-foreground/[0.06]" />
        <div class="flex-1 space-y-2">
          <div class="h-3 w-1/3 animate-pulse rounded bg-foreground/[0.06]" />
          <div class="h-3 w-1/4 animate-pulse rounded bg-foreground/[0.04]" />
        </div>
      </div>
    </div>

    <template v-else>
      <p v-if="loadError" class="mt-8 text-sm text-dim" data-test="lastfm-list-error">
        {{ $t(`lastfm.${loadError}`) }}
      </p>
      <p v-if="catalogStore.actionError" class="mt-2 text-sm text-dim">
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

      <div v-else class="mt-8 space-y-1">
        <TopTrackRow
          v-for="item in items"
          :key="item.match_key"
          :item="item"
          @play="playTrack"
          @save="saveTrack"
        />
      </div>
    </template>
  </div>
</template>
