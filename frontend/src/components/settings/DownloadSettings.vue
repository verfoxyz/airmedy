<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { Download, FolderOpen, RotateCcw } from '@lucide/vue'
import { Select, SelectTrigger, SelectValue, SelectContent, SelectItem, Switch, Input } from '@airmedy/ui'
import * as LibraryService from '../../../bindings/airmedy/internal/infra/wails/libraryservice'
import { useAppStore, DOWNLOAD_QUALITIES, DOWNLOAD_NAMING_ORDERS, type DownloadQuality, type DownloadNamingOrder } from '@/stores/app'
import SettingSection from './SettingSection.vue'
import SettingRow from './SettingRow.vue'

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
    </SettingSection>

    <!-- What is fetched and written beside the audio file. Each switch is an
         independent request, so they are grouped apart from the audio options. -->
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
