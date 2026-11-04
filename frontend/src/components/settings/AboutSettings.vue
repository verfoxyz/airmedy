<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import * as SettingsService from '../../../bindings/airmedy/internal/infra/wails/settingsservice'
import { CloudOff, ChevronRight } from '@lucide/vue'
import { Browser } from '@wailsio/runtime';

const { t } = useI18n()

// State
const appInfo = ref<any>(null)
const isLoading = ref(true)
const sponsorLinks = [
  {
    key: 'settings.about.sponsor_github',
    url: 'https://github.com/sponsors/misa198',
    logo: '/github-sponsor.png',
    logoAlt: 'GitHub Sponsors',
  },
  {
    key: 'settings.about.sponsor_kofi',
    url: 'https://ko-fi.com/misa198',
    logo: '/ko-fi.png',
    logoAlt: 'Ko-fi',
  },
  {
    key: 'settings.about.sponsor_bmac',
    url: 'https://buymeacoffee.com/misa1982',
    logo: '/buy-me-a-coffee.png',
    logoAlt: 'Buy Me a Coffee',
  },
  {
    key: 'settings.about.sponsor_patreon',
    url: 'https://www.patreon.com/c/misa198',
    logo: '/patreon.png',
    logoAlt: 'Patreon',
  },
]

const loadData = async () => {
  isLoading.value = true
  try {
    appInfo.value = await SettingsService.GetAppInfo()
  } catch (err) {
    console.error('Failed to load app info:', err)
  } finally {
    isLoading.value = false
  }
}

const openAppDataFolder = async () => {
  try {
    await SettingsService.OpenAppDataFolder()
  } catch (err) {
    console.error('Failed to open folder:', err)
  }
}

onMounted(() => {
  loadData()
})
</script>

<template>
  <div class="space-y-8 animate-in fade-in slide-in-from-bottom-2 duration-500">
    <div class="text-center py-8">
      <div class="app-icon-shadow w-24 h-24 mx-auto mb-6 rounded-[30%]">
        <img src="/airmedy.png" alt="Airmedy" class="block w-full h-full" />
      </div>
      <h2 class="text-3xl font-black mb-2">{{ appInfo?.name || 'Airmedy' }}</h2>
      <p class="text-sm font-bold text-primary mb-2">{{ t("settings.about.version") }} {{ appInfo?.version || '1.0.0' }}
      </p>

      <!-- In-app updates are disabled in this build; state the reason instead of
           offering a button that the backend would refuse. -->
      <div class="mt-4 flex items-center justify-center gap-2 text-dim min-h-[40px]">
        <CloudOff class="w-4 h-4" />
        <span class="text-xs font-bold">{{ t('settings.about.updates_disabled') }}</span>
      </div>

    </div>

    <section class="space-y-3">
      <h3 class="text-sm font-bold">{{ t('settings.about.support_title') }}</h3>

      <div class="grid grid-cols-1 sm:grid-cols-4 gap-2">
        <button
          v-for="sponsor in sponsorLinks"
          :key="sponsor.url"
          @click="Browser.OpenURL(sponsor.url)"
          class="flex items-center justify-center gap-2 px-3 py-2 bg-card rounded-lg border border-foreground/[0.08] hover:bg-foreground/[0.02] transition-all"
        >
          <div class="flex items-center justify-center gap-2 min-w-0">
            <img v-if="sponsor.logo" :src="sponsor.logo" :alt="sponsor.logoAlt" class="h-5 w-auto object-contain" />
            <span class="text-xs font-semibold truncate">{{ t(sponsor.key) }}</span>
          </div>
        </button>
      </div>
    </section>

    <section class="space-y-3">
      <h3 class="text-sm font-bold">{{ t('settings.about.resources_title', 'Ứng dụng & Nguồn lực') }}</h3>

      <div class="rounded-2xl border border-foreground/[0.08] bg-card overflow-hidden">
        <button
          v-if="appInfo?.github_url"
          @click="Browser.OpenURL(appInfo.github_url)"
          class="w-full flex items-center justify-between px-5 py-4 hover:bg-foreground/[0.02] transition-colors text-left"
        >
          <span class="text-sm font-medium">{{ t('settings.about.github') }}</span>
          <ChevronRight class="w-4 h-4 text-subdued" />
        </button>

        <button
          v-if="appInfo?.license_url"
          @click="Browser.OpenURL(appInfo.license_url)"
          class="w-full flex items-center justify-between px-5 py-4 border-t border-foreground/[0.08] hover:bg-foreground/[0.02] transition-colors text-left"
        >
          <span class="text-sm font-medium">{{ t('settings.about.license') }}</span>
          <ChevronRight class="w-4 h-4 text-subdued" />
        </button>

        <button
          @click="openAppDataFolder"
          class="w-full flex items-center justify-between px-5 py-4 border-t border-foreground/[0.08] hover:bg-foreground/[0.02] transition-colors text-left"
        >
          <span class="text-sm font-medium">{{ t('settings.about.open_data_folder') }}</span>
          <ChevronRight class="w-4 h-4 text-subdued" />
        </button>
      </div>
    </section>
  </div>
</template>

<style scoped>
.app-icon-shadow {
  /* Avoid filter: drop-shadow() here: it occasionally leaves a gray compositing
     surface in the desktop webview when the settings panel is mounted. */
  box-shadow: 0 16px 32px -12px rgb(0 0 0 / 0.35);
}
</style>
