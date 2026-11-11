<script setup lang="ts">
import { useRouter } from 'vue-router'
import { Download, Search, Settings, Sparkles } from '@lucide/vue'
import { useI18n } from 'vue-i18n'
import SidebarNav from './SidebarNav.vue'
import SidebarPlaylists from './SidebarPlaylists.vue'
import SidebarItem from './SidebarItem.vue'
import { useKeyboardShortcut } from '@/composables/useKeyboardShortcut'
import { useDeviceStore } from '@/stores/device'
import { useAppStore } from '@/stores/app'
import { useDownloadStore } from '@/stores/download'

const { t } = useI18n()
const router = useRouter()
const deviceStore = useDeviceStore()
const appStore = useAppStore()
const downloadStore = useDownloadStore()

useKeyboardShortcut(
  {
    key: ',',
    [deviceStore.isMac ? 'meta' : 'ctrl']: true,
  },
  () => {
    router.push('/settings')
  },
)

function openUpdateDialog() {
  appStore.isUpdateDialogOpen = true
}
</script>

<template>
  <div class="flex flex-col h-full bg-background w-full">
    <!-- Search section -->
    <div class="px-3 py-2">
      <SidebarItem :to="'/search'" :icon="Search" :label="t('sidebar.search')" />
    </div>

    <!-- Divider -->
    <div class="mx-3 border-t border-foreground/[0.06] mb-1" />

    <!-- Main nav -->
    <SidebarNav />

    <!-- Divider -->
    <div class="mx-3 border-t border-foreground/[0.06] my-1" />

    <!-- Playlists section -->
    <SidebarPlaylists />

    <!-- Divider -->
    <div class="mx-3 border-t border-foreground/[0.06] my-1" />

    <!-- Update section -->
    <div v-if="appStore.updateInfo && !appStore.updateApplied" class="px-3 py-2">
      <button 
        @click="openUpdateDialog"
        class="w-full flex items-center gap-3 px-3 py-2 rounded-lg text-primary bg-primary/10 hover:bg-primary/20 transition-all duration-200 group"
      >
        <Sparkles class="w-4 h-4 animate-pulse" />
        <span class="text-sm font-medium">{{ t('app.update_available') }}</span>
      </button>
    </div>

    <!-- Downloads -->
    <div class="px-3 py-2">
      <SidebarItem to="/downloads" :icon="Download" :label="t('sidebar.download')">
        <template #actions>
          <span
            v-if="downloadStore.activeCount > 0"
            class="h-1.5 w-1.5 rounded-full bg-primary"
            aria-hidden="true"
            data-test="sidebar-downloads-dot"
          />
        </template>
      </SidebarItem>
    </div>

    <!-- Settings section -->
    <div class="px-3 py-2">
      <SidebarItem :to="'/settings'" :icon="Settings" :label="t('sidebar.settings')" />
    </div>
  </div>
</template>
