<script setup lang="ts">
import { computed, onActivated, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { Events } from '@wailsio/runtime'
import {
  AlertTriangle,
  Clock,
  Heart,
  Music,
  Radio,
  RefreshCw,
  Sparkles,
  TrendingUp,
  Users,
} from '@lucide/vue'
import { IconButton } from '@airmedy/ui'
import RecommendationCard from '@/components/lastfm/RecommendationCard.vue'
import { useCatalogStore } from '@/stores/catalog'
import { usePlayerStore } from '@/stores/player'
import * as LastFmService from '../../bindings/airmedy/internal/infra/wails/lastfmservice'
import type {
  RecommendationPageDTO,
  RecommendationTrackDTO,
  SimilarArtistDTO,
} from '../../bindings/airmedy/internal/infra/wails/models'

/**
 * Last.fm recommendations.
 *
 * The page is a read-only view over the backend's cached page: opening it never
 * triggers a rebuild unless the cache expired or the user pressed refresh. The
 * rows are catalogue rows, so playing and saving go through the same store the
 * cloud search uses.
 */
const { t } = useI18n()
const router = useRouter()
const catalogStore = useCatalogStore()
const playerStore = usePlayerStore()

const page = ref<RecommendationPageDTO | null>(null)
const loading = ref(false)
const loaded = ref(false)
/** loadError is a machine key the template words. */
const loadError = ref('')
/** matchDone turns rows still marked matching into "no playable source". */
const matchDone = ref(false)
let offMatch: (() => void) | null = null
// A very fast match can beat the page response; keep it until the page exists.
let pendingMatches: RecommendationTrackDTO[] = []
let pendingMatchDone = false
let pendingMatchUser = ''

const status = computed(() => page.value?.status ?? '')
const notConnected = computed(() => status.value === 'not_connected')

const racks = computed(() => {
  const current = page.value
  if (!current) return []
  return [
    { key: 'recent', title: t('lastfm.recent'), icon: Clock, tracks: current.recent_tracks ?? [] },
    { key: 'loved', title: t('lastfm.loved'), icon: Heart, tracks: current.loved_tracks ?? [] },
    { key: 'top', title: t('lastfm.top'), icon: TrendingUp, tracks: current.top_tracks ?? [] },
  ].filter((rack) => rack.tracks.length > 0)
})

const similarArtists = computed(() => page.value?.similar_artists ?? [])
// Only the four track lists decide the empty state; similar artists can still
// be useful when Last.fm has no playable recommendation at all.
const isEmpty = computed(() => {
  const current = page.value
  if (!current) return false
  return (current.tracks ?? []).length === 0 && racks.value.length === 0
})

/** artistInitial is the avatar fallback when Last.fm has no real image. */
function artistInitial(name: string): string {
  const trimmed = name.trim()
  if (!trimmed) return '?'
  return Array.from(trimmed)[0]!.toUpperCase()
}

async function load(force: boolean) {
  if (loading.value) return
  loading.value = true
  loadError.value = ''
  matchDone.value = false
  try {
    page.value = await LastFmService.GetRecommendations(force)
    // An event that arrived while the request was in flight belongs to this
    // page; apply it now that there are rows to update.
    if (page.value && pendingMatchUser === page.value.username) {
      if (pendingMatches.length > 0) {
        page.value = applyMatchUpdate(page.value, pendingMatches)
      }
      if (pendingMatchDone) matchDone.value = true
    }
    pendingMatches = []
    pendingMatchDone = false
    pendingMatchUser = ''
  } catch (error) {
    console.error('Failed to load Last.fm recommendations', error)
    loadError.value = 'load_failed'
  } finally {
    loading.value = false
    loaded.value = true
  }
}

interface RecommendationMatchUpdate {
  username?: string
  items?: RecommendationTrackDTO[]
  done?: boolean
}

/**
 * applyMatchUpdate replaces rows by their stable match key.
 *
 * A matched row keeps its position in whichever lists contain it and gains the
 * provider identity the play/save actions need.
 */
function applyMatchUpdate(
  current: RecommendationPageDTO,
  items: RecommendationTrackDTO[],
): RecommendationPageDTO {
  const matches = new Map<string, RecommendationTrackDTO>()
  for (const item of items) {
    if (item.match_key) matches.set(item.match_key, item)
  }
  const replace = (rows: RecommendationTrackDTO[] | undefined) =>
    (rows ?? []).map((row) => {
      const match = row.match_key ? matches.get(row.match_key) : undefined
      return match ? { ...row, ...match, matching: false } : row
    })

  return {
    ...current,
    tracks: replace(current.tracks),
    recent_tracks: replace(current.recent_tracks),
    loved_tracks: replace(current.loved_tracks),
    top_tracks: replace(current.top_tracks),
  }
}

/**
 * onRecommendationsMatched fills in matched rows as the backend finds them.
 *
 * Events are ignored for another account (the page shows one user); an update
 * that arrives before the page response is buffered until `load` resolves.
 */
function onRecommendationsMatched(event: Events.WailsEvent) {
  const update = (event.data ?? null) as RecommendationMatchUpdate | null
  if (!update) return
  if (!page.value) {
    // The page request has not resolved yet; remember the update for it.
    if (update.username) {
      pendingMatchUser = update.username
      pendingMatches.push(...(update.items ?? []))
      pendingMatchDone = pendingMatchDone || update.done === true
    }
    return
  }
  if (update.username !== page.value.username) return
  if (update.items && update.items.length > 0) {
    page.value = applyMatchUpdate(page.value, update.items)
  }
  if (update.done) matchDone.value = true
}

/**
 * playTrack streams the row through the catalogue's playback preparation.
 *
 * Imported files play from disk; a remote row is registered and resolved by the
 * stream cache, exactly like a cloud search row.
 */
async function playTrack(track: RecommendationTrackDTO) {
  try {
    const trackId = await catalogStore.preparePlayback(track)
    if (!trackId) return
    await playerStore.playTrackIds([trackId])
  } catch (error) {
    console.error('Failed to play a Last.fm recommendation', error)
  }
}

/** saveTrack favorites the row, which queues its download like the search page. */
function saveTrack(track: RecommendationTrackDTO) {
  void catalogStore.save(track, { favorite: true })
}

function openSettings() {
  void router.push('/settings/integrations')
}

/** searchArtist opens the search page with the artist prefilled. */
function searchArtist(artist: SimilarArtistDTO | string) {
  const name = typeof artist === 'string' ? artist : artist.name
  if (!name.trim()) return
  void router.push({ path: '/search', query: { q: name } })
}

function listenersLabel(count: number): string {
  if (!count) return ''
  return new Intl.NumberFormat().format(count)
}

onMounted(() => {
  offMatch = Events.On('lastfm:recommendations-matched', onRecommendationsMatched)
  void load(false)
})

onUnmounted(() => {
  offMatch?.()
  offMatch = null
})

// KeepAlive re-activates the cached instance; a page that already loaded keeps
// its rows. A not-connected page is the exception: the user may have connected
// the account in settings since it was cached, so it asks again.
onActivated(() => {
  if (!loaded.value || page.value?.status === 'not_connected') void load(false)
})
</script>

<template>
  <div class="h-full overflow-y-auto p-8 custom-scrollbar select-none">
    <!-- Header -->
    <header class="flex items-start justify-between gap-4">
      <div class="min-w-0">
        <div class="flex items-center gap-3">
          <img
            v-if="page?.avatar_url"
            :src="page.avatar_url"
            :alt="page.username"
            class="h-10 w-10 rounded-xl border border-foreground/[0.06] object-cover"
          />
          <div
            v-else
            class="flex h-10 w-10 items-center justify-center rounded-xl bg-primary/10 text-primary"
          >
            <Radio class="h-5 w-5" aria-hidden="true" />
          </div>
          <h1 class="text-4xl font-bold tracking-tight">{{ $t('lastfm.title') }}</h1>
        </div>
        <p class="mt-2 text-xl text-dim">
          <template v-if="page?.username">
            {{ $t('lastfm.subtitle_account', { username: page.username }) }}
          </template>
          <template v-else>{{ $t('lastfm.subtitle') }}</template>
        </p>
      </div>

      <IconButton
        data-test="lastfm-refresh"
        :aria-label="$t('lastfm.refresh')"
        :title="$t('lastfm.refresh')"
        :disabled="loading || notConnected"
        @click="load(true)"
      >
        <RefreshCw class="h-4 w-4" :class="{ 'animate-spin': loading }" aria-hidden="true" />
      </IconButton>
    </header>

    <!-- Not connected: the page explains where the account lives. -->
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
    <div v-else-if="loading && !page" class="mt-12 space-y-10" data-test="lastfm-loading">
      <div class="space-y-4">
        <div class="h-7 w-48 animate-pulse rounded-lg bg-foreground/[0.06]" />
        <div class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6">
          <div v-for="index in 6" :key="index" class="space-y-2">
            <div class="aspect-square animate-pulse rounded-xl bg-foreground/[0.04]" />
            <div class="h-3 w-3/4 animate-pulse rounded bg-foreground/[0.06]" />
            <div class="h-3 w-1/2 animate-pulse rounded bg-foreground/[0.04]" />
          </div>
        </div>
      </div>
    </div>

    <template v-else>
      <!-- Partial build: some requests or matches failed. -->
      <div
        v-if="status === 'partial'"
        class="mt-8 flex items-center gap-2 rounded-xl bg-foreground/[0.04] px-4 py-3 text-sm text-dim"
        data-test="lastfm-partial"
      >
        <AlertTriangle class="h-4 w-4 shrink-0" aria-hidden="true" />
        {{ $t('lastfm.partial') }}
      </div>

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
          @click="load(true)"
        >
          {{ $t('lastfm.refresh') }}
        </button>
      </div>

      <template v-else>
        <!-- Combined list -->
        <section v-if="(page?.tracks ?? []).length > 0" class="mt-12" data-test="lastfm-combined">
          <div class="mb-6 flex items-center gap-3">
            <div class="rounded-lg bg-primary/10 p-2 text-primary">
              <Sparkles class="h-5 w-5" aria-hidden="true" />
            </div>
            <h2 class="text-2xl font-bold tracking-tight">{{ $t('lastfm.combined') }}</h2>
          </div>
          <div class="grid grid-cols-2 gap-x-4 gap-y-8 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6">
            <RecommendationCard
              v-for="track in page?.tracks ?? []"
              :key="track.match_key"
              :track="track"
              :match-done="matchDone"
              @play="playTrack"
              @save="saveTrack"
            />
          </div>
        </section>

        <!-- Per-source racks -->
        <section
          v-for="rack in racks"
          :key="rack.key"
          class="mt-12"
          :data-test="`lastfm-rack-${rack.key}`"
        >
          <div class="mb-6 flex items-center gap-3">
            <div class="rounded-lg bg-foreground/[0.06] p-2 text-dim">
              <component :is="rack.icon" class="h-5 w-5" aria-hidden="true" />
            </div>
            <h2 class="text-2xl font-bold tracking-tight">{{ rack.title }}</h2>
          </div>
          <div class="flex gap-4 overflow-x-auto pb-2 custom-scrollbar">
            <div v-for="track in rack.tracks" :key="`${rack.key}-${track.match_key}`" class="w-36 shrink-0">
              <RecommendationCard :track="track" :match-done="matchDone" @play="playTrack" @save="saveTrack" />
            </div>
          </div>
        </section>
      </template>

      <!-- Similar artists stay visible even when no recommendation could be
           resolved, so the page never looks broken. -->
      <section v-if="similarArtists.length > 0" class="mt-12" data-test="lastfm-similar-artists">
        <div class="mb-6 flex items-center gap-3">
          <div class="rounded-lg bg-foreground/[0.06] p-2 text-dim">
            <Users class="h-5 w-5" aria-hidden="true" />
          </div>
          <h2 class="text-2xl font-bold tracking-tight">{{ $t('lastfm.similar_artists') }}</h2>
        </div>
        <div class="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5">
          <button
            v-for="artist in similarArtists"
            :key="artist.name"
            type="button"
            class="flex min-w-0 items-center gap-3 rounded-xl p-2 text-left transition-colors hover:bg-foreground/[0.04]"
            data-test="lastfm-similar-artist"
            :title="$t('lastfm.search_artist', { artist: artist.name })"
            @click="searchArtist(artist)"
          >
            <div class="h-12 w-12 shrink-0 overflow-hidden rounded-full bg-foreground/[0.06] ring-1 ring-foreground/[0.06]">
              <img
                v-if="artist.image_url"
                :src="artist.image_url"
                :alt="artist.name"
                class="h-full w-full object-cover"
                loading="lazy"
              />
              <div
                v-else
                class="flex h-full w-full items-center justify-center bg-primary/10 text-sm font-semibold text-primary"
                data-test="lastfm-artist-initial"
              >
                {{ artistInitial(artist.name) }}
              </div>
            </div>
            <div class="min-w-0">
              <p class="truncate text-sm font-semibold text-foreground">{{ artist.name }}</p>
              <p v-if="artist.listeners" class="truncate text-xs text-dimmer">
                {{ $t('lastfm.listeners', { count: listenersLabel(artist.listeners) }) }}
              </p>
            </div>
          </button>
        </div>
      </section>
    </template>
  </div>
</template>
