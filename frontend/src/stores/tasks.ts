import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { Events } from '@wailsio/runtime'
import * as DownloadService from '../../bindings/airmedy/internal/infra/wails/downloadservice'
import type { TaskRun } from '../../bindings/airmedy/internal/domain/models'
import type { DownloadJobState, DownloadJobView } from './download'

/**
 * One row of the tasks page: a media request or a stream-cache session, already
 * normalised by the backend into TaskRun. It keeps the DownloadJobView shape so
 * the page can reuse the queue row's visuals; the fields a request or cache
 * session does not have (bytes, attempts) stay zero.
 */
export interface TaskRunView extends DownloadJobView {
  kind: 'request' | 'cache'
  /** MediaRequestPhase* while the request runs; empty otherwise. */
  phase: string
  createdAt: string
  /** Stable media request key; survives the merge that replaces trackId. */
  requestId: string
  provider: string
  externalId: string
}

export type TaskTab = 'all' | 'active' | 'done' | 'failed'

/**
 * A cache session canceled by a track switch, shutdown or user stop is history,
 * not a failure (nothing went wrong), so it belongs to "done". A canceled
 * download request still needs the user's attention and stays in "failed".
 */
export function taskInTab(task: TaskRunView, tab: TaskTab): boolean {
  switch (tab) {
    case 'all':
      return true
    case 'active':
      return task.state === 'active'
    case 'done':
      return task.state === 'done' || (task.kind === 'cache' && task.state === 'canceled')
    case 'failed':
      return task.state === 'failed' || (task.kind !== 'cache' && task.state === 'canceled')
  }
}

const TASK_KINDS: TaskRunView['kind'][] = ['request', 'cache']

function taskKind(kind: string | undefined): TaskRunView['kind'] {
  return TASK_KINDS.includes(kind as TaskRunView['kind'])
    ? (kind as TaskRunView['kind'])
    : 'request'
}

function fromRun(run: TaskRun): TaskRunView {
  const kind = taskKind(run.kind)
  return {
    // A request's stable key is its request id: a merge replaces track_id, and
    // keying by it would leave the old row behind beside the new one.
    jobId: run.request_id || run.id,
    // Request rows never show a bare track id; the snapshot/title is the label.
    trackId: kind === 'cache' ? (run.track_id ?? '') : '',
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
    kind,
    phase: run.phase ?? '',
    createdAt: run.created_at ?? '',
    requestId: run.request_id ?? '',
    provider: run.provider ?? '',
    externalId: run.external_id ?? '',
  }
}

export const useTasksStore = defineStore('tasks', () => {
  const tasks = ref<TaskRunView[]>([])
  const loading = ref(false)
  let _hydrating: Promise<void> | null = null

  const byTab = computed<Record<TaskTab, TaskRunView[]>>(() => ({
    all: tasks.value,
    active: tasks.value.filter(task => taskInTab(task, 'active')),
    done: tasks.value.filter(task => taskInTab(task, 'done')),
    failed: tasks.value.filter(task => taskInTab(task, 'failed')),
  }))

  const counts = computed(() => ({
    all: byTab.value.all.length,
    active: byTab.value.active.length,
    done: byTab.value.done.length,
    failed: byTab.value.failed.length,
  }))

  /** hydrate loads requests and cache sessions; a second call while one is in flight joins it. */
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

  async function cancel(taskId: string): Promise<void> {
    await DownloadService.CancelTask(taskId)
    await hydrate()
  }

  async function retry(taskId: string): Promise<void> {
    await DownloadService.RetryTask(taskId)
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
