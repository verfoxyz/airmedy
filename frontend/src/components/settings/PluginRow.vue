<script setup lang="ts">
import { computed, ref } from 'vue'
import { ChevronRight, Globe, HardDrive } from '@lucide/vue'
import { Switch } from '@airmedy/ui'
import type { PluginInfo, PluginRowLabels } from './pluginTypes'

/**
 * One plugin row: the switch, the status dot, the tags and the error disclosure.
 *
 * A plugin is a leaf: it has its own process and its own switch, with nothing
 * nested under it. The row therefore has no "nested" mode — every row behaves
 * identically, which is exactly what the flat model is for.
 *
 * Labels are passed in already translated, so this component is presentation
 * only and its test does not need an i18n instance.
 */
const props = defineProps<{
  plugin: PluginInfo
  busy?: boolean
  labels: PluginRowLabels
  /**
   * Whether this plugin manages its own sub-list. True only for the Lx plugin:
   * it owns a user-editable source list, and its row opens that page instead of
   * being a leaf. Every other plugin stays flat.
   */
  manageSources?: boolean
}>()

const emit = defineEmits<{
  toggle: [enabled: boolean]
  open: []
}>()

const expanded = ref(false)

const statusTone = computed<'running' | 'idle' | 'failed'>(() => {
  if (props.plugin.invalid) return 'failed'
  switch (props.plugin.status) {
    case 'running': return 'running'
    case 'failed': return 'failed'
    default: return 'idle'
  }
})

const statusText = computed(() => {
  if (props.plugin.invalid) return props.labels.invalid
  switch (props.plugin.status) {
    case 'running': return props.labels.running
    case 'failed': return props.labels.failed
    default: return props.labels.idle
  }
})

const canToggle = computed(() => !props.plugin.invalid && !props.busy)

const dotClass = computed(() => {
  switch (statusTone.value) {
    case 'running': return 'bg-primary'
    case 'failed': return 'bg-foreground opacity-40'
    default: return 'bg-foreground opacity-20'
  }
})

/**
 * The manifest's network declaration.
 *
 * This is a declaration, not a live indicator: the host does not sandbox plugin
 * traffic, so a globe that changed with connection state would promise
 * enforcement the app cannot deliver. It stays on the row while the plugin is
 * disabled, because "what will this reach?" is exactly the question the user
 * asks before flipping the switch.
 */
const declaredHosts = computed(() => (props.plugin.network ?? []).filter(host => host !== '*'))

const reachesAnyHost = computed(() => (props.plugin.network ?? []).includes('*'))

const showsNetwork = computed(() => (props.plugin.network ?? []).length > 0)

const networkText = computed(() => {
  if (!showsNetwork.value) return ''
  if (reachesAnyHost.value) {
    // `*` covers everything, so naming hosts next to it would understate the
    // manifest rather than describe it.
    return `${props.labels.networkLabel} · ${props.labels.networkAny}`
  }
  const hosts = declaredHosts.value.join(', ')
  return `${props.labels.networkLabel} · ${props.labels.networkHosts.replace('{hosts}', hosts)}`
})

// Inactive rows recede by opacity, not by colour, so the badge keeps the same
// meaning in every theme. A disabled plugin's declaration is dimmer but still
// legible — it is exactly what the user is deciding about.
const networkClass = computed(() => (props.plugin.enabled ? 'opacity-100' : 'opacity-40'))
</script>

<template>
  <div
    class="p-5"
    :data-test="`plugin-row-${plugin.id}`"
    :data-plugin-id="plugin.id"
  >
    <div class="flex items-start justify-between gap-4">
      <div class="min-w-0 flex-1">
        <div class="flex items-center gap-2 flex-wrap">
          <button
            v-if="manageSources"
            class="flex items-center gap-1 min-w-0 text-sm font-semibold hover:text-foreground transition-colors"
            :data-test="`plugin-manage-${plugin.id}`"
            @click="emit('open')"
          >
            <span class="truncate" data-test="plugin-name">{{ plugin.name }}</span>
            <ChevronRight class="w-4 h-4 shrink-0 opacity-60" />
          </button>
          <template v-else>
            <span class="text-sm font-semibold truncate" data-test="plugin-name">{{ plugin.name }}</span>
          </template>
          <span v-if="plugin.version" class="text-xs text-dim">v{{ plugin.version }}</span>
        </div>

        <div v-if="plugin.author" class="text-xs text-dim mt-1" data-test="plugin-author">
          {{ plugin.author }}
        </div>

        <div class="flex items-center gap-1.5 flex-wrap mt-2" data-test="plugin-capabilities">
          <span
            v-for="capability in plugin.capabilities"
            :key="capability"
            class="px-2 py-0.5 rounded-md bg-foreground/[0.04] text-[11px] text-dim"
          >{{ capability }}</span>
        </div>

        <!-- The routing declaration, shown so the user can tell which plugin
             will handle a given source. A plugin may legitimately serve none. -->
        <div
          v-if="plugin.providers && plugin.providers.length > 0"
          class="flex items-center gap-1.5 flex-wrap mt-1.5"
          data-test="plugin-providers"
        >
          <span
            v-for="provider in plugin.providers"
            :key="provider"
            class="px-2 py-0.5 rounded-md border border-[var(--border-glass)] text-[11px] text-dim"
          >{{ provider }}</span>
        </div>
      </div>

      <div class="flex items-center gap-3 shrink-0">
        <!-- The network declaration sits next to the live state, so the user
             reads "what it says it reaches" and "whether it is running" in one
             glance. It is never hidden by the enable state: seeing it before
             turning the plugin on is the whole point. -->
        <span
          v-if="plugin.kv"
          class="flex items-center text-dim transition-opacity"
          :class="networkClass"
          :title="labels.storageLabel"
          :aria-label="labels.storageLabel"
          :data-test="`plugin-storage-${plugin.id}`"
        >
          <HardDrive class="w-3.5 h-3.5 shrink-0" aria-hidden="true" />
        </span>
        <span
          v-if="showsNetwork"
          class="flex items-center text-dim transition-opacity"
          :class="networkClass"
          :title="networkText"
          :aria-label="networkText"
          :data-test="`plugin-network-${plugin.id}`"
          :data-network="(plugin.network ?? []).join(',')"
        >
          <Globe class="w-3.5 h-3.5 shrink-0" aria-hidden="true" />
        </span>
        <span class="flex items-center gap-1.5 text-xs text-dim" :data-test="`plugin-status-${plugin.id}`">
          <span class="w-1.5 h-1.5 rounded-full" :class="dotClass" />
          {{ statusText }}
        </span>
        <Switch
          :model-value="plugin.enabled"
          :disabled="!canToggle"
          :data-test="`plugin-toggle-${plugin.id}`"
          @update:model-value="(value: boolean) => emit('toggle', value)"
        />
      </div>
    </div>

    <button
      v-if="plugin.last_error"
      class="mt-2 text-xs text-dim hover:text-foreground transition-colors text-left"
      :data-test="`plugin-error-toggle-${plugin.id}`"
      @click="expanded = !expanded"
    >
      {{ expanded ? labels.hideError : labels.error }}
    </button>
    <pre
      v-if="expanded && plugin.last_error"
      class="mt-2 p-3 rounded-lg bg-foreground/[0.04] text-[11px] whitespace-pre-wrap break-words text-dim"
      :data-test="`plugin-error-${plugin.id}`"
    >{{ plugin.last_error }}</pre>
  </div>
</template>