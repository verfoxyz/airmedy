<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Download, FolderOpen, RotateCcw, AlertTriangle, Trash2 } from '@lucide/vue'
import { Select, SelectTrigger, SelectValue, SelectContent, SelectItem, Switch, Input } from '@airmedy/ui'
import * as LibraryService from '../../../bindings/airmedy/internal/infra/wails/libraryservice'
import * as DownloadService from '../../../bindings/airmedy/internal/infra/wails/downloadservice'
import type { DuplicateCacheReport } from '../../../bindings/airmedy/internal/domain/models'
import {
  useAppStore,
  DOWNLOAD_QUALITIES,
  DOWNLOAD_NAMING_ORDERS,
  MIN_STREAM_CACHE_MB,
  MAX_STREAM_CACHE_MB,
  type DownloadQuality,
  type DownloadNamingOrder,
} from '@/stores/app'
import SettingSection from './SettingSection.vue'
import SettingRow from './SettingRow.vue'
import ConfirmDialog from '../ConfirmDialog.vue'

const { t } = useI18n()
const appStore = useAppStore()

// An empty directory means "use the default" (<Music>/Airmedy). The backend
// resolves it at download time, so the field shows the default as a hint rather
// than persisting an absolute path that would go stale if the Music folder moves.
const dirPlaceholder = computed(() => t('settings.download.dir_placeholder'))

const pickDirectory = async () => {
  const selected = await LibraryService.SelectFolder()
  if (selected) await appStore.updateDownloadDir(selected)
}

const clearDirectory = () => appStore.updateDownloadDir('')

const qualityLabel = (q: DownloadQuality) => t(`settings.download.quality_${q}`)

const namingOrderLabel = (o: DownloadNamingOrder) =>
  o === 'artist-title'
    ? t('settings.download.naming_artist_title')
    : t('settings.download.naming_title_artist')

/**
 * The translation switch depends on the lyric switch: a translation of lyrics
 * that are never fetched does not exist. Rather than hide it (which would make
 * the option look absent), the row stays visible, dimmed, and explains the
 * dependency — the same wording the backend applies when it collapses the pair.
 */
const translationDescription = computed(() =>
  appStore.downloadWithLyrics
    ? t('settings.download.with_lyric_translation_desc')
    : t('settings.download.with_lyric_translation_needs_lyrics'),
)

/** The stream tier and the download tier should match; a lower stream tier is
 * flagged because every save would otherwise re-download the better copy. */
const qualityMismatch = computed(() => appStore.streamQuality !== appStore.downloadQuality)
const streamQualityNeedsMatch = computed(
  () => qualityRank(appStore.streamQuality) < qualityRank(appStore.downloadQuality),
)
const streamQualityDescription = computed(() =>
  streamQualityNeedsMatch.value
    ? t('settings.download.stream_quality_mismatch')
    : t('settings.download.stream_quality_desc'),
)

function qualityRank(quality: DownloadQuality): number {
  if (quality === 'flac') return 3
  if (quality === '320k') return 2
  return 1
}

function onCacheSizeChange(event: Event) {
  const value = Number((event.target as HTMLInputElement).value)
  void appStore.updateStreamCacheMB(value)
}

// --- Duplicate-cache cleanup ------------------------------------------------
//
// The scan is a read-only report; the user confirms it before anything is
// deleted. While either call runs the button is disabled so a double click
// cannot delete twice.
const dedupeScanning = ref(false)
const dedupeRunning = ref(false)
const dedupeOpen = ref(false)
const dedupePending = ref<DuplicateCacheReport | null>(null)
const dedupeMessage = ref('')
const dedupeFailed = ref(false)
const dedupeBusy = computed(() => dedupeScanning.value || dedupeRunning.value)

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  const digits = value >= 100 || unit === 0 ? 0 : 1
  return `${value.toFixed(digits)} ${units[unit]}`
}

/** Deleted entries plus orphan files, which the report counts separately. */
function removableCount(report: DuplicateCacheReport | null): number {
  if (!report) return 0
  return (report.removable ?? 0) + (report.orphan_files ?? 0)
}

const dedupeReportText = computed(() =>
  t('settings.download.dedupe_report', {
    count: removableCount(dedupePending.value),
    size: formatBytes(dedupePending.value?.removable_bytes ?? 0),
  }),
)

async function scanDuplicates() {
  if (dedupeBusy.value) return
  dedupeScanning.value = true
  dedupeMessage.value = ''
  dedupeFailed.value = false
  try {
    const report = await DownloadService.ScanDuplicateCache()
    if (removableCount(report) === 0) {
      dedupePending.value = null
      dedupeMessage.value = t('settings.download.dedupe_nothing')
      return
    }
    dedupePending.value = report
    dedupeOpen.value = true
  } catch (error) {
    console.error('Duplicate cache scan failed:', error)
    dedupeFailed.value = true
    dedupeMessage.value = t('settings.download.dedupe_failed')
  } finally {
    dedupeScanning.value = false
  }
}

async function cleanDuplicates() {
  dedupeOpen.value = false
  if (dedupeRunning.value) return
  dedupeRunning.value = true
  dedupeMessage.value = ''
  dedupeFailed.value = false
  try {
    const result = await DownloadService.CleanDuplicateCache()
    dedupeMessage.value = t('settings.download.dedupe_done', {
      count: removableCount(result),
      size: formatBytes(result?.removable_bytes ?? 0),
    })
    if (result?.skipped?.length) {
      dedupeMessage.value += ` ${t('settings.download.dedupe_skipped', { count: result.skipped.length })}`
    }
    dedupePending.value = null
  } catch (error) {
    console.error('Duplicate cache cleanup failed:', error)
    dedupeFailed.value = true
    dedupeMessage.value = t('settings.download.dedupe_failed')
  } finally {
    dedupeRunning.value = false
  }
}
</script>

<template>
  <div class="space-y-10 animate-in fade-in slide-in-from-bottom-2 duration-500">
    <SettingSection :icon="Download" :label="t('settings.download.title')">
      <SettingRow :title="t('settings.download.dir')" :description="t('settings.download.dir_desc')">
        <div class="flex items-center gap-2">
          <Input :model-value="appStore.downloadDir" :placeholder="dirPlaceholder" readonly
            class="w-[240px] bg-foreground/[0.04] border-0 h-9 text-sm" data-test="download-dir" />
          <button
            class="flex items-center gap-2 px-3 py-2 bg-foreground/[0.04] text-foreground rounded-xl hover:bg-foreground/[0.08] transition-all text-sm font-bold shrink-0"
            data-test="download-browse" :title="t('settings.download.browse')" @click="pickDirectory">
            <FolderOpen class="w-4 h-4" />
          </button>
          <button
            v-if="appStore.downloadDir"
            class="flex items-center gap-1 px-2 py-2 text-[color:var(--text-muted)] hover:text-foreground transition-colors text-xs shrink-0"
            data-test="download-reset" @click="clearDirectory">
            <RotateCcw class="w-3.5 h-3.5" />
            {{ t('settings.download.use_default') }}
          </button>
        </div>
      </SettingRow>

      <SettingRow :title="t('settings.download.quality')" :description="t('settings.download.quality_desc')">
        <Select :model-value="appStore.downloadQuality"
          @update:model-value="q => appStore.updateDownloadQuality(q as DownloadQuality)">
          <SelectTrigger class="w-[140px] bg-foreground/[0.04] border-0 h-9 text-sm" data-test="download-quality">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem v-for="q in DOWNLOAD_QUALITIES" :key="q" :value="q">
              {{ qualityLabel(q) }}
            </SelectItem>
          </SelectContent>
        </Select>
      </SettingRow>

      <SettingRow :title="t('settings.download.naming_order')" :description="t('settings.download.naming_order_desc')">
        <Select :model-value="appStore.downloadNamingOrder"
          @update:model-value="o => appStore.updateDownloadNamingOrder(o as DownloadNamingOrder)">
          <SelectTrigger class="w-[180px] bg-foreground/[0.04] border-0 h-9 text-sm" data-test="download-naming-order">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem v-for="o in DOWNLOAD_NAMING_ORDERS" :key="o" :value="o">
              {{ namingOrderLabel(o) }}
            </SelectItem>
          </SelectContent>
        </Select>
      </SettingRow>

      <SettingRow :title="t('settings.download.auto_on_add')" :description="t('settings.download.auto_on_add_desc')">
        <Switch :model-value="appStore.autoDownloadOnPlaylistAdd" data-test="download-auto"
          @update:model-value="appStore.updateAutoDownloadOnPlaylistAdd" />
      </SettingRow>

      <SettingRow :title="t('settings.download.stream_cache')" :description="t('settings.download.stream_cache_desc')">
        <div class="flex items-center gap-2">
          <Input
            type="number"
            :min="MIN_STREAM_CACHE_MB"
            :max="MAX_STREAM_CACHE_MB"
            :model-value="String(appStore.streamCacheMB)"
            class="w-[120px] bg-foreground/[0.04] border-0 h-9 text-sm"
            data-test="stream-cache-mb"
            @change="onCacheSizeChange"
          />
          <span class="text-xs text-subdued">MB</span>
        </div>
      </SettingRow>

      <SettingRow :title="t('settings.download.stream_quality')" :description="streamQualityDescription">
        <div class="flex items-center gap-2">
          <Select :model-value="appStore.streamQuality"
            @update:model-value="q => appStore.updateStreamQuality(q as DownloadQuality)">
            <SelectTrigger class="w-[140px] bg-foreground/[0.04] border-0 h-9 text-sm" data-test="stream-quality">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem v-for="q in DOWNLOAD_QUALITIES" :key="q" :value="q">
                {{ qualityLabel(q) }}
              </SelectItem>
            </SelectContent>
          </Select>
          <AlertTriangle
            v-if="qualityMismatch"
            class="w-4 h-4 text-primary"
            data-test="stream-quality-warning"
            :title="t('settings.download.stream_quality_mismatch')"
          />
        </div>
      </SettingRow>

      <SettingRow :title="t('settings.download.dedupe')" :description="t('settings.download.dedupe_desc')">
        <div class="flex flex-col items-end gap-1.5">
          <button
            type="button"
            class="flex items-center gap-2 rounded-xl bg-foreground/[0.04] px-3 py-2 text-sm font-bold text-foreground transition-all hover:bg-foreground/[0.08] disabled:cursor-not-allowed disabled:opacity-50"
            :disabled="dedupeBusy"
            data-test="dedupe-scan"
            @click="scanDuplicates">
            <Trash2 class="h-4 w-4" />
            {{ dedupeScanning ? t('settings.download.dedupe_scanning') : t('settings.download.dedupe_scan') }}
          </button>
          <p
            v-if="dedupeMessage"
            class="max-w-[260px] text-right text-xs"
            :class="dedupeFailed ? 'text-primary' : 'text-subdued'"
            role="status"
            data-test="dedupe-status">
            {{ dedupeMessage }}
          </p>
        </div>
      </SettingRow>
    </SettingSection>

    <ConfirmDialog
      :open="dedupeOpen"
      :title="t('settings.download.dedupe')"
      :message="dedupeReportText"
      :confirm-label="t('settings.download.dedupe_clean')"
      danger
      @update:open="dedupeOpen = $event"
      @cancel="dedupeOpen = false"
      @confirm="cleanDuplicates" />
    <SettingSection :icon="Download" :label="t('settings.download.content_title')">
      <SettingRow :title="t('settings.download.with_lyrics')" :description="t('settings.download.with_lyrics_desc')">
        <Switch :model-value="appStore.downloadWithLyrics" data-test="download-with-lyrics"
          @update:model-value="appStore.updateDownloadWithLyrics" />
      </SettingRow>

      <!-- A translation cannot exist without the lyrics, so this row is visibly
           dependent instead of silently doing nothing. -->
      <SettingRow
        :title="t('settings.download.with_lyric_translation')"
        :description="translationDescription"
      >
        <Switch
          :model-value="appStore.downloadWithLyricTranslation"
          :disabled="!appStore.downloadWithLyrics"
          data-test="download-with-translation"
          @update:model-value="appStore.updateDownloadWithLyricTranslation"
        />
      </SettingRow>

      <SettingRow :title="t('settings.download.with_cover')" :description="t('settings.download.with_cover_desc')">
        <Switch :model-value="appStore.downloadWithCover" data-test="download-with-cover"
          @update:model-value="appStore.updateDownloadWithCover" />
      </SettingRow>
    </SettingSection>
  </div>
</template>
