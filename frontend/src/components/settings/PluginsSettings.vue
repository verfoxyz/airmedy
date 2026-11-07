<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Puzzle, RefreshCw, FolderOpen } from '@lucide/vue'
import * as PluginService from '../../../bindings/airmedy/internal/infra/wails/pluginservice'
import SettingSection from './SettingSection.vue'
import PluginRow from './PluginRow.vue'
import type { PluginInfo, PluginRowLabels } from './pluginTypes'

/**
 * The plugin id whose row owns a sub-list. It is data, not a feature check: the
 * page only needs to know which row gets the "manage" affordance, and the
 * backend owns everything else (see internal/app/lx).
 */
const LX_PLUGIN_ID = 'lx'

// The page can be asked to open the lx source list; SettingsView owns the
// category switch, so navigation is an event rather than a router push here.
const emit = defineEmits<{
  openLxSources: []
}>()

function managesSources(plugin: PluginInfo) {
  return plugin.id === LX_PLUGIN_ID
}

/**
 * Settings → Plugins.
 *
 * One plugin = one functional unit = one row with one switch. The backend returns
 * a flat list and the page renders it as-is: there is no grouping and no nested
 * row, because plugins no longer have children.
 */
const { t, locale } = useI18n()

const plugins = ref<PluginInfo[]>([])
const loading = ref(true)
const loadError = ref('')
const busyIds = ref<string[]>([])
const actionError = ref('')

const labels = computed<PluginRowLabels>(() => ({
  running: t('settings.plugins.status_running'),
  idle: t('settings.plugins.status_idle'),
  failed: t('settings.plugins.status_failed'),
  invalid: t('settings.plugins.status_invalid'),
  error: t('settings.plugins.show_error'),
  hideError: t('settings.plugins.hide_error'),
  networkLabel: t('settings.plugins.network_label'),
  networkHosts: t('settings.plugins.network_hosts'),
  networkAny: t('settings.plugins.network_any'),
  storageLabel: t('settings.plugins.storage_label'),
}))

/**
 * Where the app looks for plugins, straight from the backend. It is a path, not
 * a sentence: hardcoding it in the locale files would be wrong on every platform
 * the app is not currently built for.
 */
const pluginsDirectory = ref('')

async function loadPlugins() {
  loading.value = true
  loadError.value = ''
  try {
    pluginsDirectory.value = await PluginService.GetPluginsDirectory()
    plugins.value = await PluginService.ListPlugins()
  } catch (error) {
    loadError.value = String(error)
  } finally {
    loading.value = false
  }
}

async function rescan() {
  loading.value = true
  loadError.value = ''
  actionError.value = ''
  try {
    plugins.value = await PluginService.RescanPlugins()
  } catch (error) {
    // A scan with one broken manifest still returns the good plugins, so the
    // list is reloaded either way and the failure is shown as a notice.
    loadError.value = String(error)
  } finally {
    loading.value = false
  }
}

async function setEnabled(plugin: PluginInfo, enabled: boolean) {
  busyIds.value = [...busyIds.value, plugin.id]
  actionError.value = ''
  try {
    await PluginService.SetPluginEnabled(plugin.id, enabled)
    await loadPlugins()
  } catch (error) {
    actionError.value = String(error)
  } finally {
    busyIds.value = busyIds.value.filter(id => id !== plugin.id)
  }
}

async function setSetting(plugin: PluginInfo, key: string, value: boolean | number) {
  busyIds.value = [...busyIds.value, plugin.id]
  actionError.value = ''
  try {
    await PluginService.SetPluginSetting(plugin.id, key, value)
    await loadPlugins()
  } catch (error) {
    actionError.value = String(error)
  } finally {
    busyIds.value = busyIds.value.filter(id => id !== plugin.id)
  }
}

function isBusy(id: string) {
  return busyIds.value.includes(id)
}

onMounted(loadPlugins)
</script>

<template>
  <div class="space-y-10 animate-in fade-in slide-in-from-bottom-2 duration-500">
    <SettingSection :icon="Puzzle" :label="t('settings.plugins.title')" variant="panel">
      <div class="flex items-start justify-between gap-4 mb-6">
        <p class="text-xs text-dim max-w-md">{{ t('settings.plugins.description') }}</p>
        <button
          class="flex items-center gap-2 px-3 py-2 bg-foreground/[0.04] text-foreground rounded-xl hover:bg-foreground/[0.08] transition-all text-sm font-bold shrink-0"
          :disabled="loading"
          data-test="plugins-rescan"
          @click="rescan"
        >
          <RefreshCw class="w-4 h-4" :class="loading ? 'opacity-40' : ''" />
          {{ t('settings.plugins.refresh') }}
        </button>
      </div>

      <p v-if="actionError" class="mb-4 text-xs text-dim" data-test="plugins-action-error">
        {{ actionError }}
      </p>

      <div v-if="loading && plugins.length === 0" class="text-sm text-dim" data-test="plugins-loading">
        {{ t('settings.plugins.loading') }}
      </div>

      <p v-else-if="loadError && plugins.length === 0" class="text-sm text-dim" data-test="plugins-load-error">
        {{ loadError }}
      </p>

      <div
        v-else-if="plugins.length === 0"
        class="flex flex-col items-center gap-3 py-10 text-center"
        data-test="plugins-empty"
      >
        <FolderOpen class="w-8 h-8 text-dim" />
        <p class="text-sm font-semibold">{{ t('settings.plugins.empty_title') }}</p>
        <p class="text-xs text-dim max-w-sm">{{ t('settings.plugins.empty_desc') }}</p>
        <code class="text-[11px] text-dim bg-foreground/[0.04] rounded-md px-2 py-1">
          {{ pluginsDirectory }}
        </code>
      </div>

      <div v-else class="divide-y divide-foreground/[0.06] -mx-6" data-test="plugins-list">
        <PluginRow
          v-for="plugin in plugins"
          :key="plugin.id"
          :plugin="plugin"
          :busy="isBusy(plugin.id)"
          :labels="labels"
          :locale="locale"
          :manage-sources="managesSources(plugin)"
          @toggle="enabled => setEnabled(plugin, enabled)"
          @setting-change="(key, value) => setSetting(plugin, key, value)"
          @open="emit('openLxSources')"
        />
      </div>
    </SettingSection>
  </div>
</template>
