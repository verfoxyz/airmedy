import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { Events } from '@wailsio/runtime'
import * as DownloadService from '../../bindings/airmedy/internal/infra/wails/downloadservice'
import type { TaskRun } from '../../bindings/airmedy/internal/domain/models'
import type { DownloadJobState, DownloadJobView } from './download'

/**
 * One row of the tasks page: a download or a promotion, already normalised by
 * the backend into TaskRun. It keeps the DownloadJobView shape so the page can
 * reuse the queue row's visuals; the fields a promotion does not have (bytes,
 * attempts) stay zero.
 */
export interface TaskRunView extends DownloadJobView {
  kind: 'download' | 'promote'
  createdAt: string
}

export type TaskTab = 'all' | 'active' | 'done' | 'failed'

const TAB_STATE: Record<TaskTab, (state: DownloadJobState) => boolean> = {
  all: () => true,
  active: state => state === 'active',
  done: state => state === 'done',
  failed: state => state === 'failed' || state === 'canceled',
}

function fromRun(run: TaskRun): TaskRunView {
  return {
    jobId: run.id,
    trackId: run.track_id ?? '',
    playlistId: '',
    state: (run.state || 'active') as DownloadJobState,
    bytesDone: 0,
    bytesTotal: 0,
    percent: 0,
    error: run.error ?? '',
    quality: run.quality ?? '',
    attempts: 0,
    maxAttempts: 0,
    title: run.title ?? '',
    artist: run.artist ?? '',
    kind: run.kind === 'promote' ? 'promote' : 'download',
    createdAt: run.created_at ?? '',
  }
}

export const useTasksStore = defineStore('tasks', () => {
  const tasks = ref<TaskRunView[]>([])
  const loading = ref(false)
  let _hydrating: Promise<void> | null = null

  const byTab = computed<Record<TaskTab, TaskRunView[]>>(() => ({
    all: tasks.value,
    active: tasks.value.filter(task => TAB_STATE.active(task.state)),
    done: tasks.value.filter(task => TAB_STATE.done(task.state)),
    failed: tasks.value.filter(task => TAB_STATE.failed(task.state)),
  }))

  const counts = computed(() => ({
    all: byTab.value.all.length,
    active: byTab.value.active.length,
    done: byTab.value.done.length,
    failed: byTab.value.failed.length,
  }))

  /** hydrate loads both tables merged; a second call while one is in flight joins it. */
  async function hydrate(): Promise<void> {
    if (_hydrating) return _hydrating
    _hydrating = (async () => {
      loading.value = true
      try {
        const listed = ((await DownloadService.ListTasks()) ?? []) as TaskRun[]
        tasks.value = listed.filter(Boolean).map(fromRun)
      } catch (error) {
        console.error('Failed to load the task list:', error)
      } finally {
        loading.value = false
        _hydrating = null
      }
    })()
    return _hydrating
  }

  /** clearHistory deletes finished runs; the backend emits task:updated. */
  async function clearHistory(): Promise<void> {
    await DownloadService.ClearTaskHistory()
    await hydrate()
  }

  async function cancel(jobId: string): Promise<void> {
    await DownloadService.CancelDownload(jobId)
    await hydrate()
  }

  async function retry(jobId: string): Promise<void> {
    await DownloadService.RetryDownload(jobId)
    await hydrate()
  }

  const _offTask = Events.On('task:updated', () => {
    void hydrate()
  })

  function dispose() {
    _offTask()
  }

  return {
    tasks,
    byTab,
    counts,
    loading,
    hydrate,
    clearHistory,
    cancel,
    retry,
    dispose,
  }
})
