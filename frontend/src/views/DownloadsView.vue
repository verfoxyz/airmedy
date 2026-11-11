<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Download } from '@lucide/vue'
import { DynamicScroller, DynamicScrollerItem } from 'vue-virtual-scroller'
import ViewHeader from '../components/ViewHeader.vue'
import DownloadQueueItem from '../components/DownloadQueueItem.vue'
import { useDownloadStore } from '../stores/download'

const { t } = useI18n()
const downloadStore = useDownloadStore()

const busyJobIds = ref<Set<string>>(new Set())

const rows = computed(() => downloadStore.pendingJobs.map(job => ({ ...job, rowKey: job.jobId })))

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

// The page is the queue's only home: hydrate on entry so jobs queued while the
// user was elsewhere (or before a restart) are listed without waiting for an event.
onMounted(() => {
  void downloadStore.hydrate()
})
</script>

<template>
  <div class="flex h-full flex-col overflow-hidden bg-background">
    <ViewHeader :title="t('download.queue_title')">
      <template #actions>
        <span v-if="rows.length" class="text-sm text-dim" data-test="downloads-count">{{ rows.length }}</span>
      </template>
    </ViewHeader>

    <div
      v-if="rows.length === 0"
      class="flex flex-1 flex-col items-center justify-center gap-2 text-dim"
      data-test="downloads-empty">
      <Download class="h-12 w-12 opacity-20" />
      <p class="text-sm">{{ t('download.queue_empty') }}</p>
      <p class="text-xs text-dimmer">{{ t('download.queue_empty_hint') }}</p>
    </div>

    <DynamicScroller
      v-else
      :items="rows"
      key-field="rowKey"
      :min-item-size="72"
      class="custom-scrollbar flex-1 px-6 py-4"
      data-test="downloads-list">
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
  </div>
</template>
