<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Download, X } from '@lucide/vue'
import { DynamicScroller, DynamicScrollerItem } from 'vue-virtual-scroller'
import { useDownloadStore } from '@/stores/download'
import DownloadQueueItem from '@/components/DownloadQueueItem.vue'

/**
 * The download queue panel: a right-hand slide-over listing every job the
 * backend still has (queued / active / failed / canceled; `done` rows are
 * dropped by the store, because a finished track is already in the library).
 *
 * Two things the panel owns:
 *  - `hydrate()` on open, so a queue that changed while the app was closed or
 *    while the panel was shut shows up without restarting.
 *  - the in-flight set for cancel/retry, so a row cannot be double-submitted.
 */
const props = defineProps<{
  open: boolean
}>()

const emit = defineEmits<{
  close: []
}>()

const { t } = useI18n()
const downloadStore = useDownloadStore()

const busyJobIds = ref<Set<string>>(new Set())

const jobs = computed(() => downloadStore.pendingJobs)

type QueueRow = (typeof jobs.value)[number] & { rowKey: string }

// vue-virtual-scroller needs a stable key; a retry reuses the track but gets a
// new job id, so the job id is the key and not the track.
const rows = computed<QueueRow[]>(() => jobs.value.map(job => ({ ...job, rowKey: job.jobId })))

function markBusy(jobId: string, busy: boolean) {
  const next = new Set(busyJobIds.value)
  if (busy) next.add(jobId)
  else next.delete(jobId)
  busyJobIds.value = next
}

async function cancel(jobId: string) {
  if (busyJobIds.value.has(jobId)) return
  markBusy(jobId, true)
  try {
    await downloadStore.cancel(jobId)
  } catch (error) {
    console.error('Failed to cancel download:', error)
  } finally {
    markBusy(jobId, false)
  }
}

async function retry(jobId: string) {
  if (busyJobIds.value.has(jobId)) return
  markBusy(jobId, true)
  try {
    await downloadStore.retry(jobId)
  } catch (error) {
    console.error('Failed to retry download:', error)
  } finally {
    markBusy(jobId, false)
  }
}

// Escape closes the panel; the listener only exists while it is open.
function onKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') emit('close')
}

function bindKeydown(open: boolean) {
  window.removeEventListener('keydown', onKeydown)
  if (open) window.addEventListener('keydown', onKeydown)
}

// Opening the panel refreshes the queue: the app may have been running with it
// closed while jobs finished, failed or were queued from another window.
watch(
  () => props.open,
  (open) => {
    bindKeydown(open)
    if (open) void downloadStore.hydrate()
  },
  { immediate: true },
)

onMounted(() => bindKeydown(props.open))

onUnmounted(() => {
  window.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <Transition name="download-panel">
    <aside
      v-if="open"
      class="fixed right-0 top-0 z-40 flex h-full w-[360px] max-w-full flex-col rounded-l-2xl border-l border-border-glass bg-glass shadow-2xl backdrop-blur-3xl isolate"
      role="complementary"
      :aria-label="t('download.queue_title')"
      data-test="download-queue-panel">
      <header class="flex items-center justify-between gap-2 border-b border-border-glass px-4 py-3">
        <div class="flex min-w-0 items-center gap-2">
          <Download class="h-4 w-4 shrink-0 text-primary" />
          <span class="truncate text-sm font-semibold">{{ t('download.queue_title') }}</span>
          <span v-if="rows.length" class="shrink-0 text-xs text-[color:var(--text-muted)]">({{ rows.length }})</span>
        </div>
        <button
          type="button"
          class="rounded-full p-1 text-dim transition-colors hover:bg-foreground/[0.06] hover:text-foreground"
          :title="t('common.close')"
          :aria-label="t('common.close')"
          data-test="download-queue-close"
          @click="emit('close')">
          <X class="h-4 w-4" />
        </button>
      </header>

      <div v-if="rows.length === 0" class="flex flex-1 flex-col items-center justify-center gap-2 px-8 text-center"
        data-test="download-queue-empty">
        <Download class="h-8 w-8 text-dimmer" />
        <p class="text-sm text-dim">{{ t('download.queue_empty') }}</p>
        <p class="text-xs text-dimmer">{{ t('download.queue_empty_hint') }}</p>
      </div>

      <DynamicScroller
        v-else
        :items="rows"
        key-field="rowKey"
        :min-item-size="72"
        class="custom-scrollbar flex-1 px-2 py-2">
        <template #default="{ item, index, active }">
          <DynamicScrollerItem :item="item" :active="active" :data-index="index" :size-dependencies="[item.state, item.percent]">
            <DownloadQueueItem
              :job="item"
              :busy="busyJobIds.has(item.jobId)"
              @cancel="cancel"
              @retry="retry" />
          </DynamicScrollerItem>
        </template>
      </DynamicScroller>
    </aside>
  </Transition>
</template>

<style scoped>
.download-panel-enter-active,
.download-panel-leave-active {
  transition: all 0.3s cubic-bezier(0.4, 0, 0.2, 1);
}

.download-panel-enter-from,
.download-panel-leave-to {
  transform: translateX(100%);
  opacity: 0;
}
</style>
