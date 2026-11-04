<script setup lang="ts">
import { computed } from 'vue'
import { ArrowUp, ArrowDown, Trash2 } from '@lucide/vue'
import { Switch } from '@airmedy/ui'
import type { LxSource, LxSourceRowLabels } from './lxSourceTypes'

/**
 * One row of the lx source list: order handles, the label, the platforms the
 * source registered, its switch and its delete button.
 *
 * Ordering uses up/down buttons rather than drag-and-drop. The app's only
 * sortable dependency (`vue-virtual-sortable`) is a virtual-list wrapper for
 * track tables; pulling it in here would add a virtualisation layer the list does
 * not need, and the order handles must work for a user on a trackpad or with a
 * screen reader anyway. The task explicitly allows either.
 *
 * Labels arrive translated, so this component is presentation only.
 */
const props = defineProps<{
  source: LxSource
  index: number
  total: number
  busy?: boolean
  labels: LxSourceRowLabels
}>()

const emit = defineEmits<{
  toggle: [enabled: boolean]
  remove: []
  move: [direction: -1 | 1]
}>()

const canMoveUp = computed(() => props.index > 0 && !props.busy)
const canMoveDown = computed(() => props.index < props.total - 1 && !props.busy)

const statusText = computed(() => {
  if (props.source.error) return props.labels.failed
  if (!props.source.enabled) return props.labels.disable
  return props.labels.enable
})
</script>

<template>
  <div class="p-4" :data-test="`lx-source-row-${source.id}`" :data-source-id="source.id">
    <div class="flex items-start justify-between gap-4">
      <div class="flex items-center gap-3 min-w-0 flex-1">
        <!-- Order handles. Buttons, not a drag library: the list is short and the
             controls double as keyboard-accessible reordering. -->
        <div class="flex flex-col gap-0.5 shrink-0">
          <button
            class="p-1 rounded-md text-foreground opacity-40 hover:opacity-80 hover:bg-foreground/[0.04] transition-all disabled:opacity-20 disabled:hover:bg-transparent"
            :disabled="!canMoveUp"
            :aria-label="labels.moveUp"
            :data-test="`lx-source-up-${source.id}`"
            @click="emit('move', -1)"
          >
            <ArrowUp class="w-3.5 h-3.5" />
          </button>
          <button
            class="p-1 rounded-md text-foreground opacity-40 hover:opacity-80 hover:bg-foreground/[0.04] transition-all disabled:opacity-20 disabled:hover:bg-transparent"
            :disabled="!canMoveDown"
            :aria-label="labels.moveDown"
            :data-test="`lx-source-down-${source.id}`"
            @click="emit('move', 1)"
          >
            <ArrowDown class="w-3.5 h-3.5" />
          </button>
        </div>

        <div class="min-w-0 flex-1">
          <div class="flex items-center gap-2 flex-wrap">
            <span class="text-sm font-semibold truncate" data-test="lx-source-name">{{ source.name }}</span>
            <span
              class="text-[11px] text-dim"
              :data-test="`lx-source-status-${source.id}`"
            >{{ statusText }}</span>
          </div>

          <div
            v-if="source.providers && source.providers.length > 0"
            class="flex items-center gap-1.5 flex-wrap mt-1.5"
            data-test="lx-source-providers"
          >
            <span
              v-for="provider in source.providers"
              :key="provider"
              class="px-2 py-0.5 rounded-md border border-[var(--border-glass)] text-[11px] text-dim"
            >{{ provider }}</span>
          </div>
          <p v-else class="mt-1.5 text-[11px] text-dim" data-test="lx-source-no-providers">
            {{ source.error || labels.noProviders }}
          </p>
        </div>
      </div>

      <div class="flex items-center gap-2 shrink-0">
        <button
          class="p-2 rounded-lg text-foreground opacity-40 hover:opacity-80 hover:bg-foreground/[0.04] transition-all"
          :disabled="busy"
          :aria-label="labels.remove"
          :data-test="`lx-source-remove-${source.id}`"
          @click="emit('remove')"
        >
          <Trash2 class="w-4 h-4" />
        </button>
        <Switch
          :model-value="source.enabled"
          :disabled="busy"
          :data-test="`lx-source-toggle-${source.id}`"
          @update:model-value="(value: boolean) => emit('toggle', value)"
        />
      </div>
    </div>
  </div>
</template>
