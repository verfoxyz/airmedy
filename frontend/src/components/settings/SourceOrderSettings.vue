<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ArrowDown, ArrowUp, GripVertical, Loader2, Search } from '@lucide/vue'
import { Switch } from '@airmedy/ui'
import * as CatalogService from '../../../bindings/airmedy/internal/infra/wails/catalogservice'
import SettingSection from './SettingSection.vue'

/**
 * Search sources: order and per-source enable flags.
 *
 * The order decides tie-breaks between equally relevant rows, the order
 * sources are asked and the source chip row. A disabled source keeps its
 * position, so switching it back on restores where the user had arranged it;
 * disabling only affects new searches, never a running download or stream.
 */
const { t } = useI18n()

interface SourceRow {
  id: string
  name: string
}

const sources = ref<SourceRow[]>([])
const order = ref<string[]>([])
const disabled = ref<string[]>([])
const loading = ref(false)
const saving = ref(false)
const saveFailed = ref(false)
const draggingId = ref('')
const dragOverId = ref('')

const rows = computed(() =>
  order.value
    .map((id) => sources.value.find((source) => source.id === id))
    .filter((source): source is SourceRow => !!source),
)
const disabledSet = computed(() => new Set(disabled.value))
const allDisabled = computed(() => sources.value.length > 0 && disabled.value.length >= sources.value.length)

function nameFor(id: string): string {
  return sources.value.find((source) => source.id === id)?.name || id
}

async function load() {
  loading.value = true
  saveFailed.value = false
  try {
    const preferences = await CatalogService.GetSourcePreferences()
    sources.value = (preferences?.sources ?? []) as SourceRow[]
    order.value = (preferences?.order ?? []) as string[]
    disabled.value = (preferences?.disabled ?? []) as string[]
  } catch (error) {
    console.error('Failed to load the search source preferences', error)
    saveFailed.value = true
  } finally {
    loading.value = false
  }
}

function fail(error: unknown, previousOrder: string[], previousDisabled: string[]) {
  order.value = previousOrder
  disabled.value = previousDisabled
  saveFailed.value = true
  console.error('Failed to save the search source preferences', error)
}

/** persistOrder writes one reorder; drag and the up/down buttons share it. */
async function persistOrder(next: string[]) {
  if (saving.value) return
  const previousOrder = [...order.value]
  const previousDisabled = [...disabled.value]
  order.value = next
  saving.value = true
  saveFailed.value = false
  try {
    const saved = (await CatalogService.SaveSourceOrder(next)) as string[]
    if (Array.isArray(saved) && saved.length > 0) order.value = saved
  } catch (error) {
    fail(error, previousOrder, previousDisabled)
  } finally {
    saving.value = false
  }
}

async function move(id: string, delta: number) {
  const index = order.value.indexOf(id)
  const target = index + delta
  if (index < 0 || target < 0 || target >= order.value.length) return
  const next = [...order.value]
  ;[next[index], next[target]] = [next[target], next[index]]
  await persistOrder(next)
}

async function setEnabled(id: string, enabled: boolean) {
  if (saving.value) return
  const previousOrder = [...order.value]
  const previousDisabled = [...disabled.value]
  disabled.value = enabled
    ? disabled.value.filter((provider) => provider !== id)
    : [...disabled.value.filter((provider) => provider !== id), id]
  saving.value = true
  saveFailed.value = false
  try {
    const saved = (await CatalogService.SetSourceDisabled(id, !enabled)) as string[]
    if (Array.isArray(saved)) disabled.value = saved
  } catch (error) {
    fail(error, previousOrder, previousDisabled)
  } finally {
    saving.value = false
  }
}

function onDragStart(event: DragEvent, id: string) {
  draggingId.value = id
  dragOverId.value = id
  if (event.dataTransfer) {
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('text/plain', id)
  }
}

function onDragOver(event: DragEvent, id: string) {
  if (!draggingId.value) return
  // preventDefault is what makes the row a valid drop target.
  event.preventDefault()
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'
  if (draggingId.value !== id) dragOverId.value = id
}

function clearDrag() {
  draggingId.value = ''
  dragOverId.value = ''
}

async function onDrop(event: DragEvent, targetId: string) {
  event.preventDefault()
  const sourceId = draggingId.value || event.dataTransfer?.getData('text/plain') || ''
  clearDrag()
  if (!sourceId || sourceId === targetId) return
  const from = order.value.indexOf(sourceId)
  const to = order.value.indexOf(targetId)
  if (from < 0 || to < 0) return
  const next = [...order.value]
  next.splice(from, 1)
  next.splice(to, 0, sourceId)
  await persistOrder(next)
}

onMounted(load)
</script>

<template>
  <SettingSection :icon="Search" :label="$t('settings.source_order')" variant="panel">
    <p class="mb-1 text-sm text-dim">{{ $t('settings.source_order_hint') }}</p>
    <p class="mb-4 text-xs text-dimmer">{{ $t('settings.source_drag_hint') }}</p>

    <div
      v-if="loading && rows.length === 0"
      class="flex items-center justify-center py-8 text-dim"
      data-test="source-order-loading"
    >
      <Loader2 class="h-4 w-4 animate-spin" aria-hidden="true" />
    </div>

    <div
      v-else
      class="divide-y divide-foreground/[0.06] rounded-xl border border-foreground/[0.06]"
      data-test="source-order-list"
    >
      <div
        v-for="(row, index) in rows"
        :key="row.id"
        class="flex items-center gap-3 px-4 py-2.5 transition-colors"
        :class="[
          disabledSet.has(row.id) ? 'opacity-50' : '',
          dragOverId === row.id && draggingId !== row.id ? 'bg-foreground/[0.06]' : '',
        ]"
        data-test="source-order-row"
        :data-source="row.id"
        :data-enabled="!disabledSet.has(row.id)"
        @dragover="onDragOver($event, row.id)"
        @drop="onDrop($event, row.id)"
      >
        <button
          type="button"
          draggable="true"
          class="flex h-8 w-6 shrink-0 cursor-grab items-center justify-center text-dimmer active:cursor-grabbing"
          data-test="source-order-handle"
          :aria-label="$t('settings.source_drag_hint')"
          :title="$t('settings.source_drag_hint')"
          @dragstart="onDragStart($event, row.id)"
          @dragend="clearDrag"
        >
          <GripVertical class="h-4 w-4" aria-hidden="true" />
        </button>
        <span class="w-6 shrink-0 text-xs tabular-nums text-dimmer">{{ index + 1 }}</span>
        <span class="min-w-0 flex-1 truncate text-sm text-foreground" :title="nameFor(row.id)">
          {{ row.name }}
        </span>
        <span class="shrink-0 text-xs text-dim" data-test="source-order-state">
          {{ disabledSet.has(row.id) ? $t('settings.source_disabled') : $t('settings.source_enabled') }}
        </span>
        <button
          type="button"
          class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-dim transition-colors hover:bg-foreground/[0.04] hover:text-foreground disabled:pointer-events-none disabled:opacity-30"
          data-test="source-order-up"
          :aria-label="$t('settings.move_up')"
          :title="$t('settings.move_up')"
          :disabled="index === 0 || saving"
          @click="move(row.id, -1)"
        >
          <ArrowUp class="h-4 w-4" aria-hidden="true" />
        </button>
        <button
          type="button"
          class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-dim transition-colors hover:bg-foreground/[0.04] hover:text-foreground disabled:pointer-events-none disabled:opacity-30"
          data-test="source-order-down"
          :aria-label="$t('settings.move_down')"
          :title="$t('settings.move_down')"
          :disabled="index === rows.length - 1 || saving"
          @click="move(row.id, 1)"
        >
          <ArrowDown class="h-4 w-4" aria-hidden="true" />
        </button>
        <Switch
          class="shrink-0"
          data-test="source-order-toggle"
          :model-value="!disabledSet.has(row.id)"
          :disabled="saving"
          :aria-label="disabledSet.has(row.id) ? $t('settings.source_disabled') : $t('settings.source_enabled')"
          @update:model-value="(value) => setEnabled(row.id, Boolean(value))"
        />
      </div>
    </div>

    <p
      v-if="allDisabled"
      class="mt-3 text-sm text-dim"
      data-test="source-order-all-disabled"
    >
      {{ $t('settings.all_sources_disabled') }}
    </p>

    <p
      v-if="saveFailed"
      class="mt-3 text-sm text-dim"
      role="alert"
      data-test="source-order-error"
    >
      {{ $t('settings.source_save_failed') }}
    </p>
  </SettingSection>
</template>
