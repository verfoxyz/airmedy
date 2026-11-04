import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { Events } from '@wailsio/runtime'
import * as DownloadService from '../../bindings/airmedy/internal/infra/wails/downloadservice'
import * as LibraryService from '../../bindings/airmedy/internal/infra/wails/libraryservice'
import type { DownloadJob } from '../../bindings/airmedy/internal/domain/models'

export type DownloadJobState = 'queued' | 'active' | 'done' | 'failed' | 'canceled'

/**
 * One download in flight or recently finished, as reported by the backend's
 * download:progress / download:done / download:failed events.
 *
 * `percent` is precomputed by the backend, but it stays 0 while the server has
 * not sent a Content-Length, so the UI must treat `bytesTotal === 0` as
 * "indeterminate" rather than "0% done".
 */
export interface DownloadState {
  jobId: string
  trackId: string
  playlistId: string
  state: DownloadJobState
  bytesDone: number
  bytesTotal: number
  percent: number
  error: string
}

/**
 * A queue row for the download panel: the job row plus the track title and
 * artist. Job rows only carry ids, so the panel resolves titles with one
 * batched GetTracksByIDs call instead of one request per row.
 */
export interface DownloadJobView extends DownloadState {
  quality: string
  attempts: number
  maxAttempts: number
  title: string
  artist: string
}

interface RawDownloadEvent {
  job_id: string
  track_id: string
  playlist_id?: string
  state: string
  bytes_done: number
  bytes_total: number
  percent: number
  error?: string
}

const EMPTY_JOBS: DownloadJobView[] = []

function percentOf(bytesDone: number, bytesTotal: number): number {
  if (bytesTotal <= 0) return 0
  return Math.min(100, Math.floor((bytesDone * 100) / bytesTotal))
}

function toState(raw: RawDownloadEvent): DownloadState {
  return {
    jobId: raw.job_id,
    trackId: raw.track_id,
    playlistId: raw.playlist_id ?? '',
    state: raw.state as DownloadJobState,
    bytesDone: raw.bytes_done ?? 0,
    bytesTotal: raw.bytes_total ?? 0,
    percent: raw.percent ?? 0,
    error: raw.error ?? '',
  }
}

function fromJob(job: DownloadJob): DownloadJobView {
  return {
    jobId: job.id,
    trackId: job.track_id,
    playlistId: job.playlist_id ?? '',
    state: job.state as DownloadJobState,
    bytesDone: job.bytes_done ?? 0,
    bytesTotal: job.bytes_total ?? 0,
    percent: percentOf(job.bytes_done ?? 0, job.bytes_total ?? 0),
    error: job.last_error ?? '',
    quality: job.quality ?? '',
    attempts: job.attempts ?? 0,
    maxAttempts: job.max_attempts ?? 0,
    title: '',
    artist: '',
  }
}

export const useDownloadStore = defineStore('download', () => {
  /**
   * Track id -> its current download. Keyed by track because the playlist row
   * asks "is this track still downloading?" while rendering. Only jobs that are
   * queued or active live here: done/canceled rows have nothing left to draw.
   */
  const byTrack = ref<Record<string, DownloadState>>({})

  /**
   * Every job the backend still reports, keyed by job id. This is the panel's
   * source of truth and is hydrated from ListDownloads at startup, so a restart
   * no longer loses the queue.
   */
  const jobs = ref<DownloadJobView[]>([])

  /**
   * Job ids touched by a live event since the last hydrate started. A progress
   * event is always newer than the snapshot ListDownloads returned, so hydrate
   * must not roll those rows back.
   */
  let _liveJobIds = new Set<string>()
  let _hydrating: Promise<void> | null = null
  /** Track ids with a title request in flight, and ids the library no longer has. */
  const _titleInFlight = new Set<string>()
  const _missingTracks = new Set<string>()

  const _offProgress = Events.On('download:progress', (event: Events.WailsEvent) => {
    upsert(event.data as RawDownloadEvent)
  })
  const _offDone = Events.On('download:done', (event: Events.WailsEvent) => {
    // A finished download leaves both views so the row renders normally again;
    // the backend has already flipped the track's availability to 'ready'.
    remove(event.data as RawDownloadEvent)
  })
  const _offFailed = Events.On('download:failed', (event: Events.WailsEvent) => {
    upsert(event.data as RawDownloadEvent)
  })

  /** Applies a state to the per-track map used by the progress ring. */
  function applyToByTrack(state: DownloadState) {
    if (state.state === 'done' || state.state === 'canceled') {
      removeFromByTrack(state.trackId)
      return
    }
    byTrack.value = { ...byTrack.value, [state.trackId]: state }
  }

  function removeFromByTrack(trackId: string) {
    if (!(trackId in byTrack.value)) return
    const rest = { ...byTrack.value }
    delete rest[trackId]
    byTrack.value = rest
  }

  /** Adds or replaces one panel row, keeping a title that is already resolved. */
  function applyToJobs(next: DownloadJobView) {
    const index = jobs.value.findIndex(job => job.jobId === next.jobId)
    if (index === -1) {
      jobs.value = [...jobs.value, next]
      return
    }
    const existing = jobs.value[index]
    const merged: DownloadJobView = {
      ...next,
      title: next.title || existing.title,
      artist: next.artist || existing.artist,
    }
    jobs.value = jobs.value.map((job, i) => (i === index ? merged : job))
  }

  function remove(raw: RawDownloadEvent) {
    const { jobId, trackId } = toState(raw)
    _liveJobIds.add(jobId)
    jobs.value = jobs.value.filter(job => job.jobId !== jobId)
    removeFromByTrack(trackId)
  }

  function upsert(raw: RawDownloadEvent): DownloadState {
    const state = toState(raw)
    _liveJobIds.add(state.jobId)
    applyToByTrack(state)
    if (state.state !== 'done') {
      // An event carries no quality/attempts, so it must not wipe the fields a
      // hydrate (or a previous snapshot) already resolved for this job.
      const existing = jobs.value.find(job => job.jobId === state.jobId)
      const view: DownloadJobView = {
        ...state,
        quality: existing?.quality ?? '',
        attempts: existing?.attempts ?? 0,
        maxAttempts: existing?.maxAttempts ?? 0,
        title: '',
        artist: '',
      }
      applyToJobs(view)
      void loadTitles([state.trackId])
    }
    return state
  }

  /** The download for a track, or undefined when it is not downloading. */
  function forTrack(trackId: string): DownloadState | undefined {
    return byTrack.value[trackId]
  }

  /** True while a track has a queued or active download. */
  function isDownloading(trackId: string): boolean {
    const state = byTrack.value[trackId]
    return state !== undefined && (state.state === 'queued' || state.state === 'active')
  }

  /** True when a failed job should offer a retry. */
  function hasFailed(trackId: string): boolean {
    return byTrack.value[trackId]?.state === 'failed'
  }

  /** How many jobs are queued or running (sidebar badge). */
  const activeCount = computed(
    () => Object.values(byTrack.value).filter(s => s.state === 'queued' || s.state === 'active').length,
  )

  /**
   * The panel list: `done` rows never appear (the track is already in the
   * library), and a failed/canceled row is hidden once a newer queued or active
   * row exists for the same track — retrying creates a fresh row and leaves the
   * old one behind as the record of what happened.
   */
  const pendingJobs = computed<DownloadJobView[]>(() => {
    const rows = jobs.value.filter(job => job.state !== 'done')
    if (rows.length === 0) return EMPTY_JOBS
    const busyTracks = new Set(
      rows.filter(job => job.state === 'queued' || job.state === 'active').map(job => job.trackId),
    )
    return rows.filter(
      job => !(busyTracks.has(job.trackId) && (job.state === 'failed' || job.state === 'canceled')),
    )
  })

  /**
   * Resolves track titles in one batched request. A track can be gone from the
   * library (deleted while its job was queued), in which case the row keeps its
   * fallback text instead of breaking the panel.
   *
   * A track that was not found is not requested again before the next hydrate:
   * progress fires per second and each one asks for a title.
   */
  async function loadTitles(trackIds: string[]) {
    const wanted = [...new Set(trackIds)].filter(
      id => id
        && !_titleInFlight.has(id)
        && !_missingTracks.has(id)
        && !jobs.value.some(job => job.trackId === id && job.title),
    )
    if (wanted.length === 0) return
    wanted.forEach(id => _titleInFlight.add(id))

    try {
      const tracks = await LibraryService.GetTracksByIDs(wanted)
      const resolved = new Map<string, { title: string; artist: string }>()
      for (const track of tracks ?? []) {
        if (track) resolved.set(track.id, { title: track.title ?? '', artist: track.raw_artist_names ?? '' })
      }
      wanted.filter(id => !resolved.has(id)).forEach(id => _missingTracks.add(id))
      if (resolved.size > 0) {
        jobs.value = jobs.value.map((job) => {
          const track = resolved.get(job.trackId)
          return track ? { ...job, title: track.title, artist: track.artist } : job
        })
      }
    } catch (error) {
      // No caching on failure: a later hydrate should retry the lookup.
      console.error('Failed to load track titles for the download queue:', error)
    } finally {
      wanted.forEach(id => _titleInFlight.delete(id))
    }
  }

  /**
   * Loads the queue from the backend. Called once at startup so jobs left
   * unfinished by a previous run are visible again, and again when the panel
   * opens to pick up anything that changed while it was closed.
   */
  async function hydrate(): Promise<void> {
    if (_hydrating) return _hydrating
    _hydrating = (async () => {
      try {
        const listed = await DownloadService.ListDownloads()
        const rows = (listed ?? []).filter((job): job is DownloadJob => job !== null).map(fromJob)
        // Rows the backend no longer reports are gone from the queue, unless a
        // live event touched them while the snapshot was in flight.
        const listedIds = new Set(rows.map(row => row.jobId))
        jobs.value = jobs.value.filter(
          job => listedIds.has(job.jobId) || _liveJobIds.has(job.jobId),
        )

        for (const row of rows) {
          if (_liveJobIds.has(row.jobId)) continue
          if (row.state === 'done') {
            jobs.value = jobs.value.filter(job => job.jobId !== row.jobId)
            removeFromByTrack(row.trackId)
            continue
          }
          applyToJobs(row)
          applyToByTrack(row)
        }

        await loadTitles(rows.map(row => row.trackId))
      } catch (error) {
        console.error('Failed to load the download queue:', error)
      } finally {
        // A cache miss is only final until the next snapshot: the track may
        // have been imported since, so hydrate gives it another chance.
        _missingTracks.clear()
        _liveJobIds = new Set()
        _hydrating = null
      }
    })()
    return _hydrating
  }

  /** Stops a queued or active job. Its track row stops showing progress. */
  async function cancel(jobId: string) {
    await DownloadService.CancelDownload(jobId)
    const row = jobs.value.find(job => job.jobId === jobId)
    const trackId = row?.trackId
      ?? Object.values(byTrack.value).find(state => state.jobId === jobId)?.trackId
      ?? ''
    jobs.value = jobs.value.map(job =>
      job.jobId === jobId ? { ...job, state: 'canceled', error: '' } : job,
    )
    if (trackId) removeFromByTrack(trackId)
  }

  /**
   * Re-runs a failed or canceled job. The backend enqueues a fresh row for the
   * same track and keeps the old one as the record, so the list is reloaded.
   */
  async function retry(jobId: string) {
    await DownloadService.RetryDownload(jobId)
    await hydrate()
  }

  function dispose() {
    _offProgress()
    _offDone()
    _offFailed()
  }

  return {
    byTrack,
    jobs,
    pendingJobs,
    activeCount,
    forTrack,
    isDownloading,
    hasFailed,
    hydrate,
    cancel,
    retry,
    dispose,
  }
})
