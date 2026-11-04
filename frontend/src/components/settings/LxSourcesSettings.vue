<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ArrowLeft, Plus, FolderOpen, Puzzle } from '@lucide/vue'
import * as LxSourcesService from '../../../bindings/airmedy/internal/infra/wails/lxsourcesservice'
import SettingSection from './SettingSection.vue'
import LxSourceRow from './LxSourceRow.vue'
import ConfirmDialog from '../ConfirmDialog.vue'
import type { LxSource, LxSourceRowLabels, LxSourcesState } from './lxSourceTypes'

/**
 * Settings → Plugins → Lx 自定义源.
 *
 * The Lx plugin is one plugin with a list under it: the user imports .js custom
 * sources, enables/disables them, and orders them. Order is priority — when the
 * first source cannot get a track, the plugin tries the next one (the plugin
 * implements that fallback; the core only stores the list).
 *
 * Navigation is a sibling settings category (`plugins/lx`), not a nested route:
 * SettingsView already switches on `activeCategory`, so one more id is cheaper
 * and more predictable than introducing route nesting.
 */
const props = defineProps<{
  /** Called when the page's back button is pressed. */
  back: () => void
}>()

const { t } = useI18n()

const state = ref<LxSourcesState>({
  sources: [],
  directory: '',
  plugin_running: false,
  plugin_error: '',
})
const loading = ref(true)
const loadError = ref('')
const actionError = ref('')
const busyIds = ref<string[]>([])
const pendingRemoval = ref<LxSource | null>(null)

const labels = computed<LxSourceRowLabels>(() => ({
  enable: t('settings.plugins.lx.enabled'),
  disable: t('settings.plugins.lx.disabled'),
  remove: t('settings.plugins.lx.remove'),
  moveUp: t('settings.plugins.lx.move_up'),
  moveDown: t('settings.plugins.lx.move_down'),
  providers: t('settings.plugins.lx.providers'),
  noProviders: t('settings.plugins.lx.no_providers'),
  pluginOff: t('settings.plugins.lx.plugin_off'),
  failed: t('settings.plugins.lx.failed'),
}))

async function loadSources() {
  loading.value = true
  loadError.value = ''
  try {
    state.value = await LxSourcesService.ListLxSources()
  } catch (error) {
    loadError.value = String(error)
  } finally {
    loading.value = false
  }
}

/** runAction wraps a mutation so errors land in the notice area, not the console. */
async function runAction(id: string, action: () => Promise<void>) {
  busyIds.value = [...busyIds.value, id]
  actionError.value = ''
  try {
    await action()
    await loadSources()
  } catch (error) {
    actionError.value = String(error)
  } finally {
    busyIds.value = busyIds.value.filter(value => value !== id)
  }
}

async function addSource() {
  actionError.value = ''
  try {
    const created = await LxSourcesService.ImportLxSource()
    if (created) await loadSources()
  } catch (error) {
    // The picker returning nothing means the user cancelled, which is not an
    // error; only a real failure reaches this branch.
    actionError.value = String(error)
  }
}

function isBusy(id: string) {
  return busyIds.value.includes(id)
}

function setEnabled(source: LxSource, enabled: boolean) {
  return runAction(source.id, () => LxSourcesService.SetLxSourceEnabled(source.id, enabled))
}

function requestRemoval(source: LxSource) {
  pendingRemoval.value = source
}

async function confirmRemoval() {
  const source = pendingRemoval.value
  pendingRemoval.value = null
  if (source) await runAction(source.id, () => LxSourcesService.RemoveLxSource(source.id))
}

/** moveSource swaps two rows and persists the whole new order. */
async function moveSource(index: number, direction: -1 | 1) {
  const target = index + direction
  const sources = state.value.sources
  if (target < 0 || target >= sources.length) return

  const ordered = sources.map(source => source.id)
  ;[ordered[index], ordered[target]] = [ordered[target], ordered[index]]
  const moved = sources[target]

  await runAction(moved.id, async () => {
    await LxSourcesService.ReorderLxSources(ordered)
    // Optimistically reflect the move so the list does not flick back while the
    // plugin restarts.
    const next = [...sources]
    ;[next[index], next[target]] = [next[target], next[index]]
    state.value = { ...state.value, sources: next }
  })
}

onMounted(loadSources)
</script>

<template>
  <div class="space-y-8 animate-in fade-in slide-in-from-bottom-2 duration-500">
    <div class="flex items-center justify-between gap-3">
      <button
        class="flex items-center gap-2 text-sm font-semibold text-foreground opacity-70 hover:opacity-100 transition-all"
        data-test="lx-sources-back"
        @click="props.back()"
      >
        <ArrowLeft class="w-4 h-4" />
        {{ t('settings.plugins.lx.back') }}
      </button>
      <button
        class="flex items-center gap-2 px-3 py-2 bg-foreground/[0.04] text-foreground rounded-xl hover:bg-foreground/[0.08] transition-all text-sm font-bold"
        :disabled="loading"
        data-test="lx-sources-add"
        @click="addSource"
      >
        <Plus class="w-4 h-4" />
        {{ t('settings.plugins.lx.add') }}
      </button>
    </div>

    <SettingSection :icon="Puzzle" :label="t('settings.plugins.lx.title')" variant="panel">
      <p class="text-xs text-dim mb-4 max-w-lg">{{ t('settings.plugins.lx.description') }}</p>

      <p v-if="actionError" class="mb-4 text-xs text-dim" data-test="lx-sources-action-error">
        {{ actionError }}
      </p>
      <p
        v-if="!loading && !state.plugin_running && state.plugin_error"
        class="mb-4 text-xs text-dim"
        data-test="lx-sources-plugin-off"
      >
        {{ labels.pluginOff }} — {{ state.plugin_error }}
      </p>

      <div v-if="loading && state.sources.length === 0" class="text-sm text-dim" data-test="lx-sources-loading">
        {{ t('settings.plugins.loading') }}
      </div>

      <p v-else-if="loadError && state.sources.length === 0" class="text-sm text-dim" data-test="lx-sources-load-error">
        {{ loadError }}
      </p>

      <div
        v-else-if="state.sources.length === 0"
        class="flex flex-col items-center gap-3 py-10 text-center"
        data-test="lx-sources-empty"
      >
        <FolderOpen class="w-8 h-8 text-dim" />
        <p class="text-sm font-semibold">{{ t('settings.plugins.lx.empty_title') }}</p>
        <p class="text-xs text-dim max-w-sm">{{ t('settings.plugins.lx.empty_desc') }}</p>
        <code v-if="state.directory" class="text-[11px] text-dim bg-foreground/[0.04] rounded-md px-2 py-1">
          {{ state.directory }}
        </code>
      </div>

      <div v-else class="divide-y divide-foreground/[0.06] -mx-6" data-test="lx-sources-list">
        <LxSourceRow
          v-for="(source, index) in state.sources"
          :key="source.id"
          :source="source"
          :index="index"
          :total="state.sources.length"
          :busy="isBusy(source.id)"
          :labels="labels"
          @toggle="enabled => setEnabled(source, enabled)"
          @remove="requestRemoval(source)"
          @move="direction => moveSource(index, direction)"
        />
      </div>

      <p v-if="state.sources.length > 0" class="mt-4 text-[11px] text-dim" data-test="lx-sources-order-hint">
        {{ t('settings.plugins.lx.order_hint') }}
      </p>
    </SettingSection>

    <ConfirmDialog
      :open="pendingRemoval !== null"
      :title="t('settings.plugins.lx.remove_title')"
      :message="pendingRemoval ? t('settings.plugins.lx.remove_confirm', { name: pendingRemoval.name }) : ''"
      :confirm-label="t('settings.plugins.lx.remove')"
      danger
      @update:open="value => { if (!value) pendingRemoval = null }"
      @confirm="confirmRemoval"
      @cancel="pendingRemoval = null"
    />
  </div>
</template>
