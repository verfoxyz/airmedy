import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { Events } from '@wailsio/runtime'
import * as LibraryService from '../../bindings/airmedy/internal/infra/wails/libraryservice'
import type { SyncProgress } from '../../bindings/airmedy/internal/domain/models'

/**
 * Library sync / optimize state.
 *
 * The dialog is global (App.vue) rather than owned by the settings page, so
 * switching pages cannot leave a teleported modal behind or lose `syncType`.
 * The store owns the event subscriptions and the 1.5s completion dismissal;
 * `runOptimize` / `runSync` also settle in `finally`, so a lost `sync-finished`
 * event cannot leave the UI stuck.
 */
export type LibrarySyncType = 'sync' | 'optimize' | 'deleting'

const COMPLETE_HIDE_MS = 1500

export const useLibrarySyncStore = defineStore('librarySync', () => {
  const isSyncing = ref(false)
  const type = ref<LibrarySyncType | null>(null)
  const progress = ref<SyncProgress | null>(null)
  const complete = ref(false)
  /** True while an automatic sync runs: the button spins, the dialog stays hidden. */
  const backgroundSyncActive = ref(false)
  /** Folder deletion is not a sync event, but it uses the same global dialog. */
  const removingFolderIds = ref<string[]>([])
  /** Bumped on every sync-finished event, so pages can re-read derived state. */
  const finishedCount = ref(0)

  let offHandlers: Array<() => void> = []
  let hideTimer: ReturnType<typeof setTimeout> | null = null
  let initialized = false

  const visible = computed(
    () =>
      (isSyncing.value && !backgroundSyncActive.value) ||
      removingFolderIds.value.length > 0 ||
      complete.value,
  )

  const displayType = computed<LibrarySyncType>(() => {
    if (removingFolderIds.value.length > 0) return 'deleting'
    return type.value ?? 'sync'
  })

  function clearHideTimer() {
    if (hideTimer !== null) {
      clearTimeout(hideTimer)
      hideTimer = null
    }
  }

  function resetProgress() {
    progress.value = null
    complete.value = false
    type.value = null
  }

  function scheduleHide() {
    clearHideTimer()
    hideTimer = setTimeout(() => {
      hideTimer = null
      resetProgress()
    }, COMPLETE_HIDE_MS)
  }

  /** settleComplete closes the dialog even when the finish event was lost. */
  function settleComplete() {
    isSyncing.value = false
    backgroundSyncActive.value = false
    complete.value = true
    scheduleHide()
  }

  function onSyncStarted(event: Events.WailsEvent) {
    const data = (event.data ?? {}) as { background?: boolean; total?: number; path?: string }
    isSyncing.value = true
    backgroundSyncActive.value = Boolean(data.background)
    if (data.background) return

    complete.value = false
    // A user-triggered action set the real type before the event arrived; a
    // late `sync-started` must not turn "优化搜索" back into "同步音乐库".
    if (type.value === null) type.value = 'sync'
    progress.value = {
      current: 0,
      total: Number(data.total) || 0,
      path: typeof data.path === 'string' ? data.path : '',
    }
  }

  function onSyncProgress(event: Events.WailsEvent) {
    isSyncing.value = true
    backgroundSyncActive.value = false
    progress.value = event.data as SyncProgress
  }

  function onSyncFinished(event: Events.WailsEvent) {
    const data = (event.data ?? {}) as { background?: boolean }
    isSyncing.value = false
    backgroundSyncActive.value = false
    finishedCount.value++
    if (!data.background) {
      complete.value = true
      scheduleHide()
    }
  }

  /** init subscribes once; repeated calls are harmless. */
  function init() {
    if (initialized) return
    initialized = true
    offHandlers = [
      Events.On('library:sync-started', onSyncStarted),
      Events.On('library:sync-progress', onSyncProgress),
      Events.On('library:sync-finished', onSyncFinished),
    ]
  }

  function dispose() {
    for (const off of offHandlers) off()
    offHandlers = []
    initialized = false
    clearHideTimer()
    isSyncing.value = false
    backgroundSyncActive.value = false
    resetProgress()
  }

  /** runOptimize asks the backend to rebuild the search index. */
  async function runOptimize() {
    if (isSyncing.value) return
    clearHideTimer()
    type.value = 'optimize'
    complete.value = false
    progress.value = { current: 0, total: 0, path: '' }
    isSyncing.value = true
    backgroundSyncActive.value = false
    try {
      await LibraryService.ReindexAll()
      settleComplete()
    } catch (error) {
      console.error('Optimization failed:', error)
      isSyncing.value = false
      backgroundSyncActive.value = false
      resetProgress()
    }
  }

  /** runSync asks the backend for a full library sync. */
  async function runSync() {
    if (isSyncing.value) return
    clearHideTimer()
    type.value = 'sync'
    complete.value = false
    progress.value = { current: 0, total: 0, path: '' }
    isSyncing.value = true
    backgroundSyncActive.value = false
    try {
      await LibraryService.SyncAll()
      settleComplete()
    } catch (error) {
      console.error('Sync failed:', error)
      isSyncing.value = false
      backgroundSyncActive.value = false
      resetProgress()
    }
  }

  function addRemovingFolder(id: string) {
    if (!removingFolderIds.value.includes(id)) {
      removingFolderIds.value = [...removingFolderIds.value, id]
    }
  }

  function removeRemovingFolder(id: string) {
    removingFolderIds.value = removingFolderIds.value.filter((folderID) => folderID !== id)
  }

  return {
    isSyncing,
    type,
    progress,
    complete,
    backgroundSyncActive,
    removingFolderIds,
    finishedCount,
    visible,
    displayType,
    init,
    dispose,
    runOptimize,
    runSync,
    addRemovingFolder,
    removeRemovingFolder,
  }
})
