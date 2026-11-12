<script setup lang="ts">
import { computed, onActivated, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { Events } from '@wailsio/runtime'
import { ListMusic, Music, Radio, RefreshCw } from '@lucide/vue'
import { IconButton } from '@airmedy/ui'
import LazyImg from '@/components/LazyImg.vue'
import * as LastFmService from '../../bindings/airmedy/internal/infra/wails/lastfmservice'
import type {
  RecommendationListCollectionDTO,
  RecommendationListSummaryDTO,
} from '../../bindings/airmedy/internal/infra/wails/models'

/**
 * Last.fm page: one card per persisted recommendation list.
 *
 * Opening the page reads the database; only the first visit (or a refresh)
 * spends Last.fm requests. Matching keeps filling the stored rows in the
 * background, so a card's counts settle without leaving the page.
 */
const { t } = useI18n()
const router = useRouter()

const collection = ref<RecommendationListCollectionDTO | null>(null)
const loading = ref(false)
const loaded = ref(false)
/** loadError is a machine key the template words. */
const loadError = ref('')
const refreshingAll = ref(false)
const refreshingKinds = ref<Record<string, boolean>>({})
let offMatch: (() => void) | null = null

const lists = computed(() => collection.value?.lists ?? [])
const notConnected = computed(() => collection.value?.status === 'not_connected')
const isEmpty = computed(
  () => !!collection.value && !notConnected.value && lists.value.length === 0,
)
const isRefreshing = computed(
  () => refreshingAll.value || Object.values(refreshingKinds.value).some(Boolean),
)

async function load() {
  if (loading.value) return
  loading.value = true
  loadError.value = ''
  try {
    collection.value = await LastFmService.ListRecommendationLists()
  } catch (error) {
    console.error('Failed to load Last.fm recommendation lists', error)
    loadError.value = 'load_failed'
  } finally {
    loading.value = false
    loaded.value = true
  }
}

async function refreshAll() {
  if (refreshingAll.value) return
  refreshingAll.value = true
  loadError.value = ''
  try {
    await LastFmService.RefreshAllRecommendationLists()
    await load()
  } catch (error) {
    console.error('Failed to refresh Last.fm recommendation lists', error)
    loadError.value = 'load_failed'
  } finally {
    refreshingAll.value = false
  }
}

async function refreshList(kind: string) {
  if (refreshingKinds.value[kind]) return
  refreshingKinds.value = { ...refreshingKinds.value, [kind]: true }
  loadError.value = ''
  try {
    await LastFmService.RefreshRecommendationList(kind)
    await load()
  } catch (error) {
    console.error('Failed to refresh a Last.fm recommendation list', error)
    loadError.value = 'load_failed'
  } finally {
    refreshingKinds.value = { ...refreshingKinds.value, [kind]: false }
  }
}

function openList(list: RecommendationListSummaryDTO) {
  void router.push(`/lastfm/lists/${list.kind}`)
}

/** covers returns up to four URLs and pads the mosaic so the grid is stable. */
function covers(list: RecommendationListSummaryDTO): (string | null)[] {
  const urls = (list.cover_urls ?? []).filter((url) => !!url).slice(0, 4)
  while (urls.length > 0 && urls.length < 4) urls.push('')
  return urls.length === 0 ? [] : urls
}

/** coverClass keeps a 1–4 cover mosaic filling the square. */
function coverClass(urls: (string | null)[], index: number): string {
  if (urls.length === 1) return 'col-span-2 row-span-2'
  if (urls.length === 2) return 'row-span-2'
  if (urls.length === 3 && index === 0) return 'row-span-2'
  return ''
}

interface TopTrackUpdate {
  kind?: string
  done?: boolean
}

/**
 * onRecommendationsMatched refreshes the cards when a worker run finishes, so
 * the counts show the stored state without polling.
 */
function onRecommendationsMatched(event: Events.WailsEvent) {
  const update = (event.data ?? null) as TopTrackUpdate | null
  if (!update?.done) return
  void load()
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
  if (!loaded.value || collection.value?.status === 'not_connected') void load()
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
        data-test="lastfm-refresh-all"
        :aria-label="$t('lastfm.refresh_all')"
        :title="$t('lastfm.refresh_all')"
        :disabled="isRefreshing || notConnected"
        @click="refreshAll"
      >
        <RefreshCw class="h-4 w-4" :class="{ 'animate-spin': refreshingAll }" aria-hidden="true" />
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
    <div v-else-if="loading && !collection" class="mt-12 grid grid-cols-2 gap-6" data-test="lastfm-loading">
      <div v-for="index in 2" :key="index" class="space-y-3">
        <div class="aspect-square animate-pulse rounded-2xl bg-foreground/[0.06]" />
        <div class="h-3 w-1/2 animate-pulse rounded bg-foreground/[0.06]" />
      </div>
    </div>

    <template v-else>
      <p v-if="loadError" class="mt-8 text-sm text-dim" data-test="lastfm-error">
        {{ $t(`lastfm.${loadError}`) }}
      </p>

      <!-- No lists at all -->
      <div
        v-if="isEmpty && !loadError"
        class="mt-16 flex flex-col items-center justify-center text-center"
        data-test="lastfm-empty"
      >
        <div class="mb-6 flex h-20 w-20 items-center justify-center rounded-3xl bg-foreground/[0.03] ring-1 ring-foreground/[0.05]">
          <Music class="h-10 w-10 text-foreground opacity-30" aria-hidden="true" />
        </div>
        <p class="text-xl font-semibold text-dim">{{ $t('lastfm.empty_title') }}</p>
        <p class="mt-2 text-dimmer">{{ $t('lastfm.list_empty') }}</p>
        <button
          type="button"
          class="mt-6 rounded-full bg-foreground/[0.06] px-5 py-2 text-sm font-medium text-foreground transition-colors hover:bg-foreground/[0.10]"
          @click="refreshAll"
        >
          {{ $t('lastfm.refresh_all') }}
        </button>
      </div>

      <!-- Playlist cards -->
      <div v-else class="mt-10 grid grid-cols-2 gap-6" data-test="lastfm-cards">
        <article
          v-for="list in lists"
          :key="list.kind"
          class="group cursor-pointer"
          data-test="lastfm-card"
          :data-kind="list.kind"
          @click="openList(list)"
        >
          <div class="relative mb-3 aspect-square overflow-hidden rounded-2xl bg-foreground/5 ring-1 ring-foreground/[0.06]">
            <div v-if="covers(list).length" class="grid h-full w-full grid-cols-2 grid-rows-2 gap-0.5">
              <div
                v-for="(url, index) in covers(list)"
                :key="index"
                :class="coverClass(covers(list), index)"
                class="overflow-hidden bg-foreground/[0.06]"
              >
                <LazyImg
                  v-if="url"
                  :src="url"
                  :alt="t(list.title)"
                  class="h-full w-full object-cover transition-transform duration-500 group-hover:scale-105"
                />
              </div>
            </div>
            <div v-else class="flex h-full w-full items-center justify-center text-dimmer">
              <ListMusic class="h-10 w-10" aria-hidden="true" />
            </div>

            <IconButton
              data-test="lastfm-card-refresh"
              class="absolute right-2 top-2 bg-background/80 opacity-0 transition-opacity group-hover:opacity-100"
              :aria-label="$t('lastfm.refresh_list')"
              :title="$t('lastfm.refresh_list')"
              :disabled="!!refreshingKinds[list.kind] || refreshingAll"
              @click.stop="refreshList(list.kind)"
            >
              <RefreshCw
                class="h-4 w-4"
                :class="{ 'animate-spin': refreshingKinds[list.kind] }"
                aria-hidden="true"
              />
            </IconButton>
          </div>

          <div class="space-y-1 px-1">
            <h2 class="truncate text-sm font-semibold text-foreground">{{ $t(list.title) }}</h2>
            <p class="flex items-center gap-1.5 text-xs text-dim">
              <ListMusic class="h-3.5 w-3.5" aria-hidden="true" />
              <span data-test="lastfm-card-count">{{ list.track_count }}</span>
            </p>
          </div>
        </article>
      </div>
    </template>
  </div>
</template>
