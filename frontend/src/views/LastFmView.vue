<script setup lang="ts">
import { computed, onActivated, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { Events } from '@wailsio/runtime'
import { Music, Radio, RefreshCw } from '@lucide/vue'
import { IconButton } from '@airmedy/ui'
import TopTrackRow from '@/components/lastfm/TopTrackRow.vue'
import { useCatalogStore } from '@/stores/catalog'
import { usePlayerStore } from '@/stores/player'
import * as LastFmService from '../../bindings/airmedy/internal/infra/wails/lastfmservice'
import type {
  CatalogRowDTO,
  TopTrackItemDTO,
  TopTrackPageDTO,
} from '../../bindings/airmedy/internal/infra/wails/models'

/**
 * Last.fm top-tracks page (MVP).
 *
 * One Last.fm request returns every row; five source workers then fill in the
 * provider identities through lastfm:recommendations-matched. The page only
 * renders and forwards play/save requests to the shared catalogue store.
 */
const { t } = useI18n()
const router = useRouter()
const catalogStore = useCatalogStore()
const playerStore = usePlayerStore()

const page = ref<TopTrackPageDTO | null>(null)
const loading = ref(false)
const loaded = ref(false)
/** loadError is a machine key the template words. */
const loadError = ref('')
let offMatch: (() => void) | null = null
// Events can beat the page response; keep them until there are rows to update.
let pendingItems: TopTrackItemDTO[] = []
let pendingDone = false
// Rows the backend re-queued after a playback failure. A Done frame from an
// earlier worker run must not finish these, or the failover would look like a
// permanent "no match" before the new source answers.
const requeued = new Set<string>()

const status = computed(() => page.value?.status ?? '')
const notConnected = computed(() => status.value === 'not_connected')
const items = computed(() => page.value?.items ?? [])
const similarItems = computed(() => page.value?.similar_items ?? [])
const isEmpty = computed(
  () => !!page.value && items.value.length === 0 && similarItems.value.length === 0,
)

async function load() {
  if (loading.value) return
  loading.value = true
  loadError.value = ''
  pendingItems = []
  pendingDone = false
  requeued.clear()
  try {
    page.value = await LastFmService.GetTopTrackRecommendations()
    if (page.value) {
      if (pendingItems.length > 0) {
        page.value = applyPageItems(page.value, pendingItems, pendingDone)
      } else if (pendingDone) {
        page.value = finishPage(page.value)
      }
    }
    pendingItems = []
    pendingDone = false
  } catch (error) {
    console.error('Failed to load Last.fm top tracks', error)
    loadError.value = 'load_failed'
  } finally {
    loading.value = false
    loaded.value = true
  }
}

interface TopTrackUpdate {
  items?: TopTrackItemDTO[]
  done?: boolean
}

/**
 * applyPageItems applies one batch to both sections.
 *
 * The backend runs the two lists through the same five-source queue, so a batch
 * can contain rows for either list; a row is replaced in whichever list owns its
 * match key. Done finishes stragglers in both.
 */
function applyPageItems(
  current: TopTrackPageDTO,
  updates: TopTrackItemDTO[],
  done: boolean,
): TopTrackPageDTO {
  const byKey = new Map<string, TopTrackItemDTO>()
  for (const update of updates) {
    if (!update.match_key) continue
    byKey.set(update.match_key, update)
    // A matching update without a provider is the failover signal: the row is
    // being searched again and must survive the next Done frame.
    if (update.matching && !update.provider) requeued.add(update.match_key)
    else requeued.delete(update.match_key)
  }
  const merge = (rows: TopTrackItemDTO[]): TopTrackItemDTO[] => {
    const next = rows.map((row) => {
      const update = row.match_key ? byKey.get(row.match_key) : undefined
      return update ? { ...row, ...update } : row
    })
    return done ? finishItems(next) : next
  }
  return {
    ...current,
    items: merge(current.items),
    similar_items: merge(current.similar_items ?? []),
  }
}

function finishPage(current: TopTrackPageDTO): TopTrackPageDTO {
  return {
    ...current,
    items: finishItems(current.items),
    similar_items: finishItems(current.similar_items ?? []),
  }
}

function finishItems(rows: TopTrackItemDTO[]): TopTrackItemDTO[] {
  return rows.map((row) =>
    row.matching && !(row.match_key && requeued.has(row.match_key))
      ? { ...row, matching: false, matched: false, no_source: true }
      : row,
  )
}

/**
 * onRecommendationsMatched applies one batch of source results.
 *
 * A Done frame is authoritative: it means no row may stay in the matching
 * state, so any row without a result becomes "no match".
 */
function onRecommendationsMatched(event: Events.WailsEvent) {
  const update = (event.data ?? null) as TopTrackUpdate | null
  if (!update) return
  const updates = update.items ?? []
  if (!page.value) {
    pendingItems.push(...updates)
    pendingDone = pendingDone || update.done === true
    return
  }
  page.value = applyPageItems(page.value, updates, update.done === true)
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
    console.error('Failed to play a Last.fm top track', error)
  }
}

function saveTrack(item: TopTrackItemDTO) {
  void catalogStore.save(toCatalogRow(item), { favorite: true })
}

function openSettings() {
  void router.push('/settings/integrations')
}

onMounted(() => {
  offMatch = Events.On('lastfm:recommendations-matched', onRecommendationsMatched)
  void load()
})

onUnmounted(() => {
  offMatch?.()
  offMatch = null
})

// KeepAlive re-activates the cached instance; a not-connected page asks again
// because the user may have connected the account in settings since then.
onActivated(() => {
  if (!loaded.value || page.value?.status === 'not_connected') void load()
})
</script>

<template>
  <div class="h-full overflow-y-auto p-8 custom-scrollbar select-none">
    <!-- Header -->
    <header class="flex items-start justify-between gap-4">
      <div class="min-w-0">
        <div class="flex items-center gap-3">
          <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-primary/10 text-primary">
            <Radio class="h-5 w-5" aria-hidden="true" />
          </div>
          <h1 class="text-4xl font-bold tracking-tight">{{ $t('lastfm.title') }}</h1>
        </div>
        <p class="mt-2 text-xl text-dim">{{ $t('lastfm.subtitle') }}</p>
      </div>

      <IconButton
        data-test="lastfm-refresh"
        :aria-label="$t('lastfm.refresh')"
        :title="$t('lastfm.refresh')"
        :disabled="loading || notConnected"
        @click="load"
      >
        <RefreshCw class="h-4 w-4" :class="{ 'animate-spin': loading }" aria-hidden="true" />
      </IconButton>
    </header>

    <!-- Not connected -->
    <div
      v-if="notConnected"
      class="mt-16 flex flex-col items-center justify-center rounded-3xl bg-foreground/[0.03] p-12 text-center ring-1 ring-foreground/[0.05]"
      data-test="lastfm-not-connected"
    >
      <div class="mb-6 flex h-20 w-20 items-center justify-center rounded-3xl bg-primary/10 text-primary">
        <Radio class="h-10 w-10" aria-hidden="true" />
      </div>
      <p class="text-xl font-semibold text-dim">{{ $t('lastfm.not_connected_title') }}</p>
      <p class="mt-2 max-w-md text-dimmer">{{ $t('lastfm.not_connected_description') }}</p>
      <button
        type="button"
        class="mt-6 rounded-full bg-primary px-5 py-2 text-sm font-semibold text-primary-foreground transition-opacity hover:opacity-90"
        data-test="lastfm-open-settings"
        @click="openSettings"
      >
        {{ $t('lastfm.open_settings') }}
      </button>
    </div>

    <!-- Loading skeleton -->
    <div v-else-if="loading && !page" class="mt-12 space-y-2" data-test="lastfm-loading">
      <div v-for="index in 6" :key="index" class="flex items-center gap-3 p-2">
        <div class="h-10 w-10 animate-pulse rounded-lg bg-foreground/[0.06]" />
        <div class="flex-1 space-y-2">
          <div class="h-3 w-1/3 animate-pulse rounded bg-foreground/[0.06]" />
          <div class="h-3 w-1/4 animate-pulse rounded bg-foreground/[0.04]" />
        </div>
      </div>
    </div>

    <template v-else>
      <p v-if="loadError" class="mt-8 text-sm text-dim" data-test="lastfm-error">
        {{ $t(`lastfm.${loadError}`) }}
      </p>
      <p v-if="catalogStore.actionError" class="mt-2 text-sm text-dim">
        {{ $t(`cloud_search.${catalogStore.actionError}`) }}
      </p>

      <!-- Empty -->
      <div
        v-if="isEmpty && !loadError"
        class="mt-16 flex flex-col items-center justify-center text-center"
        data-test="lastfm-empty"
      >
        <div class="mb-6 flex h-20 w-20 items-center justify-center rounded-3xl bg-foreground/[0.03] ring-1 ring-foreground/[0.05]">
          <Music class="h-10 w-10 text-foreground opacity-30" aria-hidden="true" />
        </div>
        <p class="text-xl font-semibold text-dim">{{ $t('lastfm.empty_title') }}</p>
        <p class="mt-2 text-dimmer">{{ $t('lastfm.empty_description') }}</p>
        <button
          type="button"
          class="mt-6 rounded-full bg-foreground/[0.06] px-5 py-2 text-sm font-medium text-foreground transition-colors hover:bg-foreground/[0.10]"
          @click="load"
        >
          {{ $t('lastfm.refresh') }}
        </button>
      </div>

      <!-- For you + top tracks share the row component and the match stream -->
      <template v-else>
        <section class="mt-10" data-test="lastfm-similar">
          <h2 class="mb-3 text-lg font-semibold text-foreground">
            {{ $t('lastfm.similar_tracks') }}
          </h2>
          <div v-if="similarItems.length > 0" class="space-y-1">
            <TopTrackRow
              v-for="item in similarItems"
              :key="item.match_key"
              :item="item"
              @play="playTrack"
              @save="saveTrack"
            />
          </div>
          <p v-else class="text-sm text-dim" data-test="lastfm-similar-empty">
            {{ $t('lastfm.similar_empty') }}
          </p>
        </section>

        <section class="mt-10" data-test="lastfm-top-tracks">
          <h2 class="mb-3 text-lg font-semibold text-foreground">
            {{ $t('lastfm.top') }}
          </h2>
          <div class="space-y-1">
            <TopTrackRow
              v-for="item in items"
              :key="item.match_key"
              :item="item"
              @play="playTrack"
              @save="saveTrack"
            />
          </div>
        </section>
      </template>
    </template>
  </div>
</template>
