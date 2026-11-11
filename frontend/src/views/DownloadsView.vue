<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Download, Trash2 } from '@lucide/vue'
import { DynamicScroller, DynamicScrollerItem } from 'vue-virtual-scroller'
import ViewHeader from '../components/ViewHeader.vue'
import DownloadQueueItem from '../components/DownloadQueueItem.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import { useTasksStore, type TaskTab } from '../stores/tasks'

const { t } = useI18n()
const tasksStore = useTasksStore()

const activeTab = ref<TaskTab>('all')
const clearOpen = ref(false)
const busyJobIds = ref<Set<string>>(new Set())

const tabs = computed(() => [
  { id: 'all' as TaskTab, label: t('tasks.tab_all'), count: tasksStore.counts.all },
  { id: 'active' as TaskTab, label: t('tasks.tab_active'), count: tasksStore.counts.active },
  { id: 'done' as TaskTab, label: t('tasks.tab_done'), count: tasksStore.counts.done },
  { id: 'failed' as TaskTab, label: t('tasks.tab_failed'), count: tasksStore.counts.failed },
])

const rows = computed(() =>
  tasksStore.byTab[activeTab.value].map(row => ({ ...row, rowKey: row.jobId })),
)

const hasHistory = computed(() => tasksStore.counts.done + tasksStore.counts.failed > 0)

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
    await tasksStore.cancel(jobId)
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
    await tasksStore.retry(jobId)
  } catch (error) {
    console.error('Failed to retry download:', error)
  } finally {
    markBusy(jobId, false)
  }
}

async function clearHistory() {
  try {
    await tasksStore.clearHistory()
  } catch (error) {
    console.error('Failed to clear the task history:', error)
  }
}

// The page is the tasks' only home: hydrate on entry so runs created while the
// user was elsewhere (or before a restart) are listed without waiting for an event.
onMounted(() => {
  void tasksStore.hydrate()
})
</script>

<template>
  <div class="flex h-full flex-col overflow-hidden bg-background">
    <ViewHeader :title="t('tasks.title')">
      <template #actions>
        <button
          v-if="hasHistory"
          type="button"
          class="flex items-center gap-1.5 rounded-full px-3 py-1.5 text-xs font-medium text-dim transition-colors hover:bg-foreground/[0.06] hover:text-foreground"
          data-test="tasks-clear"
          @click="clearOpen = true">
          <Trash2 class="h-3.5 w-3.5" />
          {{ t('tasks.clear') }}
        </button>
      </template>
    </ViewHeader>

    <div class="flex shrink-0 items-center gap-1 border-b border-foreground/[0.06] px-4">
      <button
        v-for="tab in tabs"
        :key="tab.id"
        type="button"
        class="relative px-3 py-2.5 text-sm font-medium transition-colors"
        :class="activeTab === tab.id ? 'text-foreground' : 'text-subdued hover:text-foreground'"
        :data-test="`tasks-tab-${tab.id}`"
        @click="activeTab = tab.id">
        {{ tab.label }}
        <span class="ml-1 text-xs tabular-nums text-dimmer">{{ tab.count }}</span>
        <span
          v-if="activeTab === tab.id"
          class="absolute inset-x-2 -bottom-px h-px bg-foreground/40" />
      </button>
    </div>

    <div
      v-if="rows.length === 0"
      class="flex flex-1 flex-col items-center justify-center gap-2 text-dim"
      data-test="tasks-empty">
      <Download class="h-12 w-12 opacity-20" />
      <p class="text-sm">{{ t(`tasks.empty_${activeTab}`) }}</p>
    </div>

    <DynamicScroller
      v-else
      :items="rows"
      key-field="rowKey"
      :min-item-size="72"
      class="custom-scrollbar flex-1 px-6 py-4"
      data-test="tasks-list">
      <template #default="{ item, index, active }">
        <DynamicScrollerItem :item="item" :active="active" :data-index="index" :size-dependencies="[item.state, item.percent]">
          <DownloadQueueItem
            :job="item"
            :kind="item.kind"
            :busy="busyJobIds.has(item.jobId)"
            @cancel="cancel"
            @retry="retry" />
        </DynamicScrollerItem>
      </template>
    </DynamicScroller>

    <ConfirmDialog
      v-model:open="clearOpen"
      :title="t('tasks.clear_title')"
      :message="t('tasks.clear_message')"
      :confirm-label="t('tasks.clear_confirm')"
      :cancel-label="t('tasks.clear_cancel')"
      danger
      @cancel="clearOpen = false"
      @confirm="clearHistory" />
  </div>
</template>
